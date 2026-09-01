package server

// P2-3 regression tests (2026-08-31 review): one request's pooled-vs-bridge
// routing must derive from ONE config snapshot. requireAuth (the outermost
// /v1 auth wrapper) pins its config load into the request context; chatCore
// and authorized must route from that pinned snapshot — never from a fresh
// load that a concurrent /admin/reload may have swapped in between the
// middleware's pass-through and the core's routing decision.
//
// These tests run inside package server because they drive chatCore,
// authorized, and the snapshot context helpers directly and observe the
// upstream Authorization header the routing decision produced.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

const (
	reviewFixModel      = "deepseek/deepseek-v4-flash"
	reviewFixAPIKey     = "sk-reviewfix"
	reviewFixRotatedKey = "sk-rotated"
	reviewFixBridgeCred = "client-cred-reviewfix"
)

// newReviewFixCore wires the real chatCore stack the way the external
// server_test helpers do — one mock upstream, one pool token (tok-0),
// hybrid config (AUTH_TOKENS pool + BRIDGE_ENABLED, API_KEYS=[reviewFixAPIKey])
// — but inside package server so the tests can reach chatCore, authorized,
// and the snapshot helpers directly.
func newReviewFixCore(t *testing.T) (*Server, *testutil.MockUpstream, *config.Config) {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		APIKeys:            []string{reviewFixAPIKey},
		BridgeEnabled:      true,
		AdminToken:         config.DefaultAdminToken,
	}
	client, err := upstream.New("tok-0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg,
		[]*upstream.Client{client},
		[]*session.Manager{session.NewManager(client)},
		reg)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, p, reg, nil, nil, ""), mock, cfg
}

func reviewFixChatBody() []byte {
	return []byte(`{"model":"` + reviewFixModel + `","messages":[{"role":"user","content":"ping"}],"stream":true}`)
}

// reviewFixDrainRelay is a minimal relayFunc: chatCore hands it the upstream
// stream after the routing decision; these tests only care about WHERE the
// upstream call went, not the relayed bytes.
func reviewFixDrainRelay(ctx context.Context, w http.ResponseWriter, up io.Reader, stats *relayStats, chatStart time.Time) {
	_, _ = io.Copy(io.Discard, up)
}

// requireAuthThenSwap simulates the P2-3 tear deterministically: the request
// flows through requireAuth (which loads and pins the live config), and only
// THEN the operator swaps the server's live config before the request
// resumes into chatCore. Returns the stamped request plus the pinned
// snapshot requireAuth saw.
func requireAuthThenSwap(t *testing.T, s *Server, authHeader string, swap func(*config.Config)) (*http.Request, *config.Config) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	var stamped *http.Request
	next := func(w http.ResponseWriter, req *http.Request) { stamped = req }
	s.requireAuth(next)(httptest.NewRecorder(), r)
	if stamped == nil {
		t.Fatal("requireAuth did not pass the request through")
	}
	pinned := cfgSnapshotFrom(stamped.Context())
	if pinned == nil {
		t.Fatal("requireAuth left no config snapshot in the request context")
	}
	swap(pinned)
	return stamped, pinned
}

// TestChatCoreSnapshotAPIKeyRemovalTear: requireAuth admitted the request
// under cfg1 (hybrid, API_KEYS=[sk-reviewfix]); the operator then swaps the
// live config to cfg2 where that key was rotated away. chatCore must route
// from the pinned snapshot — pooled, the upstream sees the pool token —
// never relay the now-unknown key upstream as a bridge token (the pre-fix
// behavior: a fresh chatCore load saw cfg2 and bridged).
func TestChatCoreSnapshotAPIKeyRemovalTear(t *testing.T) {
	s, mock, _ := newReviewFixCore(t)
	mock.ChatBody = testutil.SSEEvent(`{"id":"chatcmpl-rf1","object":"chat.completion.chunk","created":1,"model":"` + reviewFixModel + `","choices":[{"index":0,"delta":{"content":"pooled"},"finish_reason":null}]}`)

	stamped, _ := requireAuthThenSwap(t, s, "Bearer "+reviewFixAPIKey, func(pinned *config.Config) {
		cfg2 := *pinned
		cfg2.APIKeys = []string{reviewFixRotatedKey}
		s.cfg.Store(&cfg2)
	})

	w := httptest.NewRecorder()
	s.chatCore(w, stamped, reviewFixModel, true, reviewFixChatBody(), "", "chat completions", reviewFixDrainRelay)

	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1 (recorder body: %s)", len(mock.RecordedChatHeaders), w.Body.String())
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer tok-0" {
		t.Errorf("upstream Authorization = %q, want %q — the removed API key must never be relayed upstream as a bridge token", got, "Bearer tok-0")
	}
}

// TestChatCoreSnapshotBridgeFlipTear: requireAuth passed an unknown
// credential through under cfg1 (hybrid); the live config then flips
// BRIDGE_ENABLED off (pure pooled lockdown). chatCore must still treat the
// credential as a bridge token per the pinned snapshot — pre-fix it
// consulted the swapped config and served the unauthenticated request from
// the pool.
func TestChatCoreSnapshotBridgeFlipTear(t *testing.T) {
	s, mock, _ := newReviewFixCore(t)
	mock.ChatBody = testutil.SSEEvent(`{"id":"chatcmpl-rf2","object":"chat.completion.chunk","created":2,"model":"` + reviewFixModel + `","choices":[{"index":0,"delta":{"content":"bridged"},"finish_reason":null}]}`)

	stamped, _ := requireAuthThenSwap(t, s, "Bearer "+reviewFixBridgeCred, func(pinned *config.Config) {
		cfg2 := *pinned
		cfg2.BridgeEnabled = false
		s.cfg.Store(&cfg2)
	})

	w := httptest.NewRecorder()
	s.chatCore(w, stamped, reviewFixModel, true, reviewFixChatBody(), "", "chat completions", reviewFixDrainRelay)

	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1 (recorder body: %s)", len(mock.RecordedChatHeaders), w.Body.String())
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer "+reviewFixBridgeCred {
		t.Errorf("upstream Authorization = %q, want %q — the client credential must be bridged per the snapshot requireAuth admitted it under, not served from the pool", got, "Bearer "+reviewFixBridgeCred)
	}
}

// TestRequireAuthStampsConfigSnapshot: requireAuth pins the config snapshot
// it made its own decision with into the request context, freshly per
// request — the handler (and chatCore below it) sees exactly the config the
// middleware admitted the request under, even after the live config was
// swapped by an earlier request.
func TestRequireAuthStampsConfigSnapshot(t *testing.T) {
	s, _, cfg1 := newReviewFixCore(t)

	saw := func(r *http.Request) *config.Config {
		var got *config.Config
		s.requireAuth(func(w http.ResponseWriter, req *http.Request) {
			got = cfgSnapshotFrom(req.Context())
		})(httptest.NewRecorder(), r)
		return got
	}

	if got := saw(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)); got != cfg1 {
		t.Errorf("stamped snapshot = %p, want the config requireAuth loaded (%p)", got, cfg1)
	}

	cfg2 := *cfg1
	cfg2.APIKeys = []string{reviewFixRotatedKey}
	s.cfg.Store(&cfg2)
	if got := saw(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)); got != &cfg2 {
		t.Errorf("stamped snapshot after swap = %p, want the new live config (%p) — the pin must be fresh per request", got, &cfg2)
	}

	if cfgSnapshotFrom(context.Background()) != nil {
		t.Error("cfgSnapshotFrom on a bare context = non-nil, want nil (direct handler calls must fall back to their own load)")
	}
}

// TestAuthorizedUsesPassedSnapshot: authorized checks the credential against
// the SNAPSHOT it is given, not a fresh load — the property that lets
// requireAuth and chatCore keep one consistent decision per request.
func TestAuthorizedUsesPassedSnapshot(t *testing.T) {
	s, _, cfg1 := newReviewFixCore(t)
	cfgWithoutKey := &config.Config{APIKeys: []string{"sk-other"}}
	for _, tc := range []struct{ name, header, value string }{
		{"bearer", "Authorization", "Bearer " + reviewFixAPIKey},
		{"x-api-key", "x-api-key", reviewFixAPIKey},
		{"anthropic-api-key", "anthropic-api-key", reviewFixAPIKey},
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set(tc.header, tc.value)
		if !s.authorized(cfg1, r) {
			t.Errorf("%s: authorized under the config carrying the key = false, want true", tc.name)
		}
		if s.authorized(cfgWithoutKey, r) {
			t.Errorf("%s: authorized under a config without the key = true, want false", tc.name)
		}
	}
}
