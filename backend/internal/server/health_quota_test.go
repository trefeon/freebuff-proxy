package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/server"
	"freebuff-proxy/backend/internal/testutil"

	"net/http/httptest"
)

func TestHealthzPremiumQuotaOmitted(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	mock.RateLimitsByModel = map[string]any{
		"openai/gpt-5.6-luna": map[string]any{
			"model":       "openai/gpt-5.6-luna",
			"limit":       4,
			"recentCount": 2,
			"period":      "pacific_day",
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
	if _, ok := tok["premium_quota"]; ok {
		t.Errorf("premium_quota present want omitted (ADR-0027), token=%+v", tok)
	}

	resp, body = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d", resp.StatusCode)
	}
	metrics := string(body)
	for _, want := range []string{
		`freebuff_proxy_premium_quota_limit{token="1"} 4`,
		`freebuff_proxy_premium_quota_used{token="1"} 2`,
		`freebuff_proxy_premium_quota_remaining{token="1"} 2`,
	} {
		if strings.Contains(metrics, want) {
			t.Errorf("metrics contains removed premium gauge %q (ADR-0027)\nmetrics:\n%s", want, metrics)
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
		"openai/gpt-5.6-luna": map[string]any{
			"model":       "openai/gpt-5.6-luna",
			"limit":       4,
			"recentCount": 4,
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
		if _, ok := be["premium_quota"]; ok {
			t.Errorf("bridge premium_quota present want omitted (ADR-0027), entry=%+v", be)
		}
	}
}
