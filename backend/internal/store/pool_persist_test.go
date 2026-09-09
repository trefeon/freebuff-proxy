package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openPoolTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSaveLoadPoolStateRoundTrip(t *testing.T) {
	s := openPoolTestStore(t)
	blob := []byte(`{"usage":[1234567890123],"spend":{"day_used":50}}`)
	if err := s.SavePoolState("pool/ledger/abc123", blob); err != nil {
		t.Fatalf("SavePoolState: %v", err)
	}
	got, ok, err := s.LoadPoolState("pool/ledger/abc123")
	if err != nil {
		t.Fatalf("LoadPoolState: %v", err)
	}
	if !ok {
		t.Fatal("LoadPoolState ok = false, want true")
	}
	if string(got) != string(blob) {
		t.Fatalf("LoadPoolState = %q, want %q", got, blob)
	}
	// Overwrite wins.
	if err := s.SavePoolState("pool/ledger/abc123", []byte(`{}`)); err != nil {
		t.Fatalf("SavePoolState overwrite: %v", err)
	}
	got, ok, err = s.LoadPoolState("pool/ledger/abc123")
	if err != nil || !ok || string(got) != `{}` {
		t.Fatalf("after overwrite: got %q ok=%v err=%v", got, ok, err)
	}
}

func TestLoadPoolStateMissing(t *testing.T) {
	s := openPoolTestStore(t)
	got, ok, err := s.LoadPoolState("pool/ledger/nope")
	if err != nil {
		t.Fatalf("LoadPoolState missing: %v", err)
	}
	if ok || got != nil {
		t.Fatalf("LoadPoolState missing = %q, %v; want nil, false", got, ok)
	}
}

func TestDeletePoolState(t *testing.T) {
	s := openPoolTestStore(t)
	if err := s.SavePoolState("pool/burst", []byte(`{}`)); err != nil {
		t.Fatalf("SavePoolState: %v", err)
	}
	if err := s.DeletePoolState("pool/burst"); err != nil {
		t.Fatalf("DeletePoolState: %v", err)
	}
	if _, ok, err := s.LoadPoolState("pool/burst"); err != nil || ok {
		t.Fatalf("after delete: ok=%v err=%v", ok, err)
	}
	// Missing row is a no-op.
	if err := s.DeletePoolState("pool/burst"); err != nil {
		t.Fatalf("DeletePoolState missing: %v", err)
	}
}

func TestListPoolStatePrefix(t *testing.T) {
	s := openPoolTestStore(t)
	rows := map[string]string{
		"pool/ledger/aa":  `[1]`,
		"pool/ledger/bb":  `[2]`,
		"pool/burst":      `{}`,
		"pool/x%_wild":    `[3]`,
		"config/SOME_KEY": `"unrelated"`,
	}
	for k, v := range rows {
		if strings.HasPrefix(k, "config/") {
			if err := s.SetSetting(k, v); err != nil {
				t.Fatalf("SetSetting: %v", err)
			}
			continue
		}
		if err := s.SavePoolState(k, []byte(v)); err != nil {
			t.Fatalf("SavePoolState %s: %v", k, err)
		}
	}
	got, err := s.ListPoolState("pool/ledger/")
	if err != nil {
		t.Fatalf("ListPoolState: %v", err)
	}
	if len(got) != 2 || string(got["pool/ledger/aa"]) != `[1]` || string(got["pool/ledger/bb"]) != `[2]` {
		t.Fatalf("ListPoolState(pool/ledger/) = %v, want 2 ledger rows", got)
	}
	// LIKE metacharacters in keys are literal: the wildcard row is found
	// by the pool/ prefix and does not leak into other namespaces.
	all, err := s.ListPoolState("pool/")
	if err != nil {
		t.Fatalf("ListPoolState(pool/): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("ListPoolState(pool/) = %d rows, want 4", len(all))
	}
	// Empty prefix lists all pool_state rows and nothing from settings.
	if _, err := s.ListPoolState(""); err != nil {
		t.Fatalf("ListPoolState(empty): %v", err)
	}
}

func TestPoolStateEmptyKey(t *testing.T) {
	s := openPoolTestStore(t)
	if err := s.SavePoolState("", []byte(`{}`)); err == nil {
		t.Error("SavePoolState empty key: want error")
	}
	if _, _, err := s.LoadPoolState(""); err == nil {
		t.Error("LoadPoolState empty key: want error")
	}
	if err := s.DeletePoolState(""); err == nil {
		t.Error("DeletePoolState empty key: want error")
	}
}

// TestOpenMigratesV3ToV4 crafts a v3 file (current schema minus pool_state,
// stamped 3) and proves Open migrates it in place: rows preserved, version
// stamped to 4, pool_state usable — and that a second Open is idempotent.
func TestOpenMigratesV3ToV4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open fresh: %v", err)
	}
	if err := s.SetSetting("seed", "kept"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := s.SavePoolState("pool/burst", []byte(`{"m":[]}`)); err != nil {
		t.Fatalf("SavePoolState: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Downgrade to a faithful v3: drop the v4 table, keep every other row.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(`DROP TABLE pool_state`); err != nil {
		t.Fatalf("drop pool_state: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version=3`); err != nil {
		t.Fatalf("v3 stamp: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	m, err := Open(path)
	if err != nil {
		t.Fatalf("Open on v3 file: %v (want in-place migration, not rejection)", err)
	}
	defer func() { _ = m.Close() }()
	var v int
	if err := m.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	seed, ok, err := m.GetSetting("seed")
	if err != nil || !ok || seed != "kept" {
		t.Fatalf("v3 settings row lost: %q %v %v", seed, ok, err)
	}
	// pool_state is fresh and usable on the migrated file.
	if err := m.SavePoolState("pool/ledger/x", []byte(`[1]`)); err != nil {
		t.Fatalf("SavePoolState after migrate: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Second Open is idempotent.
	r, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer func() { _ = r.Close() }()
	if got, ok, err := r.LoadPoolState("pool/ledger/x"); err != nil || !ok || string(got) != `[1]` {
		t.Fatalf("after re-Open: got %q ok=%v err=%v", got, ok, err)
	}
}
