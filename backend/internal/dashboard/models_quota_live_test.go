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

// quotaFor is Freebucks-based like the CLI picker: the wire prices map is
// the only source of cost. A priced row renders "<n> Freebucks/hr", a
// premium row with no live price renders "metered", other unpriced rows
// render "unmetered". No label carries session counts or the word session.
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
	// pool construction: wire prices for luna (20/hr) and flash (0/hr).
	// UpdateQuotaFromProbe is the same path the admission/poll response
	// uses to mirror the wire state.
	mgr := session.NewManager(client)
	mgr.UpdateQuotaFromProbe(&upstream.SessionState{
		Freebucks: &upstream.FreebucksInfo{
			Balance: 100,
			Prices: map[string]float64{
				"openai/gpt-5.6-luna":        20,
				"deepseek/deepseek-v4-flash": 0,
			},
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
	if got, want := quotaBy["openai/gpt-5.6-luna"], "20 Freebucks/hr"; got != want {
		t.Errorf("live quota label = %q, want %q (wire price)", got, want)
	}
	if got, want := quotaBy["deepseek/deepseek-v4-flash"], "0 Freebucks/hr"; got != want {
		t.Errorf("live quota label = %q, want %q (zero wire price)", got, want)
	}
	if q, ok := quotaBy["mimo/mimo-v2.5"]; ok && q != "unmetered" {
		t.Errorf("unpriced quota label = %q, want unmetered", q)
	}
}
