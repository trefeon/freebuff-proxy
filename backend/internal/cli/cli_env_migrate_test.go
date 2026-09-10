package cli

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"freebuff-proxy/backend/internal/config"
	history "freebuff-proxy/backend/internal/store"

	_ "modernc.org/sqlite"
)

// migrateTestConfig loads an env-pinned effective config in an isolated
// directory (no repo .env leaks in) with CLI discovery disabled.
func migrateTestConfig(t *testing.T) config.Config {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("AUTO_DISCOVER_TOKEN", "false")
	t.Setenv("AUTH_TOKENS", "fb-test-fake-token-1")
	t.Setenv("ADMIN_TOKEN", "fb-test-fake-admin-1")
	t.Setenv("LOG_LEVEL", "debug")
	cfg, err := config.LoadOpts("", config.LoadOptions{})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	return cfg
}

func migrateTestStore(t *testing.T) *history.Store {
	t.Helper()
	st, err := history.Open(filepath.Join(t.TempDir(), "migrate.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestMigrateEnvToDBFreshImport pins the first boot: every catalog key lands
// as a config: row plus the marker, and the second boot imports nothing.
func TestMigrateEnvToDBFreshImport(t *testing.T) {
	st := migrateTestStore(t)
	cfg := migrateTestConfig(t)

	n, err := migrateEnvToDB(st, cfg)
	if err != nil {
		t.Fatalf("migrateEnvToDB: %v", err)
	}
	if n != len(config.Catalog()) {
		t.Errorf("imported %d rows, want %d (every catalog key)", n, len(config.Catalog()))
	}
	rows, err := st.ListSettings()
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	for _, def := range config.Catalog() {
		if _, ok := rows[config.OverlayRowKey(def.Key)]; !ok {
			t.Errorf("missing row %s after migration", config.OverlayRowKey(def.Key))
		}
	}
	if rows[config.MigrationMarkerRow] != config.MigrationMarkerValue {
		t.Errorf("marker row = %q, want %q", rows[config.MigrationMarkerRow], config.MigrationMarkerValue)
	}
	// The migrated secret rows carry the env-pinned values (fake only).
	if rows[config.OverlayRowKey("AUTH_TOKENS")] != "fb-test-fake-token-1" {
		t.Errorf("AUTH_TOKENS row = %q, want the migrated pool", rows[config.OverlayRowKey("AUTH_TOKENS")])
	}
	if rows[config.OverlayRowKey("ADMIN_TOKEN")] != "fb-test-fake-admin-1" {
		t.Error("ADMIN_TOKEN row missing the migrated credential")
	}

	// Second boot is a no-op: nothing imported, rows untouched.
	n, err = migrateEnvToDB(st, cfg)
	if err != nil {
		t.Fatalf("second migrateEnvToDB: %v", err)
	}
	if n != 0 {
		t.Errorf("second boot imported %d rows, want 0 (marker no-op)", n)
	}
	after, err := st.ListSettings()
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	if len(after) != len(rows) {
		t.Errorf("second boot changed row count %d -> %d, want no change", len(rows), len(after))
	}
}

// TestMigrateEnvToDBLegacyV4ThenImport pins the upgrade path: a pre-goose v4
// file (user_version=4, persistence tables, no marker) opens through the
// goose baseline with its rows intact, then imports the env config.
func TestMigrateEnvToDBLegacyV4ThenImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-v4.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	// Minimal v4 shape: the settings table plus the pool_state table 00004
	// adds (probes lift the baseline), stamped user_version=4, one
	// pre-existing control row that must survive.
	for _, ddl := range []string{
		`CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE pool_state(key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL DEFAULT 0)`,
		`INSERT INTO settings(key, value, updated_at) VALUES('ui/theme', 'dark', 1)`,
		`PRAGMA user_version=4`,
	} {
		if _, err := raw.Exec(ddl); err != nil {
			t.Fatalf("legacy v4 seed %q: %v", ddl, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	st, err := history.Open(path)
	if err != nil {
		t.Fatalf("Open on v4 file: %v (want in-place migration, not rejection)", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if v, ok, err := st.GetSetting("ui/theme"); err != nil || !ok || v != "dark" {
		t.Fatalf("legacy row lost in migration: %q %v %v", v, ok, err)
	}

	cfg := migrateTestConfig(t)
	n, err := migrateEnvToDB(st, cfg)
	if err != nil {
		t.Fatalf("migrateEnvToDB on migrated v4: %v", err)
	}
	if n != len(config.Catalog()) {
		t.Errorf("imported %d rows, want %d", n, len(config.Catalog()))
	}
	if v, ok, err := st.GetSetting("ui/theme"); err != nil || !ok || v != "dark" {
		t.Errorf("legacy row lost in import: %q %v %v", v, ok, err)
	}
	if _, ok, err := st.GetSetting(config.MigrationMarkerRow); err != nil || !ok {
		t.Errorf("marker missing after import: %v %v", ok, err)
	}
}

// smartMigrateFileHash snapshots one DB file's bytes after its handle is
// closed: a re-boot that performs zero writes hashes identically.
func smartMigrateFileHash(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// TestSmartMigrateMarkerlessGooseDBImportsEnv pins generation (c) on a
// goose-native file: a fresh OpenWithStatus converges with no marker, the
// env import lands every catalog key plus the marker, and the next boot
// imports nothing.
func TestSmartMigrateMarkerlessGooseDBImportsEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goose-native.db")
	st, ms, err := history.OpenWithStatus(path)
	if err != nil {
		t.Fatalf("OpenWithStatus: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if !ms.Fresh || ms.FromVersion != 0 {
		t.Errorf("fresh status = %+v, want {Fresh:true FromVersion:0}", ms)
	}
	if _, ok, err := st.GetSetting(config.MigrationMarkerRow); err != nil || ok {
		t.Fatalf("marker present before import: %v %v", ok, err)
	}
	cfg := migrateTestConfig(t)
	n, err := migrateEnvToDB(st, cfg)
	if err != nil {
		t.Fatalf("migrateEnvToDB: %v", err)
	}
	if n != len(config.Catalog()) {
		t.Errorf("imported %d rows, want %d (every catalog key)", n, len(config.Catalog()))
	}
	if v, ok, err := st.GetSetting(config.MigrationMarkerRow); err != nil || !ok || v != config.MigrationMarkerValue {
		t.Errorf("marker after import = %q %v %v, want %q", v, ok, err, config.MigrationMarkerValue)
	}
	if n, err := migrateEnvToDB(st, cfg); err != nil || n != 0 {
		t.Errorf("second migrateEnvToDB = %d, %v; want 0, nil (marker no-op)", n, err)
	}
}

// TestSmartMigrateMarkedLatestZeroWrites pins generation (d) end to end: a
// re-boot on a marked, latest-version file (goose converged + env imported)
// performs zero writes — identical bytes and mode — and reports the no-op.
func TestSmartMigrateMarkedLatestZeroWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marked.db")
	first, _, err := history.OpenWithStatus(path)
	if err != nil {
		t.Fatalf("first OpenWithStatus: %v", err)
	}
	cfg := migrateTestConfig(t)
	if n, err := migrateEnvToDB(first, cfg); err != nil || n == 0 {
		_ = first.Close()
		t.Fatalf("first migrateEnvToDB = %d, %v; want full import", n, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	beforeHash := smartMigrateFileHash(t, path)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600 before the no-op re-boot", got)
	}

	second, ms, err := history.OpenWithStatus(path)
	if err != nil {
		t.Fatalf("second OpenWithStatus: %v", err)
	}
	if ms.Fresh || len(ms.Applied) != 0 || !ms.Noop {
		t.Errorf("re-boot status = %+v, want {Fresh:false Applied:[] Noop:true}", ms)
	}
	if n, err := migrateEnvToDB(second, cfg); err != nil || n != 0 {
		t.Errorf("re-boot migrateEnvToDB = %d, %v; want 0, nil (marker no-op)", n, err)
	}
	if v, ok, err := second.GetSetting(config.OverlayRowKey("AUTH_TOKENS")); err != nil || !ok || v != "fb-test-fake-token-1" {
		t.Errorf("migrated AUTH_TOKENS row = %q %v %v, want the fake pool intact", v, ok, err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := smartMigrateFileHash(t, path); got != beforeHash {
		t.Error("DB bytes changed across a marked latest re-boot (want zero writes)")
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("DB mode changed across a marked latest re-boot: %v %o", err, fi.Mode().Perm())
	}
}

// TestSmartMigrateEnvWinsAfterMigrate pins precedence after the import:
// explicit process env still beats every migrated DB row at runtime, while
// the DB row carries the effective value once the environment is cleared.
func TestSmartMigrateEnvWinsAfterMigrate(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AUTO_DISCOVER_TOKEN", "false")
	t.Setenv("LOG_LEVEL", "debug")
	cfg, err := config.LoadOpts("", config.LoadOptions{})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	st, err := history.Open(filepath.Join(t.TempDir(), "precedence.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if n, err := migrateEnvToDB(st, cfg); err != nil || n == 0 {
		t.Fatalf("migrateEnvToDB = %d, %v; want full import", n, err)
	}
	rows, err := st.ListSettings()
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	ov := config.OverlayFromRows(rows)

	// DB-alone boot: the migrated row provides the effective value.
	if err := os.Unsetenv("LOG_LEVEL"); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	cfgDB, err := config.LoadOpts("", config.LoadOptions{Overlay: ov})
	if err != nil {
		t.Fatalf("LoadOpts from overlay: %v", err)
	}
	if cfgDB.LogLevel != "debug" {
		t.Errorf("DB-alone LOG_LEVEL = %q, want debug (the migrated row)", cfgDB.LogLevel)
	}
	// Env-pinned boot: explicit process env wins over the migrated row.
	t.Setenv("LOG_LEVEL", "warn")
	cfgEnv, err := config.LoadOpts("", config.LoadOptions{Overlay: ov})
	if err != nil {
		t.Fatalf("LoadOpts with env: %v", err)
	}
	if cfgEnv.LogLevel != "warn" {
		t.Errorf("env-pinned LOG_LEVEL = %q, want warn (env wins after migrate)", cfgEnv.LogLevel)
	}
}
