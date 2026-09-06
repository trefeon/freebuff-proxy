package dashboard

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/store"
)

func testHistoryDashboard(t *testing.T, logs *logring.Handler, st *store.Store) *Dashboard {
	t.Helper()
	d := New(func() *config.Config { return &config.Config{} }, nil, nil, slog.Default(), logs, WithHistory(st))
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return d
}

// The spill path persists every ring record: levels lowercased to the store
// domain, req_id recovered from flattened fields.
func TestHistorySpillRoundTrip(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	logs := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 16)
	d := testHistoryDashboard(t, logs, st)

	logger := slog.New(logs)
	logger.Info("chat done", "req_id", "r1")
	logger.Error("boom")

	deadline := time.Now().Add(10 * time.Second)
	var rows []store.LogEntry
	for {
		rows, err = st.QueryLogs(store.LogFilter{})
		if err != nil {
			t.Fatalf("QueryLogs: %v", err)
		}
		if len(rows) == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(rows) != 2 {
		t.Fatalf("spilled rows = %d, want 2", len(rows))
	}
	byMsg := map[string]store.LogEntry{}
	for _, r := range rows {
		byMsg[r.Msg] = r
	}
	done, ok := byMsg["chat done"]
	if !ok {
		t.Fatalf("missing chat done row: %+v", rows)
	}
	if done.Level != "info" {
		t.Errorf("level = %q, want info", done.Level)
	}
	if done.ReqID != "r1" {
		t.Errorf("req_id = %q, want r1", done.ReqID)
	}
	if byMsg["boom"].Level != "error" {
		t.Errorf("level = %q, want error", byMsg["boom"].Level)
	}
	if d.spillStats() != 0 {
		t.Errorf("dropped = %d, want 0", d.spillStats())
	}
}

// Close without WithHistory is a safe no-op; the dashboard runs live-only.
func TestHistoryCloseWithoutStore(t *testing.T) {
	d := New(func() *config.Config { return &config.Config{} }, nil, nil, slog.Default(), nil)
	if err := d.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// A nil store option keeps every history path dark: no consumer, no tap.
func TestHistoryNilStoreOption(t *testing.T) {
	logs := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 4)
	d := New(func() *config.Config { return &config.Config{} }, nil, nil, slog.Default(), logs, WithHistory(nil))
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if d.hist != nil || d.spillCh != nil {
		t.Errorf("nil store wired consumer: hist=%v spillCh=%v", d.hist != nil, d.spillCh != nil)
	}
	slog.New(logs).Info("live only")
	if got := logs.Recent(1); len(got) != 1 {
		t.Fatalf("ring holds %d, want 1", len(got))
	}
}

// The full-view sampler persists change points, not per-poll duplicates:
// same state twice writes once; a moved counter writes again.
func TestSampleQuotaDedupes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d := New(func() *config.Config { return &config.Config{} }, nil, nil, slog.Default(), nil, WithHistory(st))
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	reset := time.Date(2026, 9, 7, 7, 0, 0, 0, time.UTC)
	d.sampleQuota(0, "m", 75, 30, reset, "")
	d.sampleQuota(0, "m", 75, 30, reset, "")
	d.sampleQuota(0, "m", 75, 31, reset, "")
	rows, err := st.QuotaHistory(0, "m", 0, 0)
	if err != nil {
		t.Fatalf("QuotaHistory: %v", err)
	}
	if len(rows) != 2 || rows[0].Recent != 30 || rows[1].Recent != 31 {
		t.Fatalf("rows = %+v, want [30 31]", rows)
	}
}
