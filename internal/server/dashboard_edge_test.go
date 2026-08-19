package server_test

// Dashboard HTTP edge tests from the AuditServer P1/P2 gap list: token
// test-all / add / remove / action-id edges, the mode-switch branch matrix,
// config-save guards (incl. the empty-content regression), reload failure,
// smoke/diag edges, CSRF combos, cookie edges, assets, and login-without-token.
// Split from dashboard_test.go because that file is already ~1000 lines.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/server"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// bridgeDashboardServer wires a bridge-mode server (no AUTH_TOKENS) with the
// given admin token, so dashboard handlers that need an empty pool can run.
func bridgeDashboardServer(t *testing.T, adminToken string) *httptest.Server {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	cfg := &config.Config{
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		AdminToken:         adminToken,
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// --- token test-all ---

// TestDashboardTokenTestAllBridgeNoTokens: in bridge mode there are no fixed
// tokens to probe — the handler reports it instead of looping.
func TestDashboardTokenTestAllBridgeNoTokens(t *testing.T) {
	ts := bridgeDashboardServer(t, "secret")
	cookie := authedCookie(t, ts)
	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/test-all")
	body := bodyOf(t, resp)
	if !strings.Contains(body, "No tokens to test") {
		t.Errorf("test-all response = %q, want no-tokens message", body)
	}
}

// TestDashboardTokenTestAllTwoTokens: every pooled token gets a zero-cost
// validity probe and one appended result fragment.
func TestDashboardTokenTestAllTwoTokens(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, mock0, mock1)
	cookie := authedCookie(t, ts)

	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/test-all")
	body := bodyOf(t, resp)
	// JSON responses: each token gets {"token":0,...} {"token":1,...}
	if !strings.Contains(body, `"token":0`) || !strings.Contains(body, `"token":1`) {
		t.Errorf("test-all missing per-token results: %s", body)
	}
	if !strings.Contains(body, `"ok":true`) {
		t.Errorf("test-all missing ok:true: %s", body)
	}
}

// TestDashboardTokenTestAllEmptyRegistry: the zero-cost probe needs no
// registry models (the upstream GET carries no model), so test-all succeeds
// even with an empty catalog — the old registry-dependent guard is gone.
func TestDashboardTokenTestAllEmptyRegistry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		AdminToken:         "secret",
	}
	reg := registry.New(cfg, nil) // no LoadFallback → empty catalog
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []*session.Manager{session.NewManager(client)}
	p, err := pool.New(cfg, []*upstream.Client{client}, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cookie := authedCookie(t, ts)

	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/test-all")
	if body := bodyOf(t, resp); !strings.Contains(body, `"token":0`) && !strings.Contains(body, `"ok":true`) {
		t.Errorf("empty-registry test-all response = %q, want probe success row", body)
	}
}

// --- token add ---

// TestDashboardTokenAddRejections pins the handleTokenAdd validation gates:
// Bearer-prefixed, empty, whitespace-only, malformed JSON, and oversized
// bodies are all rejected before any pool mutation.
func TestDashboardTokenAddRejections(t *testing.T) {
	t.Chdir(t.TempDir())
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, testutil.NewMock())
	cookie := authedCookie(t, ts)
	start := p.TokenCount()

	cases := []struct {
		name   string
		path   string
		body   string
		hdr    map[string]string
		expect string
	}{
		// The rendered messages are HTML-escaped (html/template turns the
		// quotes into &#39;), so the expects use the escaped form.
		{"bearer prefix", "/admin/tokens/add", `{"token":"Bearer cb_x"}`, map[string]string{"Content-Type": "application/json"}, "Invalid token (must not start with 'Bearer ')"},
		{"empty", "/admin/tokens/add", `{"token":""}`, map[string]string{"Content-Type": "application/json"}, "Invalid token (must not start with 'Bearer ')"},
		{"whitespace", "/admin/tokens/add", `{"token":"   "}`, map[string]string{"Content-Type": "application/json"}, "Invalid token (must not start with 'Bearer ')"},
		{"malformed json", "/admin/tokens/add", `not json`, map[string]string{"Content-Type": "application/json"}, "Invalid request"},
		{"oversized", "/admin/tokens/add", `{"token":"` + strings.Repeat("a", 8<<10+1) + `"}`, map[string]string{"Content-Type": "application/json"}, "Failed to read request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Cookie", cookie)
			for k, v := range tc.hdr {
				req.Header.Set(k, v)
			}
			resp, err := noRedirectClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body := bodyOf(t, resp)
			if !strings.Contains(body, tc.expect) {
				t.Errorf("response = %q, want %q", body, tc.expect)
			}
		})
	}
	// None of the rejections may touch the pool.
	if got := p.TokenCount(); got != start {
		t.Errorf("pool TokenCount = %d after rejections, want %d", got, start)
	}
}

// TestDashboardTokenAddDuplicate pins the current duplicate behavior: the
// pool has no duplicate guard, so adding an identical token succeeds (the
// audit listed this as "duplicate-token pool error" — no such error exists
// today, so the success path is pinned here and the gap reported).
func TestDashboardTokenAddDuplicate(t *testing.T) {
	t.Chdir(t.TempDir())
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, testutil.NewMock())
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"cb_dup"}`)
	if body := bodyOf(t, resp); !strings.Contains(body, "Token added at index 1") {
		t.Fatalf("first add response = %q, want success", body)
	}
	resp = postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"cb_dup"}`)
	if body := bodyOf(t, resp); !strings.Contains(body, "Token added at index 2") {
		t.Fatalf("duplicate add response = %q, want success (no dup guard today)", body)
	}
	if got := p.TokenCount(); got != 3 {
		t.Errorf("pool TokenCount = %d, want 3 (original + two identical adds)", got)
	}
}

// TestDashboardTokenAddAfterDivergence is the regression for the P2 bug:
// after a config-editor AUTH_TOKENS edit diverges cfg from the pool, adding a
// token must be rejected (like remove) instead of persisting the stale
// cfg.AuthTokens+new list to .env.
func TestDashboardTokenAddAfterDivergence(t *testing.T) {
	t.Chdir(t.TempDir())
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, testutil.NewMock())
	cookie := authedCookie(t, ts)

	// Config editor rewrites AUTH_TOKENS to a two-token list; the pool still
	// holds its original single token.
	resp := postConfig(t, ts.URL, cookie, "AUTH_TOKENS=tok-0,extra-token\nSAFE_MODE=true\n")
	if body := bodyOf(t, resp); !strings.Contains(body, "Saved and reloaded") {
		t.Fatalf("config save failed: %s", body)
	}

	resp = postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"cb_after_divergence"}`)
	body := bodyOf(t, resp)
	if !strings.Contains(body, "differs from the live pool") {
		t.Errorf("add response = %q, want divergence rejection", body)
	}
	// The pool and .env must be untouched.
	if got := p.TokenCount(); got != 1 {
		t.Errorf("pool TokenCount = %d, want 1 (add rejected)", got)
	}
	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "cb_after_divergence") {
		t.Error("rejected add leaked into .env")
	}
	if !strings.Contains(string(env), "extra-token") {
		t.Error("diverged .env must be left untouched")
	}
}

// --- token remove ---

// TestDashboardTokenRemoveSuccess pins the plain success path: the last
// token leaves the pool, .env is rewritten without it, and a fresh config
// reload agrees.
func TestDashboardTokenRemoveSuccess(t *testing.T) {
	t.Chdir(t.TempDir())
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, testutil.NewMock())
	cookie := authedCookie(t, ts)

	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/remove")
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Last token removed") {
		t.Fatalf("remove response = %q, want success", body)
	}
	if got := p.TokenCount(); got != 0 {
		t.Errorf("pool TokenCount = %d, want 0", got)
	}
	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "tok-0") {
		t.Errorf("removed token still in .env: %s", env)
	}
	if !strings.Contains(string(env), "AUTH_TOKENS=") {
		t.Errorf(".env missing AUTH_TOKENS= after removal: %s", env)
	}
	reloaded, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.AuthTokens) != 0 {
		t.Errorf("reloaded AUTH_TOKENS = %d, want 0", len(reloaded.AuthTokens))
	}
}

// TestDashboardTokenRemoveEmptyPool pins the empty-pool remove: the pool
// itself rejects it with "no tokens to remove".
func TestDashboardTokenRemoveEmptyPool(t *testing.T) {
	ts := bridgeDashboardServer(t, "secret")
	cookie := authedCookie(t, ts)
	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/remove")
	body := bodyOf(t, resp)
	if !strings.Contains(body, "no tokens to remove") {
		t.Errorf("remove response = %q, want empty-pool error", body)
	}
}

// --- token action ids ---

// TestDashboardTokenActionInvalidID pins the {id} parsing: non-numeric and
// negative ids are rejected with "invalid token id" before any pool call.
func TestDashboardTokenActionInvalidID(t *testing.T) {
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, testutil.NewMock())
	cookie := authedCookie(t, ts)

	for _, path := range []string{
		"/admin/tokens/abc/unlock",
		"/admin/tokens/-1/unlock",
		"/admin/tokens/abc/finish",
		"/admin/tokens/-1/finish",
		"/admin/tokens/abc/test",
		"/admin/tokens/-1/test",
	} {
		resp := doTokenAction(t, ts.URL, cookie, path)
		body := bodyOf(t, resp)
		if !strings.Contains(body, "invalid token id") {
			t.Errorf("%s response = %q, want invalid token id", path, body)
		}
	}
}

// --- mode switch ---

// TestDashboardModeSwitchBranchMatrix drives the rejection branches of
// handleModeSwitch: invalid mode, already-in-mode, bridge→pooled without
// tokens, and a persist failure that must leave the pool and config intact.
func TestDashboardModeSwitchBranchMatrix(t *testing.T) {
	t.Chdir(t.TempDir())
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, testutil.NewMock())
	cookie := authedCookie(t, ts)

	// Invalid mode string (JSON response, not HTML-escaped).
	resp := postJSON(t, ts.URL, cookie, "/admin/mode", `{"mode":"warp"}`)
	if body := bodyOf(t, resp); !strings.Contains(body, "Mode must be 'bridge', 'pooled', or 'hybrid'.") {
		t.Errorf("invalid-mode response = %q", body)
	}

	// Already in pooled mode.
	resp = postJSON(t, ts.URL, cookie, "/admin/mode", `{"mode":"pooled"}`)
	if body := bodyOf(t, resp); !strings.Contains(body, "Already in pooled mode.") {
		t.Errorf("already-pooled response = %q", body)
	}

	// Pool untouched by the rejections.
	if got := p.TokenCount(); got != 1 {
		t.Errorf("pool TokenCount = %d, want 1", got)
	}

	// Bridge → pooled without tokens: needs a token first.
	tsBridge := bridgeDashboardServer(t, "secret")
	cookieBridge := authedCookie(t, tsBridge)
	resp = postJSON(t, tsBridge.URL, cookieBridge, "/admin/mode", `{"mode":"pooled"}`)
	if body := bodyOf(t, resp); !strings.Contains(body, "Pooled mode needs tokens") {
		t.Errorf("bridge→pooled response = %q", body)
	}
}

// TestDashboardTokenAddEnvOverrideFails is the regression for the compose
// env_file bug: docker-compose injects every .env line into the container
// environment, and config.Load lets the real environment override the file.
// Adding a token via the dashboard then persists to .env but the reload sees
// the env's stale AUTH_TOKENS= — the pool would hold the token while cfg
// claims bridge mode. The add must fail loudly and roll the pool back, not
// silently diverge.
func TestDashboardTokenAddEnvOverrideFails(t *testing.T) {
	t.Chdir(t.TempDir())
	// The environment outranks .env in config.Load: a stale empty
	// AUTH_TOKENS= env var (exactly what compose env_file injects) defeats
	// any .env AUTH_TOKENS write.
	t.Setenv("AUTH_TOKENS", "")
	original := "SAFE_MODE=true\n"
	if err := os.WriteFile(".env", []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, testutil.NewMock())
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"cb_fresh"}`)
	body := bodyOf(t, resp)
	if !strings.Contains(body, "overrides .env") {
		t.Fatalf("add response = %q, want env-override failure message", body)
	}
	// The pool must be rolled back — no token may linger while cfg claims
	// bridge mode.
	if got := p.TokenCount(); got != 1 {
		t.Errorf("pool TokenCount = %d, want 1 (original mock token; add rolled back)", got)
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf(".env after failed add = %q, want byte-exact original %q (persist must roll back)", got, original)
	}
}

// TestDashboardModeSwitchPersistFailure pins the persist-failure branch: when
// .env cannot be written (here: .env is a directory), the switch reports the
// failure and neither the pool nor the config is drained.
func TestDashboardModeSwitchPersistFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	// A directory at .env makes every .env writer fail deterministically on
	// every platform (a read-only dir is unreliable on Windows).
	if err := os.Mkdir(".env", 0o755); err != nil {
		t.Fatal(err)
	}
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, testutil.NewMock())
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/mode", `{"mode":"hybrid"}`)
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Failed to persist .env") {
		t.Fatalf("mode response = %q, want persist failure", body)
	}
	if got := p.TokenCount(); got != 1 {
		t.Errorf("pool TokenCount = %d, want 1 (persist failure must not drain the pool)", got)
	}
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d: %s", resp.StatusCode, data)
	}
	var hz struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &hz); err != nil {
		t.Fatal(err)
	}
	if hz.Mode != "pooled" {
		t.Errorf("healthz mode = %q, want pooled (switch must not land)", hz.Mode)
	}
}

// TestDashboardModeSwitchVerifyFailureRollsBack pins the verify-failure
// branch: when a higher-precedence source (here: the real environment) keeps
// AUTH_TOKENS set despite the .env write, the switch rolls the .env back
// byte-for-byte and reports the override.
func TestDashboardModeSwitchVerifyFailureRollsBack(t *testing.T) {
	t.Chdir(t.TempDir())
	// The environment outranks .env in config.Load: AUTH_TOKENS here defeats
	// the bridge switch's empty AUTH_TOKENS= write.
	t.Setenv("AUTH_TOKENS", "env-tok")
	original := "SAFE_MODE=true\nAUTH_TOKENS=tok-0\n"
	if err := os.WriteFile(".env", []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, testutil.NewMock())
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/mode", `{"mode":"bridge"}`)
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Could not switch to bridge mode") {
		t.Fatalf("mode response = %q, want verify-failure message", body)
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf(".env after failed switch = %q, want byte-exact original %q", got, original)
	}
	if p.TokenCount() != 1 {
		t.Errorf("pool TokenCount = %d, want 1 (drain happens only after verify)", p.TokenCount())
	}
}

// --- config save guards ---

// TestDashboardConfigSaveEmptyContentRejected is the regression for the P2
// bug: an empty save (urlencoded POST without content=, an empty text/plain
// body, or whitespace-only content) must be rejected with the file preserved
// — never a silent empty .env.
func TestDashboardConfigSaveEmptyContentRejected(t *testing.T) {
	t.Chdir(t.TempDir())
	original := "SAFE_MODE=true\nMAX_MESSAGES_PER_DAY=7\n"
	if err := os.WriteFile(".env", []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// urlencoded POST with no content= field at all.
	resp := postForm(t, ts.URL, cookie, "/admin/config", url.Values{})
	body := bodyOf(t, resp)
	if !strings.Contains(body, "empty .env content") {
		t.Errorf("no-content-field response = %q, want empty-content rejection", body)
	}

	// Empty text/plain body.
	resp = postConfig(t, ts.URL, cookie, "")
	if body := bodyOf(t, resp); !strings.Contains(body, "empty .env content") {
		t.Errorf("empty-body response = %q, want empty-content rejection", body)
	}

	// Whitespace-only body.
	resp = postConfig(t, ts.URL, cookie, "   \n\t")
	if body := bodyOf(t, resp); !strings.Contains(body, "empty .env content") {
		t.Errorf("whitespace-body response = %q, want empty-content rejection", body)
	}

	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf(".env after empty saves = %q, want untouched %q", got, original)
	}
}

// TestDashboardConfigSaveOversizedBody pins the 64KB editor cap: a larger
// payload is rejected before any write and the file is preserved.
func TestDashboardConfigSaveOversizedBody(t *testing.T) {
	t.Chdir(t.TempDir())
	original := "SAFE_MODE=true\n"
	if err := os.WriteFile(".env", []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	resp := postConfig(t, ts.URL, cookie, strings.Repeat("a", 64<<10+1))
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Failed to read request body") {
		t.Errorf("oversized save response = %q, want read failure", body)
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf(".env after oversized save = %q, want untouched %q", got, original)
	}
}

// TestDashboardConfigSaveConcurrent pins last-writer-wins under concurrency:
// two simultaneous saves serialize on adminSaveMu and the final .env is
// exactly one of the two contents — never a torn merge.
func TestDashboardConfigSaveConcurrent(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	contentA := "# config A\nSAFE_MODE=true\nMAX_MESSAGES_PER_DAY=7\n"
	contentB := "# config B\nSAFE_MODE=false\nLISTEN_ADDR=127.0.0.1:9999\n"

	var wg sync.WaitGroup
	responses := make([]string, 2)
	for i, content := range []string{contentA, contentB} {
		wg.Add(1)
		go func(i int, content string) {
			defer wg.Done()
			resp := postConfig(t, ts.URL, cookie, content)
			responses[i] = bodyOf(t, resp)
		}(i, content)
	}
	wg.Wait()

	for i, body := range responses {
		if !strings.Contains(body, "Saved and reloaded") {
			t.Errorf("concurrent save %d response = %q, want success", i, body)
		}
	}
	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	got := string(env)
	if got != contentA && got != contentB {
		t.Errorf(".env after concurrent saves = %q, want exactly contentA or contentB", got)
	}
}

// TestDashboardConfigSaveWhenEnvAbsent: a valid save with no prior .env
// creates the file.
func TestDashboardConfigSaveWhenEnvAbsent(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	content := "# fresh\nSAFE_MODE=true\n"
	resp := postConfig(t, ts.URL, cookie, content)
	if body := bodyOf(t, resp); !strings.Contains(body, "Saved and reloaded") {
		t.Fatalf("save response = %q, want success", body)
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf(".env = %q, want %q", got, content)
	}
}

// TestDashboardConfigSaveRejectedRemovesFile: a rejected save with no prior
// .env removes the file it wrote (nothing to restore) instead of leaving a
// broken .env behind.
func TestDashboardConfigSaveRejectedRemovesFile(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// LISTEN_ADDR without a port fails Validate.
	resp := postConfig(t, ts.URL, cookie, "LISTEN_ADDR=127.0.0.1\n")
	if body := bodyOf(t, resp); !strings.Contains(body, "Configuration rejected") {
		t.Fatalf("save response = %q, want rejection", body)
	}
	if _, err := os.Stat(".env"); !os.IsNotExist(err) {
		t.Errorf(".env still exists after rejected save with no prior file (err=%v)", err)
	}
}

// TestDashboardReloadFailureKeepsConfig pins handleReload's failure path: an
// invalid .env yields 500 reload_failed and the live config stays in force.
func TestDashboardReloadFailureKeepsConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("LISTEN_ADDR=127.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, testutil.NewMock())

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil, map[string]string{"Authorization": "Bearer secret"})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("reload status = %d, want 500: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "reload_failed") {
		t.Errorf("reload body missing reload_failed: %s", data)
	}

	// The old config is still live: pooled mode with its token.
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d: %s", resp.StatusCode, data)
	}
	var hz struct {
		Mode   string `json:"mode"`
		Tokens []any  `json:"tokens"`
	}
	if err := json.Unmarshal(data, &hz); err != nil {
		t.Fatal(err)
	}
	if hz.Mode != "pooled" || len(hz.Tokens) != 1 {
		t.Errorf("healthz after failed reload = mode %q tokens %d, want pooled 1 (cfg unchanged)", hz.Mode, len(hz.Tokens))
	}
}

// --- smoke ---

// TestDashboardSmokePromptTooLong pins the 200-char prompt cap.
func TestDashboardSmokePromptTooLong(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)
	long := strings.Repeat("x", 201)
	resp := postJSON(t, ts.URL, cookie, "/admin/smoke", `{"model":"`+modelA+`","prompt":"`+long+`"}`)
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Prompt too long (max 200 chars)") {
		t.Errorf("smoke response = %q, want prompt-too-long rejection", body)
	}
}

// TestDashboardSmokeBridgeWithoutToken: bridge mode requires a client token
// in the smoke payload.
func TestDashboardSmokeBridgeWithoutToken(t *testing.T) {
	ts := bridgeDashboardServer(t, "secret")
	cookie := authedCookie(t, ts)
	resp := postJSON(t, ts.URL, cookie, "/admin/smoke", `{"model":"`+modelA+`","prompt":"ping"}`)
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Bridge mode: include a client token in the smoke request.") {
		t.Errorf("bridge smoke response = %q, want client-token message", body)
	}
}

// TestDashboardSmokeEmptyRegistry: with no catalog models there is nothing to
// smoke-test against.
func TestDashboardSmokeEmptyRegistry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	cfg := &config.Config{
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		AdminToken:         "secret",
	}
	reg := registry.New(cfg, nil) // no fallback
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/smoke", `{"prompt":"ping"}`)
	body := bodyOf(t, resp)
	if !strings.Contains(body, "No models in the registry to test.") {
		t.Errorf("empty-registry smoke response = %q", body)
	}
}

// --- diag ---

// TestDashboardDiagBridgeMode: in bridge mode diag has no pooled tokens to
// probe — it reports the no-pooled-tokens warning instead of running probes.
func TestDashboardDiagBridgeMode(t *testing.T) {
	ts := bridgeDashboardServer(t, "secret")
	cookie := authedCookie(t, ts)
	resp := postJSON(t, ts.URL, cookie, "/admin/diag", "{}")
	body := bodyOf(t, resp)
	if !strings.Contains(body, "No pooled tokens to probe") {
		t.Errorf("bridge diag response = %q, want no-pooled-tokens warning", body)
	}
	if !strings.Contains(body, "Configuration: bridge mode") {
		t.Errorf("bridge diag missing mode line: %s", body)
	}
	if strings.Contains(body, "validity probe") {
		t.Errorf("bridge diag must not run probes:\n%s", body)
	}
}

// TestDashboardDiagTokenProbeFailure: an auth-rejecting token produces a
// failed validity-probe row in the diag fragment (probes run unconditionally;
// the old probe_tokens opt-in is gone).
func TestDashboardDiagTokenProbeFailure(t *testing.T) {
	bad := testutil.NewMock()
	defer bad.Close()
	bad.AuthReject = true
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, bad)
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/diag", "{}")
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Token #1 validity probe failed") {
		t.Errorf("diag response = %q, want probe-failure row", body)
	}
}

// TestDashboardDiagProbesRunUnconditionally: per-token validity probes are
// zero-cost upstream GETs (no session claim), so plain diag runs them by
// default and the stale probe_tokens opt-in param is ignored. Success is
// asserted without any upstream session create.
func TestDashboardDiagProbesRunUnconditionally(t *testing.T) {
	t.Run("runs by default", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, mock)
		cookie := authedCookie(t, ts)

		resp := postJSON(t, ts.URL, cookie, "/admin/diag", "{}")
		body := bodyOf(t, resp)
		if !strings.Contains(body, "Token #1 validity probe succeeded") {
			t.Errorf("diag missing probe-success row:\n%s", body)
		}
		if strings.Contains(body, "probes skipped") {
			t.Errorf("diag still reports probes skipped:\n%s", body)
		}
		if got := mock.SessionCreatesSnapshot(); got != 0 {
			t.Errorf("diag created %d upstream session(s), want 0 (zero-cost probe)", got)
		}
	})

	t.Run("probe_tokens param ignored", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, mock)
		cookie := authedCookie(t, ts)

		// The stale probe_tokens=true opt-in must not change behavior: probes
		// run unconditionally either way.
		resp := postForm(t, ts.URL, cookie, "/admin/diag", url.Values{"probe_tokens": {"true"}})
		body := bodyOf(t, resp)
		if !strings.Contains(body, "Token #1 validity probe succeeded") {
			t.Errorf("diag with probe_tokens=true missing probe-success row:\n%s", body)
		}
		if strings.Contains(body, "probes skipped") {
			t.Errorf("diag with probe_tokens=true still reports probes skipped:\n%s", body)
		}
		if got := mock.SessionCreatesSnapshot(); got != 0 {
			t.Errorf("diag created %d upstream session(s), want 0 (zero-cost probe)", got)
		}
	})
}

// --- CSRF ---

// csrfPost issues a POST with the given extra headers and returns the body.
func csrfPost(t *testing.T, url, cookie, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, bodyOf(t, resp)
}

// TestDashboardCSRFOriginNull: a sandboxed iframe's "Origin: null" must be
// rejected — it parses to an empty host that cannot match the proxy's own.
func TestDashboardCSRFOriginNull(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)
	status, body := csrfPost(t, ts.URL, cookie, "/admin/tokens/0/unlock", "", map[string]string{"Origin": "null"})
	if status != http.StatusForbidden {
		t.Fatalf("Origin:null POST status = %d, want 403", status)
	}
	if !strings.Contains(body, "Cross-origin request rejected.") {
		t.Errorf("Origin:null body = %q, want rejection message", body)
	}
}

// TestDashboardCSRFDifferentPortOrigin: an Origin whose port differs from the
// listener's is a different origin and must be rejected (host comparison
// includes the port).
func TestDashboardCSRFDifferentPortOrigin(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)
	status, body := csrfPost(t, ts.URL, cookie, "/admin/tokens/0/unlock", "", map[string]string{"Origin": "http://127.0.0.1:1"})
	if status != http.StatusForbidden {
		t.Fatalf("different-port Origin POST status = %d, want 403", status)
	}
	if !strings.Contains(body, "Cross-origin request rejected.") {
		t.Errorf("different-port body = %q, want rejection message", body)
	}
}

// TestDashboardCSRFSecFetchSiteCombos drives the Sec-Fetch-Site matrix: a
// cross-site fetch marker rejects even with a matching Origin, a cross-site
// Origin rejects even with same-origin Sec-Fetch-Site, and same-origin /
// none pass through.
func TestDashboardCSRFSecFetchSiteCombos(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)
	host := strings.TrimPrefix(ts.URL, "http://")

	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"matching origin + cross-site fetch", map[string]string{"Origin": ts.URL, "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"cross-site origin + same-origin fetch", map[string]string{"Origin": "http://evil.example", "Sec-Fetch-Site": "same-origin"}, http.StatusForbidden},
		{"matching origin + same-origin fetch", map[string]string{"Origin": ts.URL, "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"matching origin + none fetch", map[string]string{"Origin": ts.URL, "Sec-Fetch-Site": "none"}, http.StatusOK},
		{"no origin + same-origin fetch", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := csrfPost(t, ts.URL, cookie, "/admin/tokens/0/unlock", "", tc.hdr)
			if status != tc.want {
				t.Errorf("status = %d, want %d (body %q, host %q)", status, tc.want, body, host)
			}
		})
	}
}

// TestDashboardCSRFLoginGate: the login POST consumes the per-IP attempt
// budget, so it must carry the same CSRF gate as the other mutating admin
// routes — a cross-origin POST is rejected before it can burn a victim's
// login attempts (repeatable cross-site lockout DoS). Header-less clients
// (curl, API clients, tests) still pass through and can log in.
func TestDashboardCSRFLoginGate(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)

	status, body := csrfPost(t, ts.URL, "", "/admin/login", "token=secret", map[string]string{"Origin": "http://evil.example"})
	if status != http.StatusForbidden {
		t.Fatalf("cross-origin login POST status = %d, want 403", status)
	}
	if !strings.Contains(body, "Cross-origin request rejected.") {
		t.Errorf("cross-origin login body = %q, want rejection message", body)
	}

	// Header-less POST (curl/legacy clients) must still authenticate.
	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("header-less login POST status = %d, want 302", resp.StatusCode)
	}
	if cookies := resp.Cookies(); len(cookies) != 1 {
		t.Errorf("header-less login cookies = %d, want 1", len(cookies))
	}
}

// --- cookie / auth edges ---

// TestDashboardMalformedCookieRedirects: a cookie value without the expiry
// dot separator fails validation → 302 to login.
func TestDashboardMalformedCookieRedirects(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	resp := get(t, ts.URL+"/admin", "fb_admin=no-dot-here")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("malformed-cookie status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Errorf("redirect location = %q, want /admin/login", loc)
	}
}

// TestDashboardSensitiveRemoteWithCookieAllowed: with ADMIN_TOKEN set the
// cookie IS the gate — a remote client holding a valid session cookie may
// read the sensitive config page (the loopback restriction only applies in
// the default-open mode).
func TestDashboardSensitiveRemoteWithCookieAllowed(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("remote authed config status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestDashboardAssetsNoListing404 pins the asset gate: directory requests
// never render a listing, and unknown assets 404.
func TestDashboardAssetsNoListing404(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)

	resp := get(t, ts.URL+"/admin/assets/", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /admin/assets/ status = %d, want 404 (no listing)", resp.StatusCode)
	}

	resp = get(t, ts.URL+"/admin/assets/nope.css", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown asset status = %d, want 404", resp.StatusCode)
	}
}

// TestDashboardLoginGETWhenUnset: with ADMIN_TOKEN unset, GET /admin/login
// redirects straight to the dashboard (no login page exists).
func TestDashboardLoginGETWhenUnset(t *testing.T) {
	ts := dashboardServer(t, "", nil)
	resp := get(t, ts.URL+"/admin/login", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login GET status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin" {
		t.Errorf("redirect location = %q, want /admin", loc)
	}
}

// TestDashboardLoginGETWithToken serves the SPA login page (HTML), not a JSON
// error body: the Svelte form must be reachable without a session cookie.
func TestDashboardLoginGETWithToken(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	resp := get(t, ts.URL+"/admin/login", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login GET status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("login GET Content-Type = %q, want text/html (SPA page)", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<div id=\"app\">") {
		t.Errorf("login GET body does not look like the SPA index (missing #app mount)")
	}
}
