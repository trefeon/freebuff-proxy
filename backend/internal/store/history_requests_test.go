package store

import (
	"path/filepath"
	"testing"
)

// RecordRequest rows survive a close/reopen and query newest-first.
func TestRequestRecordsReopenOrdered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "req.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rows := []RequestRecord{
		{ReqID: "r1", TS: 1000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 0, Status: "ok", TTFBms: 41},
		{ReqID: "r2", TS: 2000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 1, Status: "error", TTFBms: 7, Err: "upstream"},
		{ReqID: "r3", TS: 3000, Endpoint: "/v1/chat/completions", Model: "m/b", TokenIdx: -1, Status: "ok"},
	}
	for _, rec := range rows {
		if err := s.RecordRequest(rec); err != nil {
			t.Fatalf("RecordRequest %s: %v", rec.ReqID, err)
		}
	}
	// Empty req_id never persists (PRIMARY KEY cannot distinguish
	// pre-attempt refusals).
	if err := s.RecordRequest(RequestRecord{TS: 4000}); err != nil {
		t.Fatalf("RecordRequest empty: %v", err)
	}
	// Re-record is an idempotent upsert, not a duplicate.
	if err := s.RecordRequest(RequestRecord{ReqID: "r2", TS: 2500, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 1, Status: "error", TTFBms: 9, Err: "upstream"}); err != nil {
		t.Fatalf("RecordRequest upsert: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	got, err := s.QueryRequests(0, 0)
	if err != nil {
		t.Fatalf("QueryRequests: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3", len(got))
	}
	// Newest-first: r3 (3000), r2 (2500 upserted), r1 (1000).
	if got[0].ReqID != "r3" || got[1].ReqID != "r2" || got[2].ReqID != "r1" {
		t.Fatalf("order = %s,%s,%s, want r3,r2,r1", got[0].ReqID, got[1].ReqID, got[2].ReqID)
	}
	if got[1].TS != 2500 || got[1].TTFBms != 9 {
		t.Fatalf("r2 upsert = %+v, want TS 2500 TTFBms 9", got[1])
	}
	if got[2].TokenIdx != 0 || got[0].TokenIdx != -1 {
		t.Fatalf("token idx = %d,%d, want 0,-1", got[2].TokenIdx, got[0].TokenIdx)
	}
}

// Purge honors per-table cutoffs: only rows at/after each cutoff survive.
func TestRequestRecordsPurgeCutoff(t *testing.T) {
	s := openTest(t)
	t.Cleanup(func() { _ = s.Close() })
	const day = int64(24 * 60 * 60 * 1000)
	now := int64(1_000_000 * day)
	old, fresh := now-31*day, now-29*day
	if err := s.RecordRequest(RequestRecord{ReqID: "old", TS: old, Endpoint: "/v1/chat/completions", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRequest(RequestRecord{ReqID: "fresh", TS: fresh, Endpoint: "/v1/chat/completions", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLogs([]LogEntry{{TS: old, Level: "info", Msg: "old log"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLogs([]LogEntry{{TS: fresh, Level: "info", Msg: "fresh log"}}); err != nil {
		t.Fatal(err)
	}
	cutoff := now - 30*day
	farFuture := now + day
	if err := s.Purge(cutoff, farFuture, farFuture, cutoff); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	reqs, err := s.QueryRequests(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 || reqs[0].ReqID != "fresh" {
		t.Fatalf("requests after purge = %+v, want [fresh]", reqs)
	}
	logs, err := s.QueryLogs(LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Msg != "fresh log" {
		t.Fatalf("logs after purge = %+v, want [fresh log]", logs)
	}
}
