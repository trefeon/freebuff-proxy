package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// legacyV1Schema is the exact v1 table set (store.go before the v2
// persistence tables). A v1 file crafted from it must open cleanly and
// migrate: history rows preserved, new tables created, version stamped.
const legacyV1Schema = `
CREATE TABLE log_entries(
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  level TEXT NOT NULL,
  msg TEXT NOT NULL,
  fields TEXT NOT NULL DEFAULT '',
  req_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE quota_snapshots(
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  token_idx INTEGER NOT NULL,
  model TEXT NOT NULL,
  quota_limit REAL NOT NULL,
  recent_count REAL NOT NULL,
  reset_at INTEGER NOT NULL DEFAULT 0,
  entitlements TEXT NOT NULL DEFAULT ''
);
CREATE TABLE maturity_events(
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  token_idx INTEGER NOT NULL,
  kind TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT ''
);
CREATE TABLE request_records(
  req_id TEXT PRIMARY KEY,
  ts INTEGER NOT NULL,
  endpoint TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT '',
  token_idx INTEGER NOT NULL DEFAULT -1,
  status TEXT NOT NULL DEFAULT '',
  ttfb_ms INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT ''
);
`

func TestOpenMigratesV1ToV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
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

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on v1 file: %v (want in-place migration, not rejection)", err)
	}
	defer func() { _ = s.Close() }()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	got, err := s.QueryLogs(LogFilter{})
	if err != nil {
		t.Fatalf("QueryLogs after migrate: %v", err)
	}
	if len(got) != 1 || got[0].Msg != "kept" {
		t.Fatalf("v1 history lost in migration: %+v", got)
	}
	// New tables must be usable on the migrated file.
	if err := s.SetSetting("k", "v"); err != nil {
		t.Fatalf("SetSetting after migrate: %v", err)
	}
}

func TestDBPathFromEnv(t *testing.T) {
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "custom.db"))
	if got, want := DBPathFromEnv(), os.Getenv("DB_PATH"); got != want {
		t.Fatalf("DBPathFromEnv = %q, want DB_PATH %q", got, want)
	}
}

func TestDBPathDefault(t *testing.T) {
	t.Setenv("DB_PATH", "")
	// os.Unsetenv: t.Setenv("") leaves an empty (unset-equivalent) value;
	// resolve explicitly to prove the default does not depend on env.
	if err := os.Unsetenv("DB_PATH"); err != nil {
		t.Fatalf("unset: %v", err)
	}
	if got, want := DBPathFromEnv(), filepath.Join("data", "freebuff.db"); got != want {
		t.Fatalf("DBPathFromEnv default = %q, want %q", got, want)
	}
}
