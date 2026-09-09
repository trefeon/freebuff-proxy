package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// legacyV4Schema is the verbatim pre-goose v4 DDL (the schema const Open
// applied before the goose migration chain). A production v4 file carries
// exactly these objects stamped user_version=4 with no goose rows; Open must
// take it over unmodified: every row preserved, version restamped, goose
// baselined at 4 with nothing left to run.
const legacyV4Schema = `
CREATE TABLE IF NOT EXISTS log_entries(
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  level TEXT NOT NULL,
  msg TEXT NOT NULL,
  fields TEXT NOT NULL DEFAULT '',
  req_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_log_ts ON log_entries(ts);
CREATE INDEX IF NOT EXISTS idx_log_level_ts ON log_entries(level, ts);
CREATE TABLE IF NOT EXISTS quota_snapshots(
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  token_idx INTEGER NOT NULL,
  model TEXT NOT NULL,
  quota_limit REAL NOT NULL,
  recent_count REAL NOT NULL,
  reset_at INTEGER NOT NULL DEFAULT 0,
  entitlements TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_quota_lookup ON quota_snapshots(token_idx, model, ts);
CREATE TABLE IF NOT EXISTS maturity_events(
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  token_idx INTEGER NOT NULL,
  kind TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_maturity_lookup ON maturity_events(token_idx, ts);
CREATE TABLE IF NOT EXISTS request_records(
  req_id TEXT PRIMARY KEY,
  ts INTEGER NOT NULL,
  endpoint TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT '',
  token_idx INTEGER NOT NULL DEFAULT -1,
  status TEXT NOT NULL DEFAULT '',
  ttfb_ms INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_req_ts ON request_records(ts);
CREATE TABLE IF NOT EXISTS settings(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS pages_state(
  page_id TEXT PRIMARY KEY,
  data TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS sessions_persist(
  id INTEGER PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  session_data TEXT NOT NULL DEFAULT '',
  runs_data TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sessions_token ON sessions_persist(token_hash);
CREATE TABLE IF NOT EXISTS tokens(
  id INTEGER PRIMARY KEY,
  value_hash TEXT NOT NULL UNIQUE,
  label TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '',
  quota_data TEXT NOT NULL DEFAULT '',
  maturity_json TEXT NOT NULL DEFAULT '',
  streak_blob BLOB,
  created_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS pool_state(
  key TEXT PRIMARY KEY,
  value BLOB NOT NULL DEFAULT x'',
  updated_at INTEGER NOT NULL DEFAULT 0
);
`

func TestOpenLegacyV4File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(legacyV4Schema); err != nil {
		t.Fatalf("v4 schema: %v", err)
	}
	for _, seed := range []string{
		`INSERT INTO log_entries(ts, level, msg) VALUES(7, 'INFO', 'kept')`,
		`INSERT INTO settings(key, value) VALUES('seed', 'kept')`,
		`INSERT INTO tokens(value_hash, label, maturity_json) VALUES('abc', 'kept', '{"enabled":true}')`,
		`INSERT INTO pool_state(key, value) VALUES('pool/burst', '{"m":[]}')`,
	} {
		if _, err := raw.Exec(seed); err != nil {
			t.Fatalf("v4 seed: %v", err)
		}
	}
	if _, err := raw.Exec(`PRAGMA user_version=4`); err != nil {
		t.Fatalf("v4 stamp: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on v4 file: %v (want takeover, not rejection)", err)
	}
	defer func() { _ = s.Close() }()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	// Every seeded row survives the takeover.
	if got, err := s.QueryLogs(LogFilter{}); err != nil || len(got) != 1 || got[0].Msg != "kept" {
		t.Fatalf("v4 log row lost: %+v %v", got, err)
	}
	if seed, ok, err := s.GetSetting("seed"); err != nil || !ok || seed != "kept" {
		t.Fatalf("v4 settings row lost: %q %v %v", seed, ok, err)
	}
	if _, _, ok, err := s.LoadTokenMaturity("abc"); err != nil || !ok {
		t.Fatalf("v4 token row lost: ok=%v err=%v", ok, err)
	}
	if got, ok, err := s.LoadPoolState("pool/burst"); err != nil || !ok || string(got) != `{"m":[]}` {
		t.Fatalf("v4 pool_state row lost: %q ok=%v err=%v", got, ok, err)
	}
	// Goose baselined the whole chain: nothing pending, MAX at 4.
	var maxV int
	if err := s.db.QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&maxV); err != nil {
		t.Fatalf("goose version: %v", err)
	}
	if maxV != schemaVersion {
		t.Fatalf("goose MAX(version_id) = %d, want %d", maxV, schemaVersion)
	}
	// Writes land on the taken-over file and a second Open keeps them.
	if err := s.SetSetting("fresh", "1"); err != nil {
		t.Fatalf("SetSetting after takeover: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer func() { _ = r.Close() }()
	if got, ok, err := r.GetSetting("fresh"); err != nil || !ok || got != "1" {
		t.Fatalf("after re-Open: got %q ok=%v err=%v", got, ok, err)
	}
}
