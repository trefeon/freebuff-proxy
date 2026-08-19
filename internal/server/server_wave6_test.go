package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/updatecheck"
	"freebuff-proxy/internal/upstream"
)

// --- Open Dashboard Auth Optional -------------------------------------------

// TestDashboardAuthOptional verifies the dashboard is clean and accessible when ADMIN_TOKEN is unset.
func TestDashboardAuthOptional(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	srv := newServer(t, mock, nil)
	for _, host := range []string{"192.168.1.50:3457", "127.0.0.1:3457", "localhost:3457"} {
		req := httptest.NewRequest(http.MethodGet, "/admin", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("host %s: status = %d, want 200", host, rec.Code)
		}
	}
}

// TestDashboardBannerHiddenWithAdminToken verifies the banner never shows
// when ADMIN_TOKEN is set.
func TestDashboardBannerHiddenWithAdminToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	srv := newServerCfg(t, mock, func(c *config.Config) { c.AdminToken = "secret" })
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Host = "192.168.1.50:3457"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	// Without the cookie the request redirects to login (still no banner on
	// the login page).
	if strings.Contains(rec.Body.String(), "Dashboard is open") {
		t.Error("banner shown with ADMIN_TOKEN set")
	}
}

// --- #45: playground ---------------------------------------------------------

func TestPlaygroundPageRenders(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	srv := newServer(t, mock, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/playground", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	page := rec.Body.String()
	if !strings.Contains(page, "freebuff-proxy") && !strings.Contains(page, "admin") {
		t.Error("playground page missing SPA content")
	}
}

// TestPlaygroundChatStreams verifies the playground chat endpoint streams
// the real chat pipeline (SSE body) without an API key.
func TestPlaygroundChatStreams(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	srv := newServer(t, mock, nil)
	body := `{"model":"z-ai/glm-5.2","prompt":"ping","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/admin/playground/chat", strings.NewReader(body))
	req.Host = "127.0.0.1:3457"
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "data:") {
		t.Error("no SSE data in playground response")
	}
}

// --- #62: login wizard endpoints ---------------------------------------------

func TestLoginWizardFlow(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Wire a real auth client through the option so the wizard works.
	srv := newServerCfg(t, mock, nil, func(s *Server) {
		auth, err := upstream.NewForAuth(&config.Config{UpstreamBaseURL: mock.URL()})
		if err != nil {
			t.Fatal(err)
		}
		s.authClient = auth
	})

	// Start.
	req := httptest.NewRequest(http.MethodPost, "/admin/login/start", nil)
	req.Host = "127.0.0.1:3457"
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", rec.Code, rec.Body.String())
	}
	var startResp struct {
		FlowID   string `json:"flow_id"`
		LoginURL string `json:"login_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &startResp); err != nil {
		t.Fatal(err)
	}
	if startResp.FlowID == "" || startResp.LoginURL == "" {
		t.Fatalf("start response missing flow_id/login_url: %s", rec.Body.String())
	}

	// Poll: pending (mock serves 401 until AuthCLIStatusBody is set).
	poll := func() string {
		req := httptest.NewRequest(http.MethodGet, "/admin/login/status?fingerprint=enhanced-test", nil)
		req.Host = "127.0.0.1:3457"
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status code = %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Status
	}
	if got := poll(); got != "pending" {
		t.Errorf("first poll = %q, want pending", got)
	}

	// Complete the login upstream; the next poll must add the token.
	mock.AuthCLIStatusBody = `{"authToken":"cb_wizard","user":{"id":"gh-9","name":"Wiz","email":"wiz@example.com"}}`
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := poll(); got == "completed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if srv.pool.TokenCount() != 2 {
		t.Errorf("pool tokens = %d, want 2 (1 fixed + wizard token)", srv.pool.TokenCount())
	}
}

// --- #100: queue-time model fallback -----------------------------------------

// TestChatFallbackAfterWaitingRoom verifies the acquire-time fallback:
// with FALLBACK_AFTER_MS + FALLBACK_MODEL configured and the waiting room
// RetryAfter >= the threshold, the request is re-routed to the fallback
// model and the X-FreeBuff-Fallback-Model header surfaces the switch.
func TestChatFallbackAfterWaitingRoom(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "queued"
	mock.SessionSequence = []string{"queued", "active"} // first create queues; the fallback model's create succeeds
	mock.EstimatedWaitMs = 20000                        // > FALLBACK_AFTER_MS (10s)
	srv := newServerCfg(t, mock, func(c *config.Config) {
		c.FallbackAfter = 10 * time.Second
		c.FallbackModels = map[string]string{"z-ai/glm-5.2": "deepseek/deepseek-v4-flash"}
	})
	body := `{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fallback served): %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-FreeBuff-Fallback-Model"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("X-FreeBuff-Fallback-Model = %q, want deepseek/deepseek-v4-flash", got)
	}
}

// TestChatNoFallbackBelowThreshold verifies a short waiting room (below
// FALLBACK_AFTER_MS) surfaces 503 waiting_room_queued as usual.
func TestChatNoFallbackBelowThreshold(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "queued"
	mock.EstimatedWaitMs = 1000 // < FALLBACK_AFTER_MS
	srv := newServerCfg(t, mock, func(c *config.Config) {
		c.FallbackAfter = 10 * time.Second
		c.FallbackModels = map[string]string{"z-ai/glm-5.2": "deepseek/deepseek-v4-flash"}
	})
	body := `{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (wait below threshold)", rec.Code)
	}
	if got := rec.Header().Get("X-FreeBuff-Fallback-Model"); got != "" {
		t.Errorf("fallback header set without fallback: %q", got)
	}
}

// --- #50b: update badge ------------------------------------------------------

func TestUpdateBadgeRendered(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var hits atomic.Int64
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer gh.Close()
	tr := &rewriteTransport{target: gh.URL}
	checker := updatecheck.New(updatecheck.DefaultRepo, &http.Client{Transport: tr})
	srv := newServerCfg(t, mock, nil, func(s *Server) {
		s.version = "v0.9.3"
		s.updates = checker
	})
	req := httptest.NewRequest(http.MethodGet, "/admin/api/version", nil)
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	t.Logf("hits=%d body=%s", hits.Load(), body)
	if !strings.Contains(body, `"has_update":true`) {
		t.Errorf("version api missing has_update:true in: %s", body)
	}
	if !strings.Contains(body, `"latest_version":"v9.9.9"`) {
		t.Errorf("version api missing latest_version v9.9.9 in: %s", body)
	}
	if hits.Load() == 0 {
		t.Error("update checker never queried")
	}
}

// rewriteTransport sends every request to target (tests must not contact
// api.github.com).
type rewriteTransport struct {
	target string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(t.target, "http://")
	return http.DefaultTransport.RoundTrip(clone)
}

// newServer builds a test server over one mock token with a config mutation
// and optional server options; returns the raw *Server for pool/option
// assertions.
func newServer(t *testing.T, mock *testutil.MockUpstream, mut func(*config.Config)) *Server {
	t.Helper()
	return newServerOpts(t, mock, mut)
}

func newServerCfg(t *testing.T, mock *testutil.MockUpstream, mut func(*config.Config), opts ...func(*Server)) *Server {
	t.Helper()
	return newServerOpts(t, mock, mut, opts...)
}

func newServerOpts(t *testing.T, mock *testutil.MockUpstream, mut func(*config.Config), opts ...func(*Server)) *Server {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
		LogAccess:          true,
	}
	if mut != nil {
		mut(cfg)
	}
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
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
	serverOpts := make([]Option, 0, len(opts))
	for _, o := range opts {
		serverOpts = append(serverOpts, o)
	}
	srv := New(cfg, p, reg, nil, nil, "", serverOpts...)
	return srv
}

// --- PREFER_MAX_MODELS limited-tier gating ---------------------------------

// TestChatLearnsAccessTier verifies the session admission's accessTier is
// folded into the runtime config (server + registry copies) so ResolveModel
// gates -max upgrades for limited tokens on the NEXT request. A chat that
// reports upstream tier "limited" flips the registry's resolution: the same
// model that upgraded before the fold stays on its base afterwards.
func TestChatLearnsAccessTier(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.AccessTier = "limited"
	mock.CountryCode = "US"
	mock.ChatBody = testutil.SSEEvent(testChunk("chatcmpl-tier1", 1, `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`))
	srv := newServerCfg(t, mock, func(c *config.Config) { c.PreferMaxModels = true })

	// Before the fold the tier is unknown, so a routed -max variant
	// upgrades (the registry copy has not seen the tier yet).
	if got := srv.reg.ResolveModel("deepseek/deepseek-v4-pro"); got != "deepseek/deepseek-v4-pro-max" {
		t.Fatalf("pre-fold ResolveModel(deepseek-v4-pro) = %q, want -max (unknown tier upgrades)", got)
	}

	body := `{"model":"` + testModelA + `","prompt":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	cur := srv.cfg.Load()
	if cur.AccessTier != "limited" {
		t.Errorf("runtime AccessTier = %q, want limited (folded from admission)", cur.AccessTier)
	}
	if cur.AccessTierExplicit {
		t.Error("AccessTierExplicit = true after probe fold, want false (runtime-learned)")
	}
	// The registry copy received the fold: the same model that upgraded
	// before the fold now keeps its base model for a limited tier.
	if got := srv.reg.ResolveModel("deepseek/deepseek-v4-pro"); got != "deepseek/deepseek-v4-pro" {
		t.Errorf("post-fold ResolveModel(deepseek-v4-pro) = %q, want base (limited tier gates -max)", got)
	}
}

// TestChatLearnsProvisionedModels is the regression for issue #140's ban
// amplifier: a FULL-tier token whose upstream rate limits provision ONLY
// base models (no -max entries) must not upgrade to -max — upstream
// provisions -max roots per-account, so PREFER_MAX_MODELS would otherwise
// fire 403 free_mode_invalid_agent_model on every request (the 403 storm
// that demoted and banned accounts). The admission's QuotaByModel keys are
// folded as the provisioned set and gate the next ResolveModel call.
func TestChatLearnsProvisionedModels(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.AccessTier = "full" // full tier — but NO -max in the quota map
	mock.RateLimitsByModel = map[string]any{
		"deepseek/deepseek-v4-pro": map[string]any{
			"model": "deepseek/deepseek-v4-pro", "limit": 5, "recentCount": 0,
			"period": "pacific_day", "resetTimeZone": "America/Los_Angeles",
			"resetAt": "2026-08-20T07:00:00.000Z",
		},
		"deepseek/deepseek-v4-flash": map[string]any{
			"model": "deepseek/deepseek-v4-flash", "limit": 999, "recentCount": 0,
			"period": "pacific_day", "resetTimeZone": "America/Los_Angeles",
			"resetAt": "2026-08-20T07:00:00.000Z",
		},
	}
	mock.ChatBody = testutil.SSEEvent(testChunk("chatcmpl-provm", 1, `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`))
	srv := newServerCfg(t, mock, func(c *config.Config) { c.PreferMaxModels = true })

	// Before any admission the provisioned set is unknown, so the routed
	// -max variant upgrades (historic behavior).
	if got := srv.reg.ResolveModel("deepseek/deepseek-v4-pro"); got != "deepseek/deepseek-v4-pro-max" {
		t.Fatalf("pre-admission ResolveModel(deepseek-v4-pro) = %q, want -max (unknown provisioned set)", got)
	}

	body := `{"model":"` + testModelA + `","prompt":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	cur := srv.cfg.Load()
	// The mock's quota covers the BASE models only — full tier plus a
	// base-only provisioned set is exactly the incident shape.
	if len(cur.ProvisionedModels) == 0 {
		t.Fatal("ProvisionedModels not learned after admission")
	}
	if cur.ProvisionedModels["deepseek/deepseek-v4-flash-max"] {
		t.Error("flash-max in provisioned set, want absent (base-only account)")
	}
	// The registry copy received the fold: the SAME model that upgraded
	// before the admission now stays on its base — full tier alone no
	// longer justifies the -max upgrade when the account was not
	// provisioned for it.
	if got := srv.reg.ResolveModel("deepseek/deepseek-v4-pro"); got != "deepseek/deepseek-v4-pro" {
		t.Errorf("post-admission ResolveModel(deepseek-v4-pro) = %q, want base (provisioned set gates -max)", got)
	}
	if got := srv.reg.ResolveModel("deepseek/deepseek-v4-flash"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("post-admission ResolveModel(deepseek-v4-flash) = %q, want base (flash-max not provisioned)", got)
	}
}

// TestChatAccessTierExplicitWins verifies an operator-set ACCESS_TIER is
// never clobbered by a probe observation: the fold skips when
// AccessTierExplicit is set.
func TestChatAccessTierExplicitWins(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.AccessTier = "full" // the probe would say full...
	mock.ChatBody = testutil.SSEEvent(testChunk("chatcmpl-tier2", 1, `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`))
	// ...but the operator pinned limited for the deployment.
	srv := newServerCfg(t, mock, func(c *config.Config) { c.AccessTier = "limited"; c.AccessTierExplicit = true })

	body := `{"model":"` + testModelA + `","prompt":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := srv.cfg.Load().AccessTier; got != "limited" {
		t.Errorf("runtime AccessTier = %q, want limited (explicit config wins over probe)", got)
	}
}

// TestChatAccessTierEmptyIgnored verifies an admission that reports no tier
// leaves the runtime config untouched (unknown tier keeps the current value).
func TestChatAccessTierEmptyIgnored(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(testChunk("chatcmpl-tier3", 1, `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`))
	srv := newServer(t, mock, nil)

	body := `{"model":"` + testModelA + `","prompt":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := srv.cfg.Load().AccessTier; got != "" {
		t.Errorf("runtime AccessTier = %q, want empty (no tier reported, nothing folded)", got)
	}
}
