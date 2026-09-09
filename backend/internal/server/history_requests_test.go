package server

import (
	"log/slog"
	"path/filepath"
	"testing"

	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/phasetiming"
	"freebuff-proxy/backend/internal/store"
)

// traceChat persists the request outcome alongside the ring log: the Logs
// console view's request_records row must exist without any new endpoint.
func TestTraceChatRecordsRequestOutcome(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ring := logring.NewHandler(slog.Default().Handler(), 16)
	srv, _ := newTestServerStack(t, nil, nil, nil, slog.Default(), ring, WithHistory(st))
	srv.traceChat(nil, "m/a", 42, "ok", "", map[string]int64{phasetiming.UpstreamTTFBMS: 7}, &chatTraceState{reqID: "r-trace-1"})
	// Pre-attempt refusals carry no req_id: ring log only, no DB row.
	srv.traceChat(nil, "m/a", 1, "error", "refused", nil, nil)
	rows, err := st.QueryRequests(0, 0)
	if err != nil {
		t.Fatalf("QueryRequests: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("request rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.ReqID != "r-trace-1" || got.Model != "m/a" || got.Status != "ok" || got.TTFBms != 7 || got.TokenIdx != -1 {
		t.Fatalf("request row = %+v, want r-trace-1/m/a/ok/ttfb 7/token -1", got)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
