package store

import (
	"os"
	"path/filepath"
	"testing"
)

func seedLegacyHistory(t *testing.T, path string) {
	t.Helper()
	old, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	if err := old.RecordQuota(QuotaSnapshot{TS: 1, TokenIdx: 0, Model: "m", Limit: 30, Recent: 3}); err != nil {
		t.Fatal(err)
	}
	if err := old.RecordMaturity(MaturityEvent{TS: 2, TokenIdx: 0, Kind: "touch", Detail: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := old.RecordRequest(RequestRecord{ReqID: "r1", TS: 3, Endpoint: "/v1/chat/completions"}); err != nil {
		t.Fatal(err)
	}
	if err := old.AppendLogs([]LogEntry{{TS: 4, Level: "info", Msg: "boot"}}); err != nil {
		t.Fatal(err)
	}
}

func TestImportLegacyHistoryDBCarriesRows(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "freebuff-history.db")
	seedLegacyHistory(t, oldPath)
	st, err := Open(filepath.Join(dir, "freebuff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	n, err := ImportLegacyHistoryDB(st, filepath.Join(dir, "freebuff.db"), oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("carried %d rows, want 4", n)
	}
	got, err := st.QuotaHistory(0, "m", 0, 10)
	if err != nil || len(got) != 1 {
		t.Errorf("quota rows after carry: %d, err=%v", len(got), err)
	}
	// Second boot is a no-op: target no longer empty, no duplicates.
	n, err = ImportLegacyHistoryDB(st, filepath.Join(dir, "freebuff.db"), oldPath)
	if err != nil || n != 0 {
		t.Errorf("second import = (%d, %v), want (0, nil)", n, err)
	}
	// Legacy file is left in place, never deleted.
	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("legacy file missing after import: %v", err)
	}
}
func TestImportLegacyHistoryDBReadOnlySource(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "freebuff-history.db")
	seedLegacyHistory(t, oldPath)
	if err := os.Chmod(oldPath, 0o444); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(oldPath, 0o644) }()
	st, err := Open(filepath.Join(dir, "freebuff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	// ATTACH needs write access; the import stages a temp copy instead.
	n, err := ImportLegacyHistoryDB(st, filepath.Join(dir, "freebuff.db"), oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("carried %d rows, want 4", n)
	}
}
func TestImportLegacySkipsEmptyCandidate(t *testing.T) {
	dir := t.TempDir()
	// First candidate exists but holds no history rows; the scan must
	// continue to the second file instead of stopping (first-wins bug).
	emptyPath := filepath.Join(dir, "empty.db")
	empty, err := Open(emptyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	fullPath := filepath.Join(dir, "freebuff-history.db")
	seedLegacyHistory(t, fullPath)
	st, err := Open(filepath.Join(dir, "freebuff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if n, err := ImportLegacyHistoryDB(st, filepath.Join(dir, "freebuff.db"), emptyPath); err != nil || n != 0 {
		t.Fatalf("empty candidate = (%d, %v), want (0, nil)", n, err)
	}
	n, err := ImportLegacyHistoryDB(st, filepath.Join(dir, "freebuff.db"), fullPath)
	if err != nil || n != 4 {
		t.Fatalf("full candidate = (%d, %v), want (4, nil)", n, err)
	}
}

func TestImportLegacyStagesWALSidecars(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "freebuff-history.db")
	// Live-writer simulation: a row sits uncheckpointed in -wal while the
	// file is read-only for everyone else (direct ATTACH must fail over
	// to the staged trio copy).
	writer, err := Open(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if err := writer.RecordQuota(QuotaSnapshot{TS: 1, TokenIdx: 0, Model: "m", Limit: 30, Recent: 3}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(oldPath, 0o444); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(oldPath, 0o644) }()
	st, err := Open(filepath.Join(dir, "freebuff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	n, err := ImportLegacyHistoryDB(st, filepath.Join(dir, "freebuff.db"), oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("carried %d rows, want 1 (uncheckpointed WAL row)", n)
	}
}

func TestImportLegacyHistoryDBNoops(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "freebuff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if n, err := ImportLegacyHistoryDB(st, filepath.Join(dir, "freebuff.db"), filepath.Join(dir, "absent.db")); err != nil || n != 0 {
		t.Errorf("missing file = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := ImportLegacyHistoryDB(st, filepath.Join(dir, "freebuff.db"), filepath.Join(dir, "freebuff.db")); err != nil || n != 0 {
		t.Errorf("same path = (%d, %v), want (0, nil)", n, err)
	}
	bad := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(bad, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLegacyHistoryDB(st, filepath.Join(dir, "freebuff.db"), bad); err == nil {
		t.Error("garbage file accepted, want error")
	}
}

// TestCountLegacyHistoryRows pins the multi-era inspector: per-table counts
// from a staged read-only copy, zeroes for missing/empty files, an error
// for garbage — and the source left importable afterwards (inspecting never
// modifies or locks the legacy file out of a later carry).
func TestCountLegacyHistoryRows(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "freebuff-history.db")
	seedLegacyHistory(t, oldPath)
	st, err := Open(filepath.Join(dir, "freebuff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	counts, err := CountLegacyHistoryRows(st, oldPath)
	if err != nil {
		t.Fatalf("CountLegacyHistoryRows: %v", err)
	}
	for _, table := range []string{"log_entries", "quota_snapshots", "maturity_events", "request_records"} {
		if counts[table] != 1 {
			t.Errorf("counts[%s] = %d, want 1", table, counts[table])
		}
	}
	// The inspected file still carries afterwards: counting is read-only.
	if n, err := ImportLegacyHistoryDB(st, filepath.Join(dir, "freebuff.db"), oldPath); err != nil || n != 4 {
		t.Fatalf("carry after inspect = (%d, %v), want (4, nil)", n, err)
	}
}

func TestCountLegacyHistoryRowsNoops(t *testing.T) {
	st := openTest(t)
	counts, err := CountLegacyHistoryRows(st, filepath.Join(t.TempDir(), "absent.db"))
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	for table, n := range counts {
		if n != 0 {
			t.Errorf("missing file counts[%s] = %d, want 0", table, n)
		}
	}
	counts, err = CountLegacyHistoryRows(st, "")
	if err != nil {
		t.Fatalf("empty path: %v", err)
	}
	for table, n := range counts {
		if n != 0 {
			t.Errorf("empty path counts[%s] = %d, want 0", table, n)
		}
	}
	// A live but row-less legacy file inspects as all-zeroes (an empty
	// candidate), not an error.
	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "empty-legacy.db")
	empty, err := Open(emptyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	counts, err = CountLegacyHistoryRows(st, emptyPath)
	if err != nil {
		t.Fatalf("empty legacy file: %v", err)
	}
	for table, n := range counts {
		if n != 0 {
			t.Errorf("empty legacy counts[%s] = %d, want 0", table, n)
		}
	}
	bad := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(bad, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CountLegacyHistoryRows(st, bad); err == nil {
		t.Error("garbage file inspected without error, want failure")
	}
}
