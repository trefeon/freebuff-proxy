package cli

import (
	"database/sql"
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
