package dashboard_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/dashboard"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// liveTestServer mounts the overview + tokens JSON handlers over one pooled
// token so ?view=live can be compared against the full shape.
func liveTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         []string{"tok-live-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{session.NewManager(client)}, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
	mux := http.NewServeMux()
	mux.Handle("GET /admin/api/overview", d.APIHandler("overview"))
	mux.Handle("GET /admin/api/tokens", d.APIHandler("tokens"))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec,noctx // hermetic httptest server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestLiveViewOverviewOmitsStatic pins issue #322: the 15s hot-poll shape
// carries live numbers only, while the default shape keeps every field.
func TestLiveViewOverviewOmitsStatic(t *testing.T) {
	ts := liveTestServer(t)
	full := getJSON(t, ts.URL+"/admin/api/overview")
	live := getJSON(t, ts.URL+"/admin/api/overview?view=live")

	// Full shape keeps the restart/deploy-only fields.
	for _, k := range []string{"mode", "model_count", "safe_mode", "transient_retries", "max_messages_per_day", "upstream_sync", "base_url"} {
		if _, ok := full[k]; !ok {
			t.Errorf("full overview missing %q", k)
		}
	}
	// Live shape omits them but keeps the live numbers.
	for _, k := range []string{"mode", "model_count", "safe_mode", "transient_retries", "max_messages_per_day", "upstream_sync", "base_url", "models"} {
		if _, ok := live[k]; ok {
			t.Errorf("live overview carries static %q", k)
		}
	}
	for _, k := range []string{"uptime", "tokens", "has_tokens", "bridge_tokens"} {
		if _, ok := live[k]; !ok {
			t.Errorf("live overview missing live %q", k)
		}
	}
	if up, _ := live["uptime"].(string); up == "" {
		t.Errorf("live overview uptime empty, want human duration since start")
	}
	if up, _ := full["uptime"].(string); up == "" {
		t.Errorf("full overview uptime empty, want human duration since start")
	}
	// Per-token live cards omit account-stable fields.
	toks, _ := live["tokens"].([]any)
	if len(toks) != 1 {
		t.Fatalf("live overview tokens len = %d, want 1", len(toks))
	}
	card, _ := toks[0].(map[string]any)
	for _, k := range []string{"email", "account_id", "daily_limit", "standing_level", "has_standing", "referral_code", "has_referral"} {
		if _, ok := card[k]; ok {
			t.Errorf("live token card carries static %q", k)
		}
	}
	for _, k := range []string{"index", "session_status", "active_runs", "requests", "risk_level"} {
		if _, ok := card[k]; !ok {
			t.Errorf("live token card missing live %q", k)
		}
	}
	// Full card keeps the stable fields (zero-valued here, but present keys
	// prove the default shape is unchanged for old clients).
	fullToks, _ := full["tokens"].([]any)
	if len(fullToks) != 1 {
		t.Fatalf("full overview tokens len = %d, want 1", len(fullToks))
	}
	fullCard, _ := fullToks[0].(map[string]any)
	for _, k := range []string{"index", "session_status", "daily_limit", "has_standing", "has_referral"} {
		if _, ok := fullCard[k]; !ok {
			t.Errorf("full token card missing %q", k)
		}
	}
}

// TestLiveViewTokensOmitsStatic pins issue #322 for the 10s tokens poll.
func TestLiveViewTokensOmitsStatic(t *testing.T) {
	ts := liveTestServer(t)
	full := getJSON(t, ts.URL+"/admin/api/tokens")
	live := getJSON(t, ts.URL+"/admin/api/tokens?view=live")

	if _, ok := full["mode"]; !ok {
		t.Errorf("full tokens missing mode")
	}
	for _, k := range []string{"mode", "in_bridge", "show_bridge"} {
		if _, ok := live[k]; ok {
			t.Errorf("live tokens carries static %q", k)
		}
	}
	for _, k := range []string{"tokens", "has_tokens", "token_count", "bridge_tokens"} {
		if _, ok := live[k]; !ok {
			t.Errorf("live tokens missing live %q", k)
		}
	}
	toks, _ := live["tokens"].([]any)
	if len(toks) != 1 {
		t.Fatalf("live tokens len = %d, want 1", len(toks))
	}
	card, _ := toks[0].(map[string]any)
	for _, k := range []string{"email", "account_id", "daily_limit", "has_standing", "standing_level", "has_referral", "referral_code"} {
		if _, ok := card[k]; ok {
			t.Errorf("live token detail carries static %q", k)
		}
	}
	// Session + quota rows stay live.
	for _, k := range []string{"session_instance", "session_model", "quota", "has_quota"} {
		if _, ok := card[k]; !ok {
			t.Errorf("live token detail missing live %q", k)
		}
	}
}

// TestLiveViewCarriesSessionCountdown pins the #322 live-poll contract for
// the session timers: after a real admission, the live shape must carry the
// same nonzero session_remaining_seconds / session_expires_at as the full
// shape, or merged tokens lose their countdowns after the first poll.
func TestLiveViewCarriesSessionCountdown(t *testing.T) {
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	mock.ChatBody = testutil.SSEEvent(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"deepseek/deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`) +
		testutil.SSEEvent(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"deepseek/deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		ListenAddr:         "127.0.0.1:3457",
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
	}
	client, err := upstream.New("tok-0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	sess := session.NewManager(client)
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{sess}, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)

	// Real admission so the session carries a live expiry countdown.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := p.Acquire(ctx, "deepseek/deepseek-v4-flash")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	up, err := p.Chat(ctx, lease, upstream.ChatOptions{Model: "deepseek/deepseek-v4-flash", RunID: lease.Run.RunID, SessionInstanceID: lease.SessionInstanceID},
		[]byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_, _ = io.Copy(io.Discard, up)
	_ = up.Close()
	p.LeaseRelease(lease)

	mux := http.NewServeMux()
	mux.Handle("GET /admin/api/tokens", d.APIHandler("tokens"))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	full := getJSON(t, ts.URL+"/admin/api/tokens")
	live := getJSON(t, ts.URL+"/admin/api/tokens?view=live")
	fullToks, _ := full["tokens"].([]any)
	liveToks, _ := live["tokens"].([]any)
	if len(fullToks) != 1 || len(liveToks) != 1 {
		t.Fatalf("tokens len full=%d live=%d, want 1/1", len(fullToks), len(liveToks))
	}
	fullCard, _ := fullToks[0].(map[string]any)
	liveCard, _ := liveToks[0].(map[string]any)
	fullRem, _ := fullCard["session_remaining_seconds"].(float64)
	liveRem, liveHasRem := liveCard["session_remaining_seconds"].(float64)
	if fullRem <= 0 {
		t.Fatalf("full session_remaining_seconds = %v, want >0 (mock expires in 30m)", fullRem)
	}
	if !liveHasRem {
		t.Fatalf("live token missing session_remaining_seconds (full=%v)", fullRem)
	}
	if liveRem != fullRem {
		t.Errorf("live session_remaining_seconds = %v, want full %v", liveRem, fullRem)
	}
	fullExp, _ := fullCard["session_expires_at"].(string)
	liveExp, _ := liveCard["session_expires_at"].(string)
	if fullExp == "" {
		t.Fatalf("full session_expires_at empty, want live expiry")
	}
	if liveExp != fullExp {
		t.Errorf("live session_expires_at = %q, want full %q", liveExp, fullExp)
	}
}
