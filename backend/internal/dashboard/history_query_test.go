package dashboard_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/dashboard"
	"freebuff-proxy/backend/internal/store"
)

func seedHistory(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.RecordQuota(store.QuotaSnapshot{TS: 1000, TokenIdx: 0, Model: "m", Limit: 75, Recent: 30, ResetAt: 2000}); err != nil {
		t.Fatalf("RecordQuota: %v", err)
	}
	if err := st.RecordQuota(store.QuotaSnapshot{TS: 2000, TokenIdx: 0, Model: "m", Limit: 75, Recent: 31, ResetAt: 2000}); err != nil {
		t.Fatalf("RecordQuota: %v", err)
	}
	if err := st.RecordMaturity(store.MaturityEvent{TS: 1500, TokenIdx: 0, Kind: "touch", Detail: "admit ok"}); err != nil {
		t.Fatalf("RecordMaturity: %v", err)
	}
	if err := st.AppendLogs([]store.LogEntry{{TS: 1200, Level: "info", Msg: "chat done", ReqID: "r1"}}); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}
	return st
}

func historyMux(st *store.Store) *httptest.Server {
	cfg := &config.Config{}
	d := dashboard.New(func() *config.Config { return cfg }, nil, nil, slog.Default(), nil, dashboard.WithHistory(st))
	mux := http.NewServeMux()
	mux.Handle("GET /admin/api/quota/history", d.APIHandler("quota/history"))
	mux.Handle("GET /admin/api/maturity/history", d.APIHandler("maturity/history"))
	mux.Handle("GET /admin/api/logs/history", d.APIHandler("logs/history"))
	return httptest.NewServer(mux)
}

func getHistoryJSON(t *testing.T, url string) map[string]any {
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

func TestQuotaHistoryEndpoint(t *testing.T) {
	ts := historyMux(seedHistory(t))
	t.Cleanup(ts.Close)

	got := getHistoryJSON(t, ts.URL+"/admin/api/quota/history?token=0&model=m")
	if got["enabled"] != true {
		t.Fatalf("enabled = %v, want true", got["enabled"])
	}
	snaps, _ := got["snapshots"].([]any)
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %d, want 2 (oldest first)", len(snaps))
	}
	first, _ := snaps[0].(map[string]any)
	if first["recent"] != 30.0 || first["limit"] != 75.0 {
		t.Fatalf("first snapshot = %v, want recent=30 limit=75", first)
	}
	// since filters oldest-first rows by cutoff.
	got = getHistoryJSON(t, ts.URL+"/admin/api/quota/history?token=0&model=m&since=1001")
	if snaps, _ := got["snapshots"].([]any); len(snaps) != 1 {
		t.Fatalf("since snapshots = %d, want 1", len(snaps))
	}
	// Missing params yield empty rows, never an error.
	got = getHistoryJSON(t, ts.URL+"/admin/api/quota/history")
	if got["enabled"] != true {
		t.Fatalf("enabled = %v, want true (store attached)", got["enabled"])
	}
	if snaps, _ := got["snapshots"].([]any); len(snaps) != 0 {
		t.Fatalf("unfiltered snapshots = %d, want 0", len(snaps))
	}
}

func TestMaturityHistoryEndpoint(t *testing.T) {
	ts := historyMux(seedHistory(t))
	t.Cleanup(ts.Close)

	got := getHistoryJSON(t, ts.URL+"/admin/api/maturity/history?token=0")
	if got["enabled"] != true {
		t.Fatalf("enabled = %v, want true", got["enabled"])
	}
	events, _ := got["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev, _ := events[0].(map[string]any)
	if ev["kind"] != "touch" || ev["detail"] != "admit ok" {
		t.Fatalf("event = %v", ev)
	}
}

func TestLogsHistoryEndpoint(t *testing.T) {
	ts := historyMux(seedHistory(t))
	t.Cleanup(ts.Close)

	got := getHistoryJSON(t, ts.URL+"/admin/api/logs/history?msg=chat")
	if got["enabled"] != true {
		t.Fatalf("enabled = %v, want true", got["enabled"])
	}
	entries, _ := got["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e, _ := entries[0].(map[string]any)
	if e["msg"] != "chat done" || e["req_id"] != "r1" || e["level"] != "info" {
		t.Fatalf("entry = %v", e)
	}
}

// Live-only dashboards answer every history row with enabled=false.
func TestHistoryEndpointsLiveOnly(t *testing.T) {
	cfg := &config.Config{}
	d := dashboard.New(func() *config.Config { return cfg }, nil, nil, slog.Default(), nil)
	mux := http.NewServeMux()
	mux.Handle("GET /admin/api/quota/history", d.APIHandler("quota/history"))
	mux.Handle("GET /admin/api/maturity/history", d.APIHandler("maturity/history"))
	mux.Handle("GET /admin/api/logs/history", d.APIHandler("logs/history"))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	for _, path := range []string{"/admin/api/quota/history?token=0&model=m", "/admin/api/maturity/history?token=0", "/admin/api/logs/history"} {
		if got := getHistoryJSON(t, ts.URL+path); got["enabled"] != false {
			t.Errorf("%s enabled = %v, want false", path, got["enabled"])
		}
	}
}
