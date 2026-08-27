package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/server"
	"freebuff-proxy/internal/testutil"

	"net/http/httptest"
)

func TestHealthzPremiumQuotaEmitted(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	mock.RateLimitsByModel = map[string]any{
		"deepseek/deepseek-v4-flash": map[string]any{
			"model":       "deepseek/deepseek-v4-flash",
			"limit":       5,
			"recentCount": 2,
			"period":      "pacific_day",
			"resetAt":     future,
		},
		"z-ai/glm-5.3-flash": map[string]any{
			"model":       "z-ai/glm-5.3-flash",
			"limit":       2,
			"recentCount": 1,
			"period":      "glm_v53_flash",
			"resetAt":     future,
		},
	}
	ts, p := newTestServer(t, nil, mock)
	if _, err := p.Acquire(t.Context(), "deepseek/deepseek-v4-flash"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	resp, body := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d body=%s", resp.StatusCode, string(body))
	}
	var hz struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal(body, &hz); err != nil {
		t.Fatalf("healthz json: %v body=%s", err, string(body))
	}
	if len(hz.Tokens) != 1 {
		t.Fatalf("tokens = %d want 1", len(hz.Tokens))
	}
	tok := hz.Tokens[0]
	pq, ok := tok["premium_quota"]
	if !ok {
		t.Fatalf("premium_quota missing: token=%+v", tok)
	}
	m, ok := pq.(map[string]any)
	if !ok {
		t.Fatalf("premium_quota not map: %T", pq)
	}
	if int(m["limit"].(float64)) != 5 {
		t.Errorf("premium limit = %v want 5", m["limit"])
	}
	if int(m["used"].(float64)) != 2 {
		t.Errorf("premium used = %v want 2", m["used"])
	}
	if int(m["remaining"].(float64)) != 3 {
		t.Errorf("premium remaining = %v want 3", m["remaining"])
	}
	if int(m["percent_used"].(float64)) != 40 {
		t.Errorf("premium percent_used = %v want 40", m["percent_used"])
	}
	if m["period"] != "pacific_day" {
		t.Errorf("premium period = %v want pacific_day", m["period"])
	}
	if m["capped"].(bool) {
		t.Error("premium capped true want false")
	}
	if !m["entitled"].(bool) {
		t.Error("premium entitled false want true")
	}
	if m["model"] != "_premium_pool" {
		t.Errorf("premium model = %v want _premium_pool", m["model"])
	}
	gq, ok := tok["glm53flash_quota"]
	if !ok {
		t.Fatalf("glm53flash_quota missing")
	}
	gm, ok := gq.(map[string]any)
	if !ok {
		t.Fatalf("glm53flash_quota not map")
	}
	if int(gm["limit"].(float64)) != 2 {
		t.Errorf("glm limit = %v want 2", gm["limit"])
	}
	if int(gm["used"].(float64)) != 1 {
		t.Errorf("glm used = %v want 1", gm["used"])
	}
	if gm["period"] != "glm_v53_flash" {
		t.Errorf("glm period = %v want glm_v53_flash", gm["period"])
	}
	if gm["model"] != "z-ai/glm-5.3-flash" {
		t.Errorf("glm model = %v want z-ai/glm-5.3-flash", gm["model"])
	}

	resp, body = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d", resp.StatusCode)
	}
	metrics := string(body)
	for _, want := range []string{
		`freebuff_proxy_premium_quota_limit{token="1"} 5`,
		`freebuff_proxy_premium_quota_used{token="1"} 2`,
		`freebuff_proxy_premium_quota_remaining{token="1"} 3`,
		`freebuff_proxy_glm53flash_quota_limit{token="1"} 2`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics missing %q\nmetrics:\n%s", want, metrics)
		}
	}
}

func TestHealthzPremiumQuotaOmittedWhenNil(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)
	resp, body := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
	var hz struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal(body, &hz); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(hz.Tokens) != 1 {
		t.Fatalf("tokens = %d", len(hz.Tokens))
	}
	if _, ok := hz.Tokens[0]["premium_quota"]; ok {
		t.Errorf("premium_quota present want omitted, token=%+v", hz.Tokens[0])
	}
	if _, ok := hz.Tokens[0]["glm53flash_quota"]; ok {
		t.Errorf("glm53flash_quota present want omitted")
	}
	resp, body = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	metrics := string(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d", resp.StatusCode)
	}
	if strings.Contains(metrics, `freebuff_proxy_premium_quota_limit{token="1"}`) {
		t.Errorf("metrics should not contain premium limit gauge when quota nil\n%s", metrics)
	}
}

func TestHealthzBridgePremiumQuota(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	mock.RateLimitsByModel = map[string]any{
		"deepseek/deepseek-v4-flash": map[string]any{
			"model":       "deepseek/deepseek-v4-flash",
			"limit":       5,
			"recentCount": 5,
			"period":      "pacific_day",
			"resetAt":     future,
		},
	}
	cfg := &config.Config{
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		DashboardEnabled:   true,
		AdminToken:         "123456",
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatalf("pool.New bridge: %v", err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Trigger bridge entry creation via chat
	body := []byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	_, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", body, map[string]string{
		"Authorization": "Bearer bridge-test-token-123456",
	})

	var hz struct {
		BridgeEntries []map[string]any `json:"bridge_entries"`
	}
	_, hb := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if err := json.Unmarshal(hb, &hz); err != nil {
		t.Fatalf("healthz unmarshal: %v", err)
	}
	if len(hz.BridgeEntries) == 1 {
		be := hz.BridgeEntries[0]
		pq, ok := be["premium_quota"]
		if !ok {
			t.Errorf("bridge premium_quota missing, entry=%+v", be)
		} else {
			m := pq.(map[string]any)
			if !m["capped"].(bool) {
				t.Errorf("bridge capped false want true (5/5 future)")
			}
		}
	}
}
