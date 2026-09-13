package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/store"
)

// logWindowDashboard builds a dashboard over a fresh history store with the
// given config and ring. Cleanup closes both (the dashboard owns the store
// handle, as in production).
func logWindowDashboard(t *testing.T, cfg *config.Config, ring *logring.Handler) (*Dashboard, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "logwindow.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d := New(func() *config.Config { return cfg }, nil, nil, slog.Default(), ring, WithHistory(st))
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return d, st
}

func logWindowConfig() *config.Config {
	return &config.Config{
		LogConsoleWindow:  config.DefaultLogConsoleWindow,
		LogTableRetention: config.DefaultLogTableRetention,
	}
}

func logWindowMessages(t *testing.T, ld logsData) []string {
	t.Helper()
	out := make([]string, 0, len(ld.Entries))
	for _, e := range ld.Entries {
		out = append(out, e.Message)
	}
	return out
}

func containsMessage(msgs []string, want string) bool {
	for _, m := range msgs {
		if m == want {
			return true
		}
	}
	return false
}

// TestLogConsoleWindowFiltersHistoryRows pins the console window: history rows
// older than LOG_CONSOLE_WINDOW are not returned, rows inside it are, and the
// live ring keeps rendering (the ring carries the newest activity and is never
// window-filtered). The ?window= parameter widens the view for one request
// without touching what is stored.
func TestLogConsoleWindowFiltersHistoryRows(t *testing.T) {
	ring := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 16)
	slog.New(ring).Info("ring row")

	d, st := logWindowDashboard(t, logWindowConfig(), ring)
	now := store.Millis(time.Now())
	const minute = int64(time.Minute / time.Millisecond)
	if err := st.AppendLogs([]store.LogEntry{
		{TS: now - 90*minute, Level: "info", Msg: "old history row"},
		{TS: now - 5*minute, Level: "info", Msg: "recent history row"},
	}); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}

	ld := d.logsData(nil)
	if ld.Window != "1h0m0s" {
		t.Errorf("window = %q, want 1h0m0s (the knob default)", ld.Window)
	}
	msgs := logWindowMessages(t, ld)
	if !containsMessage(msgs, "ring row") {
		t.Errorf("entries = %v, want the ring row to render inside the window", msgs)
	}
	if !containsMessage(msgs, "recent history row") {
		t.Errorf("entries = %v, want the in-window history row", msgs)
	}
	if containsMessage(msgs, "old history row") {
		t.Errorf("entries = %v, want the out-of-window history row filtered out", msgs)
	}

	// The same query with a wider request window reaches the older row.
	wide := d.logsData(httptest.NewRequest("GET", "/admin/api/logs?window=6h", nil))
	if wide.Window != "6h0m0s" {
		t.Errorf("window = %q after ?window=6h, want 6h0m0s", wide.Window)
	}
	if got := logWindowMessages(t, wide); !containsMessage(got, "old history row") {
		t.Errorf("entries = %v, want the 90m-old row inside a 6h window", got)
	}
}

// TestLogConsoleWindowOverrideAndClamp pins the ?window= contract: a valid
// duration overrides LOG_CONSOLE_WINDOW, while absent, unparsable, and
// non-positive values fall back to the knob and out-of-range values clamp.
func TestLogConsoleWindowOverrideAndClamp(t *testing.T) {
	cfg := logWindowConfig()
	cfg.LogConsoleWindow = 30 * time.Minute
	d, _ := logWindowDashboard(t, cfg, nil)

	for _, tc := range []struct {
		query string
		want  string
	}{
		{"", "30m0s"},
		{"?window=15m", "15m0s"},
		{"?window=24h", "24h0m0s"},
		{"?window=bogus", "30m0s"},
		{"?window=0s", "30m0s"},
		{"?window=-5m", "30m0s"},
		{"?window=", "30m0s"},
		{"?window=10s", "1m0s"},
		{"?window=1000h", "168h0m0s"},
	} {
		ld := d.logsData(httptest.NewRequest("GET", "/admin/api/logs"+tc.query, nil))
		if ld.Window != tc.want {
			t.Errorf("logsData(%q).Window = %q, want %q", tc.query, ld.Window, tc.want)
		}
	}
}

// TestLogConsoleWindowDropsTheRowCap is the 200-row cap removal: a busy window
// returns every stored row up to the ceiling, not a truncated 200.
func TestLogConsoleWindowDropsTheRowCap(t *testing.T) {
	d, st := logWindowDashboard(t, logWindowConfig(), nil)
	now := store.Millis(time.Now())
	const minute = int64(time.Minute / time.Millisecond)
	rows := make([]store.LogEntry, 0, 250)
	for i := 0; i < 250; i++ {
		rows = append(rows, store.LogEntry{
			TS:     now - 5*minute - int64(i),
			Level:  "info",
			Msg:    "spilled row",
			Fields: "seq=" + string(rune('a'+i%26)),
		})
	}
	if err := st.AppendLogs(rows); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}

	ld := d.logsData(nil)
	if len(ld.Entries) != 250 {
		t.Fatalf("entries = %d, want 250 (the old 200-row cap must be gone)", len(ld.Entries))
	}
	if ld.Truncated {
		t.Error("truncated = true, want false below the ceiling")
	}
}

// TestLogConsoleWindowTruncationFlag pins the ceiling: a window holding more
// rows than one response may carry reports Truncated so the UI can say the
// view is partial, and the payload stops at the ceiling.
func TestLogConsoleWindowTruncationFlag(t *testing.T) {
	d, st := logWindowDashboard(t, logWindowConfig(), nil)
	now := store.Millis(time.Now())
	const minute = int64(time.Minute / time.Millisecond)
	rows := make([]store.LogEntry, 0, logConsoleLimit+50)
	for i := 0; i < logConsoleLimit+50; i++ {
		rows = append(rows, store.LogEntry{TS: now - 10*minute - int64(i), Level: "info", Msg: "busy row"})
	}
	if err := st.AppendLogs(rows); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}

	ld := d.logsData(nil)
	if len(ld.Entries) != logConsoleLimit {
		t.Fatalf("entries = %d, want the ceiling %d", len(ld.Entries), logConsoleLimit)
	}
	if !ld.Truncated {
		t.Error("truncated = false, want true when the ceiling is hit")
	}

	// The JSON names are the frontend contract for the window label and the
	// partial-view note.
	blob, err := json.Marshal(ld)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(blob, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := payload["window"]; !ok {
		t.Errorf("payload keys = %v, want a window field", payload)
	}
	if v, ok := payload["truncated"]; !ok || v != true {
		t.Errorf("payload[truncated] = %v, want true", payload["truncated"])
	}
}

// TestPurgeHonorsLogTableRetention pins the knob-driven purge: log_entries and
// request_records use LOG_TABLE_RETENTION, quota and maturity keep their own
// 90-day window.
func TestPurgeHonorsLogTableRetention(t *testing.T) {
	cfg := logWindowConfig()
	cfg.LogTableRetention = 2 * time.Hour
	d, st := logWindowDashboard(t, cfg, nil)

	now := store.Millis(time.Now())
	const day = int64(24 * time.Hour / time.Millisecond)
	const halfHour = int64(30 * time.Minute / time.Millisecond)
	if err := st.AppendLogs([]store.LogEntry{
		{TS: now - halfHour, Level: "info", Msg: "fresh log", ReqID: "fresh-req"},
		{TS: now - 3*day, Level: "info", Msg: "stale log", ReqID: "stale-req"},
	}); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}
	if err := st.RecordRequest(store.RequestRecord{ReqID: "fresh-req", TS: now - halfHour, Endpoint: "/v1/chat", Status: "200"}); err != nil {
		t.Fatalf("RecordRequest(fresh): %v", err)
	}
	if err := st.RecordRequest(store.RequestRecord{ReqID: "stale-req", TS: now - 3*day, Endpoint: "/v1/chat", Status: "200"}); err != nil {
		t.Fatalf("RecordRequest(stale): %v", err)
	}
	if err := st.RecordQuota(store.QuotaSnapshot{TS: now - 10*day, TokenIdx: 0, Model: "m", Limit: 75}); err != nil {
		t.Fatalf("RecordQuota(recent): %v", err)
	}
	if err := st.RecordQuota(store.QuotaSnapshot{TS: now - 100*day, TokenIdx: 1, Model: "m", Limit: 75}); err != nil {
		t.Fatalf("RecordQuota(old): %v", err)
	}
	if err := st.RecordMaturity(store.MaturityEvent{TS: now - 10*day, TokenIdx: 0, Kind: "touch"}); err != nil {
		t.Fatalf("RecordMaturity(recent): %v", err)
	}
	if err := st.RecordMaturity(store.MaturityEvent{TS: now - 100*day, TokenIdx: 0, Kind: "touch"}); err != nil {
		t.Fatalf("RecordMaturity(old): %v", err)
	}

	d.purgeHistory()

	logs, err := st.QueryLogs(store.LogFilter{})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(logs) != 1 || logs[0].Msg != "fresh log" {
		t.Fatalf("logs after purge = %+v, want only the 30m-old row (2h knob)", logs)
	}
	reqs, err := st.QueryRequests(0, 100)
	if err != nil {
		t.Fatalf("QueryRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].ReqID != "fresh-req" {
		t.Fatalf("requests after purge = %+v, want only the fresh request outcome", reqs)
	}
	// Quota and maturity are sparse change points: the 10-day-old rows must
	// survive the 2h log retention, and the 100-day-old ones must not.
	if quotas, err := st.QuotaHistory(0, "m", 0, 100); err != nil || len(quotas) != 1 {
		t.Fatalf("token 0 quota rows = %d (err %v), want 1 (90d retention, not the 2h knob)", len(quotas), err)
	}
	if quotas, err := st.QuotaHistory(1, "m", 0, 100); err != nil || len(quotas) != 0 {
		t.Fatalf("token 1 quota rows = %d (err %v), want 0 (older than 90d)", len(quotas), err)
	}
	if events, err := st.MaturityHistory(0, 0, 100); err != nil || len(events) != 1 {
		t.Fatalf("maturity rows = %d (err %v), want 1 (90d retention)", len(events), err)
	}
}

// TestPurgeZeroRetentionFallsBackToDefault is the data-loss guard: a
// hand-built config with the zero value must not resolve to "delete
// everything" — the documented 7-day default applies instead.
func TestPurgeZeroRetentionFallsBackToDefault(t *testing.T) {
	d, st := logWindowDashboard(t, &config.Config{}, nil)
	now := store.Millis(time.Now())
	const day = int64(24 * time.Hour / time.Millisecond)
	if err := st.AppendLogs([]store.LogEntry{
		{TS: now - 3*day, Level: "info", Msg: "3 days old"},
		{TS: now - 8*day, Level: "info", Msg: "8 days old"},
	}); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}
	if err := st.RecordRequest(store.RequestRecord{ReqID: "r-3d", TS: now - 3*day, Endpoint: "/v1/chat"}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}

	d.purgeHistory()

	logs, err := st.QueryLogs(store.LogFilter{})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(logs) != 1 || logs[0].Msg != "3 days old" {
		t.Fatalf("logs after purge = %+v, want only the 3-day-old row inside the 7d default", logs)
	}
	reqs, err := st.QueryRequests(0, 100)
	if err != nil {
		t.Fatalf("QueryRequests: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("requests after purge = %d, want the 3-day-old outcome kept", len(reqs))
	}
}
