package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"
)

// fileHash snapshots one DB file's bytes after its handle is closed: a
// re-open that performs zero writes hashes identically.
func fileHash(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// TestOpenWithStatusFreshInit pins generation (a): no DB file opens as a
// fresh init running the whole goose chain, with the from-version and the
// applied steps reported.
func TestOpenWithStatusFreshInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	s, st, err := OpenWithStatus(path)
	if err != nil {
		t.Fatalf("OpenWithStatus: %v", err)
	}
	defer func() { _ = s.Close() }()
	if !st.Fresh {
		t.Error("Fresh = false on a missing file, want true (fresh init)")
	}
	if st.FromVersion != 0 {
		t.Errorf("FromVersion = %d, want 0 (no file existed)", st.FromVersion)
	}
	if st.ToVersion != schemaVersion {
		t.Errorf("ToVersion = %d, want %d", st.ToVersion, schemaVersion)
	}
	if want := []int{1, 2, 3, 4}; !reflect.DeepEqual(st.Applied, want) {
		t.Errorf("Applied = %v, want %v (whole chain ran)", st.Applied, want)
	}
	if st.Noop {
		t.Error("Noop = true on a fresh init that created and migrated, want false")
	}
	// The MigrateStatus reader serves the same report from memory.
	if got := s.MigrateStatus(); !reflect.DeepEqual(got, st) {
		t.Errorf("MigrateStatus() = %+v, want the Open report %+v", got, st)
	}
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version = %d, %v; want %d", v, err, schemaVersion)
	}
	if err := s.SetSetting("k", "v"); err != nil {
		t.Fatalf("SetSetting on fresh file: %v", err)
	}
}

// TestOpenWithStatusLegacyV1AppliesChain pins generation (b): a legacy
// pre-goose v1 file auto-runs only the pending chain, keeps its rows, and
// reports the detected from-version with the applied remainder.
func TestOpenWithStatusLegacyV1AppliesChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-v1.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(legacyV1Schema); err != nil {
		t.Fatalf("v1 schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO log_entries(ts, level, msg) VALUES(1, 'INFO', 'kept')`); err != nil {
		t.Fatalf("v1 seed: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version=1`); err != nil {
		t.Fatalf("v1 stamp: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}
	s, st, err := OpenWithStatus(path)
	if err != nil {
		t.Fatalf("OpenWithStatus on v1 file: %v (want in-place migration, not rejection)", err)
	}
	defer func() { _ = s.Close() }()
	if st.Fresh {
		t.Error("Fresh = true on an existing v1 file, want false")
	}
	if st.FromVersion != 1 {
		t.Errorf("FromVersion = %d, want 1 (the legacy stamp)", st.FromVersion)
	}
	if want := []int{2, 3, 4}; !reflect.DeepEqual(st.Applied, want) {
		t.Errorf("Applied = %v, want %v (only the pending chain runs)", st.Applied, want)
	}
	if st.Noop {
		t.Error("Noop = true after running three migrations, want false")
	}
	got, err := s.QueryLogs(LogFilter{})
	if err != nil || len(got) != 1 || got[0].Msg != "kept" {
		t.Fatalf("v1 history lost in migration: %+v %v", got, err)
	}
	var maxV int
	if err := s.db.QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&maxV); err != nil || maxV != schemaVersion {
		t.Fatalf("goose MAX(version_id) = %d, %v; want %d", maxV, err, schemaVersion)
	}
}

// TestOpenWithStatusLegacyV4TakeoverThenNoop pins the pre-goose v4 path: the
// first boot baselines the whole chain (a write, so not a no-op) with every
// row intact, and the immediate re-boot is the strict no-op.
func TestOpenWithStatusLegacyV4TakeoverThenNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-v4.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(legacyV4Schema); err != nil {
		t.Fatalf("v4 schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO log_entries(ts, level, msg) VALUES(1, 'INFO', 'kept')`); err != nil {
		t.Fatalf("v4 seed: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version=4`); err != nil {
		t.Fatalf("v4 stamp: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	first, fst, err := OpenWithStatus(path)
	if err != nil {
		t.Fatalf("first OpenWithStatus: %v", err)
	}
	if fst.FromVersion != 4 || fst.ToVersion != schemaVersion {
		t.Errorf("takeover from/to = %d->%d, want 4->%d", fst.FromVersion, fst.ToVersion, schemaVersion)
	}
	if len(fst.Applied) != 0 {
		t.Errorf("takeover Applied = %v, want [] (chain baselined, nothing ran)", fst.Applied)
	}
	if fst.Noop {
		t.Error("takeover Noop = true, want false (baseline seeding wrote)")
	}
	got, err := first.QueryLogs(LogFilter{})
	if err != nil || len(got) != 1 || got[0].Msg != "kept" {
		_ = first.Close()
		t.Fatalf("v4 history lost in takeover: %+v %v", got, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, snd, err := OpenWithStatus(path)
	if err != nil {
		t.Fatalf("second OpenWithStatus: %v", err)
	}
	defer func() { _ = second.Close() }()
	if snd.Fresh || len(snd.Applied) != 0 || !snd.Noop {
		t.Errorf("re-boot status = %+v, want {Fresh:false Applied:[] Noop:true}", snd)
	}
	if snd.Applied == nil {
		t.Error("Applied is nil after a no-op boot, want [] (dashboard encodes null otherwise)")
	}
}

// TestOpenSteadyStateStrictNoop pins generation (d) at the file level: a
// re-boot on a converged file performs zero writes (identical bytes and
// mode) and reports the no-op.
func TestOpenSteadyStateStrictNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steady.db")
	first, _, err := OpenWithStatus(path)
	if err != nil {
		t.Fatalf("first OpenWithStatus: %v", err)
	}
	if err := first.SetSetting("steady", "1"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	beforeHash, beforeMode := fileHash(t, path), fileMode(t, path)
	if beforeMode != 0o600 {
		t.Fatalf("mode = %o, want 600 before the no-op re-boot", beforeMode)
	}

	second, st, err := OpenWithStatus(path)
	if err != nil {
		t.Fatalf("second OpenWithStatus: %v", err)
	}
	if st.Fresh {
		t.Error("Fresh = true on an existing converged file, want false")
	}
	if st.FromVersion != schemaVersion || st.ToVersion != schemaVersion {
		t.Errorf("from/to = %d->%d, want %d->%d", st.FromVersion, st.ToVersion, schemaVersion, schemaVersion)
	}
	if len(st.Applied) != 0 {
		t.Errorf("Applied = %v on a converged file, want []", st.Applied)
	}
	if !st.Noop {
		t.Error("Noop = false on a converged re-boot, want true")
	}
	if got, ok, err := second.GetSetting("steady"); err != nil || !ok || got != "1" {
		t.Errorf("row lost across re-boot: %q %v %v", got, ok, err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := fileHash(t, path); got != beforeHash {
		t.Error("DB bytes changed across a no-op re-boot (want zero writes)")
	}
	if got := fileMode(t, path); got != beforeMode {
		t.Errorf("DB mode = %o across a no-op re-boot, want %o (unchanged)", got, beforeMode)
	}
}
