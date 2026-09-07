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
	defer old.Close()
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
	defer st.Close()
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

func TestImportLegacyHistoryDBNoops(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "freebuff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
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
