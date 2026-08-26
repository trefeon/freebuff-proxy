package dashboard_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/dashboard"
	"freebuff-proxy/internal/logring"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// newTestDashboard wires a real (mock-upstream) stack behind the dashboard:
// one pooled token by default, or bridge mode when tokens is 0.
func newTestDashboard(t *testing.T, tokens int) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         make([]string, tokens),
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	mock := testutil.NewMock()
	clients := make([]*upstream.Client, 0, tokens)
	sessions := make([]*session.Manager, 0, tokens)
	for i := 0; i < tokens; i++ {
		cfg.AuthTokens[i] = fmt.Sprintf("tok-%d", i)
		clientCfg := *cfg
		clientCfg.UpstreamBaseURL = mock.URL()
		client, err := upstream.New(cfg.AuthTokens[i], &clientCfg)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManager(client))
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, clients, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
	ts := httptest.NewServer(d.APIHandler("overview"))
	t.Cleanup(ts.Close)
	return ts
}

func TestPageOverviewFull(t *testing.T) {
	ts := newTestDashboard(t, 0) // bridge mode: no fixed tokens
	resp, err := http.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := string(mustReadAll(t, resp))
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if data["mode"] != "bridge" {
		t.Errorf("mode = %v, want bridge", data["mode"])
	}
	if data["model_count"] == nil || data["model_count"].(float64) == 0 {
		t.Error("model_count missing or zero")
	}
}

func TestPageOverviewFragment(t *testing.T) {
	ts := newTestDashboard(t, 1) // pooled mode: one fixed token
	resp, err := http.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := string(mustReadAll(t, resp))
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if data["mode"] != "pooled" {
		t.Errorf("mode = %v, want pooled", data["mode"])
	}
}

func TestLoginPageRendersError(t *testing.T) {
	cfg := &config.Config{
		UpstreamBaseURL: "https://www.codebuff.com",
		AuthTokens:      []string{},
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
	rec := httptest.NewRecorder()
	d.RenderLogin(rec, httptest.NewRequest(http.MethodGet, "/admin/login", nil), "Invalid admin token.")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if data["error"] != "Invalid admin token." {
		t.Errorf("error = %v, want 'Invalid admin token.'", data["error"])
	}
}

// newDashboardForPages builds a dashboard with a wired log ring, mounting the
// given page (defaults to "logs").
func newDashboardForPages(t *testing.T, withRing bool, page ...string) *httptest.Server {
	t.Helper()
	name := "logs"
	if len(page) > 0 && page[0] != "" {
		name = page[0]
	}
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		ListenAddr:         "127.0.0.1:3457",
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	mock := testutil.NewMock()
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New("tok-0", &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{session.NewManager(client)}, reg)
	if err != nil {
		t.Fatal(err)
	}
	var ring *logring.Handler
	if withRing {
		ring = logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 100)
		slog.New(ring).Info("hello ring", "k", "v")
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, ring)
	ts := httptest.NewServer(d.APIHandler(name))
	t.Cleanup(ts.Close)
	return ts
}

func TestLogsPageWithRing(t *testing.T) {
	ts := newDashboardForPages(t, true)
	resp, err := http.Get(ts.URL + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := string(mustReadAll(t, resp))
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if data["enabled"] != true {
		t.Error("logs page should report ring as enabled")
	}
}

func TestLogsPageWithoutRing(t *testing.T) {
	ts := newDashboardForPages(t, false)
	resp, err := http.Get(ts.URL + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := string(mustReadAll(t, resp))
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if data["enabled"] != false {
		t.Error("logs page should report the ring as disabled")
	}
}

// TestLogsPageFilters pins the T19 filter row: ?level and ?msg (substring,
// case-insensitive) render only matching rows, the empty state switches to
// the filtered copy when a filter matches nothing, and the filter controls
// are present for the hx-get wiring.
func TestLogsPageFilters(t *testing.T) {
	ts := newDashboardForPages(t, true) // seeds one INFO "hello ring" record

	get := func(path string) map[string]any {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var data map[string]any
		if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
			t.Fatalf("response is not valid JSON: %v", err)
		}
		return data
	}

	// Default: enabled with entries.
	data := get("/logs")
	if data["enabled"] != true {
		t.Error("logs should be enabled")
	}

	// level=warn excludes the INFO record.
	data = get("/logs?level=warn")
	if data["enabled"] != true {
		t.Error("logs should be enabled")
	}
	entries, _ := data["entries"].([]any)
	if len(entries) != 0 {
		t.Errorf("level=warn filter returned %d entries, want 0", len(entries))
	}

	// level=info keeps the INFO record.
	data = get("/logs?level=info")
	entries, _ = data["entries"].([]any)
	if len(entries) != 1 {
		t.Errorf("level=info filter returned %d entries, want 1", len(entries))
	}

	// msg matching nothing flips to empty.
	data = get("/logs?msg=zzz-none")
	entries, _ = data["entries"].([]any)
	if len(entries) != 0 {
		t.Errorf("msg=zzz-none filter returned %d entries, want 0", len(entries))
	}
}

func TestMetricsPageRendersSparklines(t *testing.T) {
	cfg := &config.Config{
		UpstreamBaseURL: "https://www.codebuff.com",
		AuthTokens:      []string{},
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
	rec := httptest.NewRecorder()
	d.APIHandler("metrics")(rec, httptest.NewRequest(http.MethodGet, "/admin/api/metrics", nil))
	page := rec.Body.String()
	for _, want := range []string{"requests_spark", "retries_spark", "fingerprint_rotations", "svg"} {
		if !strings.Contains(page, want) {
			t.Errorf("metrics page missing %q in: %s", want, page[:min(len(page), 300)])
		}
	}
}

func mustReadAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The restricted page renders the access-denied error as JSON.
func TestRestrictedPageRenders(t *testing.T) {
	cfg := &config.Config{
		UpstreamBaseURL: "https://www.codebuff.com",
		AuthTokens:      []string{},
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
	rec := httptest.NewRecorder()
	d.RenderRestricted(rec, httptest.NewRequest(http.MethodGet, "/admin/config", nil), "blocked")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if data["error"] != "blocked" {
		t.Errorf("error = %v, want 'blocked'", data["error"])
	}
}

// The models page returns the catalog with agent mappings as JSON.
func TestModelsPageRenders(t *testing.T) {
	ts := newDashboardForPages(t, false, "models")
	resp, err := http.Get(ts.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if data["count"].(float64) == 0 {
		t.Error("model count should be > 0")
	}
	models, _ := data["models"].([]any)
	if len(models) == 0 {
		t.Error("models array should not be empty")
	}
}

// The setup page returns the base URL and client info as JSON.
func TestSetupPageRenders(t *testing.T) {
	ts := newDashboardForPages(t, false, "setup")
	resp, err := http.Get(ts.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if data["base_url"] == nil || data["base_url"].(string) == "" {
		t.Error("setup page missing base_url")
	}
}

// The traces page returns the trace data as JSON.
func TestTracesPageRenders(t *testing.T) {
	ts := newDashboardForPages(t, true, "traces")
	resp, err := http.Get(ts.URL + "/traces")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if data["enabled"] != true {
		t.Error("traces page should report enabled")
	}
}

// --- data-path coverage (AuditServer priority 10) ---

// pageServer builds a dashboard server mounting the named page over a pool
// with the given token count; mut adjusts the config before the stack is
// built, ring wires the log viewer when non-nil. Returns the pool so tests
// can seed cooldowns or assert counters.
func pageServer(t *testing.T, tokens int, page string, mut func(*config.Config), ring *logring.Handler) (*httptest.Server, *pool.Pool) {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         make([]string, tokens),
		ListenAddr:         "127.0.0.1:3457",
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	if mut != nil {
		mut(cfg)
	}
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	clients := make([]*upstream.Client, 0, tokens)
	sessions := make([]*session.Manager, 0, tokens)
	for i := 0; i < tokens; i++ {
		cfg.AuthTokens[i] = fmt.Sprintf("tok-%d", i)
		clientCfg := *cfg
		clientCfg.UpstreamBaseURL = mock.URL()
		client, err := upstream.New(cfg.AuthTokens[i], &clientCfg)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManager(client))
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, clients, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, ring)
	ts := httptest.NewServer(d.APIHandler(page))
	t.Cleanup(ts.Close)
	return ts, p
}

// dashModel is the fallback-registry model the quota seeding chats use.
const dashModel = "deepseek/deepseek-v4-flash"

// quotaPageServer builds a tokens page whose session admission carries the
// mock-configured quota state (rateLimitsByModel entries, glmPromo block),
// driven through a real pool chat so the token snapshot gains QuotaByModel/
// GlmPromo (the only way the quota table renders).
func quotaPageServer(t *testing.T, mut func(*testutil.MockUpstream)) *httptest.Server {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	mut(mock)
	mock.ChatBody = testutil.SSEEvent(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"`+dashModel+`","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`) +
		testutil.SSEEvent(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"`+dashModel+`","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := p.Acquire(ctx, dashModel)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	up, err := p.Chat(ctx, lease, upstream.ChatOptions{Model: dashModel, RunID: lease.Run.RunID, SessionInstanceID: lease.SessionInstanceID},
		[]byte(`{"model":"`+dashModel+`","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_, _ = io.Copy(io.Discard, up)
	_ = up.Close()
	p.LeaseRelease(lease)

	ts := httptest.NewServer(d.APIHandler("tokens"))
	t.Cleanup(ts.Close)
	return ts
}

// TestTokensPageQuotaRows pins the quota table JSON fields.
func TestTokensPageQuotaRows(t *testing.T) {
	reset := time.Now().Add(4*time.Hour + 12*time.Minute).UTC().Format(time.RFC3339)
	ts := quotaPageServer(t, func(m *testutil.MockUpstream) {
		m.RateLimitsByModel = map[string]any{
			dashModel: map[string]any{
				"model":       dashModel,
				"limit":       100,
				"recentCount": 120, // > limit → UsagePct clamps to 100
				"period":      "pacific_day",
				"resetAt":     reset,
				"entitlementBreakdown": map[string]any{
					"base":     1,
					"referral": 1,
					"streak":   3,
				},
			},
			"anthropic/claude-fable-5": map[string]any{
				"model":       "anthropic/claude-fable-5",
				"limit":       100,
				"recentCount": 80, // exactly at NearLimit threshold
				"period":      "pacific_day",
				"resetAt":     reset,
			},
		}
	})
	resp, err := http.Get(ts.URL + "/tokens")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	tokens, _ := data["tokens"].([]any)
	if len(tokens) == 0 {
		t.Fatal("no tokens in response")
	}
	token, _ := tokens[0].(map[string]any)
	quota, _ := token["quota"].([]any)
	if len(quota) == 0 {
		t.Fatal("no quota entries")
	}
	// Find the exhausted row (the dashModel one).
	for _, q := range quota {
		qm := q.(map[string]any)
		if qm["model"] == dashModel {
			if qm["usage_pct"].(float64) != 100 {
				t.Errorf("usage_pct = %v, want 100", qm["usage_pct"])
			}
			if qm["near_limit"] != true {
				t.Error("near_limit should be true for exhausted row")
			}
			if qm["has_entitlement"] != true {
				t.Error("has_entitlement should be true")
			}
		}
	}
}

// TestTokensDataGlmPromoSynthesis pins the synthesized z-ai/glm-5.2 promo
// quota row (issue #178): the upstream glmPromo block ({dailySessions,
// endsAt}) grants a referral quota on scarce models like GLM, so the
// dashboard renders it even when no rateLimitsByModel entry was admitted.
func TestTokensDataGlmPromoSynthesis(t *testing.T) {
	ts := quotaPageServer(t, func(m *testutil.MockUpstream) {
		m.GlmPromo = map[string]any{
			"dailySessions": 2,
			"endsAt":        "2026-08-22T07:00:00Z",
		}
	})
	resp, err := http.Get(ts.URL + "/tokens")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	tokens, _ := data["tokens"].([]any)
	if len(tokens) == 0 {
		t.Fatal("no tokens in response")
	}
	token, _ := tokens[0].(map[string]any)
	quota, _ := token["quota"].([]any)
	if len(quota) == 0 {
		t.Fatal("no quota entries")
	}
	var glm map[string]any
	for _, q := range quota {
		qm := q.(map[string]any)
		if qm["model"] == "z-ai/glm-5.2" {
			glm = qm
			break
		}
	}
	if glm == nil {
		t.Fatal("no z-ai/glm-5.2 quota row")
	}
	if glm["limit"] != "2" {
		t.Errorf("limit = %v, want 2", glm["limit"])
	}
	if glm["period"] != "promo" {
		t.Errorf("period = %v, want promo", glm["period"])
	}
	if glm["entitled"] != "referral" {
		t.Errorf("entitled = %v, want referral", glm["entitled"])
	}
	if glm["has_bar"] != true {
		t.Error("has_bar should be true for promo row")
	}
	if glm["has_entitlement"] != true {
		t.Error("has_entitlement should be true for promo row")
	}
	if glm["recent"] != "0" {
		t.Errorf("recent = %v, want 0", glm["recent"])
	}
}
func TestTokensPagePureBridgeInBridgeCard(t *testing.T) {
	ts, _ := pageServer(t, 0, "tokens", nil, nil)
	resp, err := http.Get(ts.URL + "/tokens")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if data["in_bridge"] != true {
		t.Errorf("in_bridge = %v, want true", data["in_bridge"])
	}
	if data["token_count"].(float64) != 0 {
		t.Errorf("token_count = %v, want 0", data["token_count"])
	}
}

// TestOverviewPageCooldownCard pins the cooldown-active card in JSON.
func TestOverviewPageCooldownCard(t *testing.T) {
	ts, p := pageServer(t, 1, "overview", nil, nil)
	p.CooldownToken(0, time.Hour)
	resp, err := http.Get(ts.URL + "/overview")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	tokens, _ := data["tokens"].([]any)
	if len(tokens) == 0 {
		t.Fatal("no tokens in response")
	}
	token, _ := tokens[0].(map[string]any)
	if token["cooldown_active"] != true {
		t.Errorf("cooldown_active = %v, want true", token["cooldown_active"])
	}
	if token["risk_level"] != "high" {
		t.Errorf("risk_level = %v, want high", token["risk_level"])
	}
}

// TestOverviewPageHasTokens pins the #200 regression: df7a16a dropped the
// HasTokens assignment in overviewData, leaving has_tokens permanently
// false — Overview rendered "No upstream tokens configured" while the
// Tokens tab listed the same pool. With a pooled token present, both the
// flag and the cards must be populated.
func TestOverviewPageHasTokens(t *testing.T) {
	ts, _ := pageServer(t, 1, "overview", nil, nil)
	resp, err := http.Get(ts.URL + "/overview")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	tokens, _ := data["tokens"].([]any)
	if len(tokens) == 0 {
		t.Fatal("no tokens in response")
	}
	if data["has_tokens"] != true {
		t.Errorf("has_tokens = %v with %d tokens, want true", data["has_tokens"], len(tokens))
	}
}

// TestModelsPageAliases pins the alias table JSON.
func TestModelsPageAliases(t *testing.T) {
	ts, _ := pageServer(t, 1, "models", func(c *config.Config) {
		c.ModelAliases = map[string]string{"gpt-4o": dashModel, "sonnet": "anthropic/claude-sonnet-5"}
	}, nil)
	resp, err := http.Get(ts.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if data["has_aliases"] != true {
		t.Error("has_aliases should be true")
	}
	aliases, _ := data["aliases"].([]any)
	if len(aliases) != 2 {
		t.Fatalf("aliases count = %d, want 2", len(aliases))
	}
}

// TestTracesPageWithLiveTrace pins the chat-trace field parsing in JSON.
func TestTracesPageWithLiveTrace(t *testing.T) {
	ring := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 100)
	slog.New(ring).Info("chat trace", "token", "1", "model", dashModel, "status", "ok", "ms", 42)
	slog.New(ring).Info("chat trace", "token", "bridge", "model", "deepseek/deepseek-v4-flash", "status", "error", "ms", 7, "error", "upstream")
	ts, _ := pageServer(t, 1, "traces", nil, ring)
	resp, err := http.Get(ts.URL + "/traces")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	traces, _ := data["traces"].([]any)
	if len(traces) != 2 {
		t.Fatalf("traces count = %d, want 2", len(traces))
	}
}

// TestSetupPageKeyHintModes pins the setup KeyHint per mode in JSON.
func TestSetupPageKeyHintModes(t *testing.T) {
	tsBridge, _ := pageServer(t, 0, "setup", nil, nil)
	resp, err := http.Get(tsBridge.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	_ = resp.Body.Close()
	if data["bridge"] != true {
		t.Errorf("bridge setup should have bridge=true")
	}
	if data["key_hint"] == nil || data["key_hint"].(string) == "" {
		t.Error("setup page missing key_hint")
	}
}

// TestConfigPageEnvAbsentTemplate pins the editor seed JSON.
func TestConfigPageEnvAbsentTemplate(t *testing.T) {
	t.Chdir(t.TempDir())
	ts, _ := pageServer(t, 0, "config", nil, nil)
	resp, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if data["has_env_file"] != false {
		t.Error("has_env_file should be false when no .env exists")
	}
	envContent, _ := data["env_content"].(string)
	if !strings.Contains(envContent, "# freebuff-proxy configuration") {
		t.Error("env_content missing default template")
	}
}

// TestConfigPageCRLFVerbatim pins the editor fidelity JSON.
func TestConfigPageCRLFVerbatim(t *testing.T) {
	t.Chdir(t.TempDir())
	crlf := "SAFE_MODE=true\r\nMAX_MESSAGES_PER_DAY=7\r\n"
	if err := os.WriteFile(".env", []byte(crlf), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, _ := pageServer(t, 0, "config", nil, nil)
	resp, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	envContent, _ := data["env_content"].(string)
	if !strings.Contains(envContent, "SAFE_MODE=true\r\nMAX_MESSAGES_PER_DAY=7\r\n") {
		t.Errorf("config page did not render CRLF content verbatim in:\n%s", envContent)
	}
}

// TestRenderConfigResultFragment pins the JSON result response: a config
// save result renders as a bare JSON object, not a full page.
func TestRenderConfigResultFragment(t *testing.T) {
	cfg := &config.Config{UpstreamBaseURL: "https://www.codebuff.com"}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)

	fragReq := httptest.NewRequest(http.MethodPost, "/admin/config", nil)
	fragReq.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	d.RenderConfigResult(rec, fragReq, true, "Saved and reloaded")
	frag := rec.Body.String()
	if strings.Contains(frag, "<html") {
		t.Error("HX-Request config result rendered a full page")
	}
	for _, want := range []string{`"ok":true`, `"Saved and reloaded"`} {
		if !strings.Contains(frag, want) {
			t.Errorf("config fragment missing %q: %s", want, frag)
		}
	}

	plainReq := httptest.NewRequest(http.MethodPost, "/admin/config", nil)
	rec = httptest.NewRecorder()
	d.RenderConfigResult(rec, plainReq, false, "rejected")
	plain := rec.Body.String()
	if !strings.Contains(plain, `"ok":false`) || !strings.Contains(plain, "rejected") {
		t.Errorf("plain config result = %q, want JSON with ok:false and message", plain)
	}
}

// TestRenderSmokeResultFragment pins the smoke-result JSON response: the
// model/token/ms summary and the bounded preview render as JSON.
func TestRenderSmokeResultFragment(t *testing.T) {
	cfg := &config.Config{UpstreamBaseURL: "https://www.codebuff.com"}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/admin/smoke", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	d.RenderSmokeResult(rec, req, dashModel, "bridge", 123, []byte("preview bytes"), []dashboard.PhaseKV{{Name: "acquire_ms", Ms: 5}, {Name: "total_ms", Ms: 123}})
	frag := rec.Body.String()
	if strings.Contains(frag, "<html") {
		t.Error("HX-Request smoke result rendered a full page")
	}
	for _, want := range []string{`"ok":true`, `"model":"` + dashModel + `"`, `"token":"bridge"`, `"ms":123`, `"preview":"preview bytes"`, `"name":"acquire_ms"`, `"name":"total_ms"`} {
		if !strings.Contains(frag, want) {
			t.Errorf("smoke fragment missing %q: %s", want, frag)
		}
	}
}

// TestTokensPageStanding renders the #96 account-standing block end-to-end:
// a session admission carrying the upstream "standing" field surfaces the
// access level/label/score/next-level pill on the tokens page.
func TestTokensPageStanding(t *testing.T) {
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	mock.Standing = map[string]any{
		"level":        "verified",
		"label":        "Verified",
		"score":        30,
		"nextLevelAt":  "2026-08-20T12:00:00Z",
		"nextLevel":    "established",
		"cappedBy":     "anonymous_network",
		"cappedReason": "Egress IP is a hosting ASN.",
		"blurb":        "Trust capped by your network.",
		"nextSteps": []map[string]any{
			{"id": "verify_email", "label": "Verify your email", "detail": "Adds 25 points.", "points": 25, "href": "/settings"},
		},
	}
	mock.ChatBody = testutil.SSEEvent(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"`+dashModel+`","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`) +
		testutil.SSEEvent(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"`+dashModel+`","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
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

	// Admit a real session so the standing block is cached, then render.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := p.Acquire(ctx, dashModel)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	up, err := p.Chat(ctx, lease, upstream.ChatOptions{Model: dashModel, RunID: lease.Run.RunID, SessionInstanceID: lease.SessionInstanceID},
		[]byte(`{"model":"`+dashModel+`","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_, _ = io.Copy(io.Discard, up)
	_ = up.Close()
	p.LeaseRelease(lease)

	ts := httptest.NewServer(d.APIHandler("tokens"))
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	for _, want := range []string{
		`"standing_label":"Verified"`, `"standing_score":30`, `"2026-08-20T12:00:00Z"`, `"established"`,
		// Issue #140 P3d: cap + earn-back fields serialize onto the page.
		`"standing_capped_by":"anonymous_network"`,
		`"standing_capped_reason":"Egress IP is a hosting ASN."`,
		`"standing_blurb":"Trust capped by your network."`,
		`"standing_next_steps":[{"id":"verify_email","label":"Verify your email","detail":"Adds 25 points.","points":25,"href":"/settings"}]`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("tokens page missing standing %q in: %s", want, page[:min(len(page), 500)])
		}
	}
}

// TestDashboardModelsViewsServeOnly pins the served gate on the three
// dashboard model views (models/overview/setup): the vendor registry also
// carries god-only/eval rows (luna-es since snapshot 0603bc1) that must
// never appear as servable in the admin UI.
func TestDashboardModelsViewsServeOnly(t *testing.T) {
	ts := newDashboardForPages(t, false, "models")
	client := ts.Client()
	for _, page := range []string{"models", "overview", "setup"} {
		resp, err := client.Get(ts.URL + "/" + page)
		if err != nil {
			t.Fatalf("%s: %v", page, err)
		}
		body := string(mustReadAll(t, resp))
		_ = resp.Body.Close()
		if strings.Contains(body, "luna-es") {
			t.Errorf("%s view leaked god-only luna-es", page)
		}
	}

}
