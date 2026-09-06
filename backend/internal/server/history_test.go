package server

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/store"
)

// WithHistory threads the store into the embedded dashboard and Close
// flushes it: history rows spilled during the test must be queryable after
// Close returns.
func TestServerHistoryLifecycle(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ring := logring.NewHandler(slog.Default().Handler(), 16)
	srv, _ := newTestServerStack(t, nil, nil, nil, slog.Default(), ring, WithHistory(st))
	if srv.dash == nil {
		t.Fatalf("dashboard not built")
	}
	slog.New(ring).Info("history lifecycle probe")
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := st.QueryLogs(store.LogFilter{Contains: "lifecycle probe"})
		if err != nil {
			t.Fatalf("QueryLogs: %v", err)
		}
		if len(rows) == 1 || time.Now().After(deadline) {
			if len(rows) != 1 {
				t.Fatalf("spilled rows = %d, want 1", len(rows))
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// Without the option the server closes cleanly and serves live-only.
func TestServerCloseWithoutHistory(t *testing.T) {
	srv, _ := newTestServerStack(t, nil, nil, nil, slog.Default(), nil)
	if err := srv.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
