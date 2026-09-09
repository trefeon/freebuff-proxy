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

func restartTestConfig() func() *config.Config {
	return func() *config.Config { return &config.Config{} }
}

// A restart (empty ring, reopened DB) restores the Logs and Traces console
// views from the history store.
func TestConsoleViewsRestoreAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	const day = int64(24 * 60 * 60 * 1000)
	now := store.Millis(time.Now())
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seed := []store.LogEntry{
		{TS: now - 1000, Level: "info", Msg: "chat trace", Fields: "model=m/a\nstatus=ok\nms=42\ntoken=1"},
		{TS: now - 2000, Level: "info", Msg: "chat done", Fields: "model=m/a"},
	}
	if err := st.AppendLogs(seed); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st, err = store.Open(path)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	// Nil ring = fresh boot: nothing live retained.
	d := New(restartTestConfig(), nil, nil, slog.Default(), nil, WithHistory(st))
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	ld := d.logsData(nil)
	if !ld.Enabled {
		t.Fatal("logs Enabled = false, want true (DB-backed)")
	}
	if len(ld.Entries) != 2 {
		t.Fatalf("logs entries = %d, want 2", len(ld.Entries))
	}
	if ld.Entries[0].Message != "chat trace" || ld.Entries[1].Message != "chat done" {
		t.Fatalf("logs order = %q,%q, want chat trace,chat done (newest-first)",
			ld.Entries[0].Message, ld.Entries[1].Message)
	}

	td := d.tracesData()
	if !td.Enabled {
		t.Fatal("traces Enabled = false, want true (DB-backed)")
	}
	if len(td.Traces) != 1 {
		t.Fatalf("traces = %d, want 1", len(td.Traces))
	}
	tr := td.Traces[0]
	if tr.Model != "m/a" || tr.Token != "1" || tr.Status != "ok" || tr.Ms != "42ms" {
		t.Fatalf("trace = %+v, want model m/a token 1 status ok 42ms", tr)
	}

	// Retention honors cutoffs: the background purge drops only stale rows.
	old := now - 31*day
	if err := st.AppendLogs([]store.LogEntry{{TS: old, Level: "info", Msg: "stale line"}}); err != nil {
		t.Fatal(err)
	}
	d.purgeHistory()
	rows, err := st.QueryLogs(store.LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rows {
		if e.Msg == "stale line" {
			t.Fatalf("stale row survived purge: %+v", e)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("rows after purge = %d, want 2", len(rows))
	}
}

// A row present in both the live ring and the DB renders once.
func TestConsoleViewsMergeDedupes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "merge.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ring := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 16)
	slog.New(ring).Info("chat done", "model", "m/a")
	recent := ring.Recent(1)
	if len(recent) != 1 {
		t.Fatal("ring empty after log")
	}
	ts, err := time.Parse(time.RFC3339, recent[0].Time)
	if err != nil {
		t.Fatalf("ring time %q: %v", recent[0].Time, err)
	}
	fields := ""
	for i, f := range recent[0].Fields {
		if i > 0 {
			fields += "\n"
		}
		fields += f
	}
	if err := st.AppendLogs([]store.LogEntry{
		{TS: store.Millis(ts), Level: "info", Msg: "chat done", Fields: fields},
	}); err != nil {
		t.Fatal(err)
	}
	d := New(restartTestConfig(), nil, nil, slog.Default(), ring, WithHistory(st))
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	ld := d.logsData(nil)
	if len(ld.Entries) != 1 {
		t.Fatalf("merged entries = %d, want 1 (no double render)", len(ld.Entries))
	}
}
