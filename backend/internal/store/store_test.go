package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenFreshStampsVersion(t *testing.T) {
	s := openTest(t)
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
}

func TestOpenRejectsUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := a.db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	_ = a.Close()
	// Reopen via a fresh handle: the file now claims a newer schema.
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted schema v99, want rejection")
	}
}

func TestOpenRejectsGarbageFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	if err := os.WriteFile(path, []byte("not a database"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a garbage file, want an error (caller degrades to live-only)")
	}
}

func TestLogRoundTripWithFilters(t *testing.T) {
	s := openTest(t)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC).UnixMilli()
	rows := []LogEntry{
		{TS: base, Level: "INFO", Msg: "chat request", Fields: "model=glm", ReqID: "r1"},
		{TS: base + 1, Level: "ERROR", Msg: "chat request refused", Fields: "reason=ban", ReqID: "r2"},
		{TS: base + 2, Level: "INFO", Msg: "chat done", Fields: "model=glm", ReqID: "r1"},
	}
	if err := s.AppendLogs(rows); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}
	if err := s.AppendLogs(nil); err != nil {
		t.Fatalf("AppendLogs(nil): %v", err)
	}

	got, err := s.QueryLogs(LogFilter{})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(got) != 3 || got[0].ReqID != "r1" || got[0].Msg != "chat done" {
		t.Fatalf("newest-first order broken: %+v", got)
	}

	got, err = s.QueryLogs(LogFilter{Level: "error"})
	if err != nil {
		t.Fatalf("level filter: %v", err)
	}
	if len(got) != 1 || got[0].ReqID != "r2" {
		t.Fatalf("level filter = %+v, want the single ERROR row", got)
	}

	got, err = s.QueryLogs(LogFilter{Contains: "refus_ed"})
	if err != nil {
		t.Fatalf("contains filter: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("LIKE metacharacter leaked: %+v", got)
	}
	got, err = s.QueryLogs(LogFilter{ReqID: "r1"})
	if err != nil {
		t.Fatalf("req filter: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("req filter = %d rows, want 2", len(got))
	}
}

func TestQuotaAndMaturityHistory(t *testing.T) {
	s := openTest(t)
	base := Millis(time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
	for i, recent := range []float64{10, 20, 30} {
		if err := s.RecordQuota(QuotaSnapshot{
			TS: base + int64(i), TokenIdx: 0, Model: "glm-5",
			Limit: 100, Recent: recent,
		}); err != nil {
			t.Fatalf("RecordQuota: %v", err)
		}
	}
	if err := s.RecordMaturity(MaturityEvent{TS: base, TokenIdx: 0, Kind: "streak", Detail: "day 7"}); err != nil {
		t.Fatalf("RecordMaturity: %v", err)
	}

	q, err := s.QuotaHistory(0, "glm-5", 0, 0)
	if err != nil {
		t.Fatalf("QuotaHistory: %v", err)
	}
	if len(q) != 3 || q[0].Recent != 10 || q[2].Recent != 30 {
		t.Fatalf("oldest-first order broken: %+v", q)
	}
	q, err = s.QuotaHistory(0, "glm-5", base+1, 0)
	if err != nil {
		t.Fatalf("QuotaHistory since: %v", err)
	}
	if len(q) != 2 {
		t.Fatalf("since cutoff = %d rows, want 2", len(q))
	}

	m, err := s.MaturityHistory(0, 0, 0)
	if err != nil {
		t.Fatalf("MaturityHistory: %v", err)
	}
	if len(m) != 1 || m[0].Kind != "streak" {
		t.Fatalf("maturity = %+v", m)
	}
}

func TestRequestUpsertKeepsLatest(t *testing.T) {
	s := openTest(t)
	base := Millis(time.Now())
	if err := s.RecordRequest(RequestRecord{ReqID: "r9", TS: base, Endpoint: "chat", Status: "ok"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.RecordRequest(RequestRecord{ReqID: "r9", TS: base + 5, Endpoint: "chat", Status: "error", Err: "ban"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var status, recErr string
	if err := s.db.QueryRow("SELECT status, error FROM request_records WHERE req_id = 'r9'").Scan(&status, &recErr); err != nil {
		t.Fatalf("select: %v", err)
	}
	if status != "error" || recErr != "ban" {
		t.Fatalf("upsert kept stale row: %s/%s", status, recErr)
	}
}

func TestPurgeKeepsFreshRows(t *testing.T) {
	s := openTest(t)
	old := Millis(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	fresh := Millis(time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC))
	cutoff := Millis(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err := s.AppendLogs([]LogEntry{
		{TS: old, Level: "INFO", Msg: "ancient"},
		{TS: fresh, Level: "INFO", Msg: "fresh"},
	}); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}
	if err := s.RecordQuota(QuotaSnapshot{TS: old, TokenIdx: 0, Model: "m"}); err != nil {
		t.Fatalf("RecordQuota: %v", err)
	}
	if err := s.RecordMaturity(MaturityEvent{TS: fresh, TokenIdx: 0, Kind: "k"}); err != nil {
		t.Fatalf("RecordMaturity: %v", err)
	}
	if err := s.Purge(cutoff, cutoff, cutoff, cutoff); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	got, err := s.QueryLogs(LogFilter{})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(got) != 1 || got[0].Msg != "fresh" {
		t.Fatalf("purge kept %+v, want only the fresh row", got)
	}
	q, _ := s.QuotaHistory(0, "m", 0, 0)
	if len(q) != 0 {
		t.Fatalf("stale quota survived purge: %+v", q)
	}
	m, _ := s.MaturityHistory(0, 0, 0)
	if len(m) != 1 {
		t.Fatalf("fresh maturity wrongly purged: %+v", m)
	}
}
