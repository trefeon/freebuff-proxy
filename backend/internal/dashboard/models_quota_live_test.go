package dashboard

import (
	"encoding/json"
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

// quotaFor prefers the LIVE wire snapshot (rateLimitsByModel mirrored per
// token) over static catalog copy. For premium pool models it renders
// "<limit> premium quota" (e.g. "5 premium quota"), using Limit only — the
// catalog view shows the model-level quota, not per-token usage; per-token
// usage remains in the Tokens → per-token quota table. Fractional limits
// are formatted via formatSessionUnits ("5", "1.6"). Non-premium rows still
// render "1.6 of 5 used" when they gain live data.
func TestModelsPageLiveQuotaLabel(t *testing.T) {
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
	// Seed the token's session manager with the observed live state BEFORE
	// pool construction: recentCount 1.6 (0.6 settled + 1.0 active
	// reservation), limit 5. UpdateQuotaFromProbe is the same path the
	// admission/poll response uses to mirror the wire quota.
	mgr := session.NewManager(client)
	mgr.UpdateQuotaFromProbe(&upstream.SessionState{
		RateLimitsByModel: map[string]upstream.ModelQuota{
			"openai/gpt-5.6-luna": {Model: "openai/gpt-5.6-luna", Limit: 5, RecentCount: 1.6, Period: "pacific_day"},
		},
	})
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{mgr}, reg)
	if err != nil {
		t.Fatal(err)
	}

	d := New(func() *config.Config { return cfg }, p, reg, nil, nil)
	ts := httptest.NewServer(d.APIHandler("models"))
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data struct {
		Models []struct {
			ID    string `json:"id"`
			Quota string `json:"quota"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	quotaBy := map[string]string{}
	for _, m := range data.Models {
		quotaBy[m.ID] = m.Quota
	}
	if got, want := quotaBy["openai/gpt-5.6-luna"], "5 premium quota"; got != want {
		t.Errorf("live quota label = %q, want %q (premium quota, limit only)", got, want)
	}
}
