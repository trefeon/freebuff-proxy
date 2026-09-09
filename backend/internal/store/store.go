// Package store is the dashboard history backend (ADR-0016): one pure-Go
// SQLite file holding log entries, quota snapshots, maturity events, and
// request records. Leaf package by construction: stdlib + the modernc driver
// only, zero internal imports (see archtest matrix). History is display and
// debug data, never control state: a missing or corrupt file degrades to
// live-only views, and retention runs off the request path.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion guards the on-disk format. v1 held the history tables only
// (log_entries, quota_snapshots, maturity_events, request_records); v2 adds
// the persistence tables (settings, pages_state, sessions_persist, tokens).
// v3 adds the maturity columns (maturity_json, streak_blob) to tokens.
// v4 adds the pool runtime table (pool_state: opaque blobs keyed by stable
// string keys for ledger counters, admissions, bridge usage/survivors and
// burst hits — see pool_persist.go).
// Open migrates older files in place; anything else non-zero is rejected so
// a stale file is ignored instead of mis-parsed (mirrors
// session.storeVersion).
const schemaVersion = 4

const schema = `
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
-- v2 persistence tables (settings, page snapshots, sessions, token meta).
-- Value columns hold raw JSON; the store never interprets them (leaf
-- package: stdlib + the sqlite driver only, zero internal imports).
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
-- v4 pool runtime table (pool_persist.go): opaque blobs keyed by stable
-- string keys (pool/ledger/<sha256hex>, pool/admissions, pool/burst,
-- pool/bridge/usage, pool/bridge/survivors). Value columns hold raw
-- JSON/bytes the pool marshals itself; the store never interprets them
-- (leaf package: stdlib + the sqlite driver only, zero internal imports).
CREATE TABLE IF NOT EXISTS pool_state(
  key TEXT PRIMARY KEY,
  value BLOB NOT NULL DEFAULT x'',
  updated_at INTEGER NOT NULL DEFAULT 0
);
`

// LogEntry is one persisted log record. TS is Unix millis UTC; Fields carries
// the flattened "key=value" pairs exactly as logring renders them.
type LogEntry struct {
	ID     int64
	TS     int64
	Level  string
	Msg    string
	Fields string
	ReqID  string
}

// QuotaSnapshot is one per-model session quota sample. Entitlements rides as
// the raw upstream JSON object (nil-friendly: "" when absent).
type QuotaSnapshot struct {
	ID           int64
	TS           int64
	TokenIdx     int
	Model        string
	Limit        float64
	Recent       float64
	ResetAt      int64
	Entitlements string
}

// MaturityEvent is one streak/standing transition worth keeping across restarts.
type MaturityEvent struct {
	ID       int64
	TS       int64
	TokenIdx int
	Kind     string
	Detail   string
}

// RequestRecord is one /v1 inference outcome for the Logs console view.
type RequestRecord struct {
	ReqID    string
	TS       int64
	Endpoint string
	Model    string
	TokenIdx int
	Status   string
	TTFBms   int64
	Err      string
}

// LogFilter selects log rows. Zero values mean "no constraint"; Limit <= 0
// defaults, and is capped, by the query.
type LogFilter struct {
	Since    int64
	Until    int64
	Level    string
	Contains string
	ReqID    string
	Limit    int
}

// Millis converts a time to the Unix-millis domain every TS field uses.
func Millis(t time.Time) int64 { return t.UnixMilli() }

// Store wraps the history database. Zero value is unusable; Open first.
type Store struct {
	db *sql.DB
}

// defaultDBPath is the DB file used when DB_PATH is unset: ./data/freebuff.db
// relative to the process working directory (mirrored by the compose
// db_data volume at /app/data/freebuff.db inside Docker).
const defaultDBPath = "data/freebuff.db"

// DBPathFromEnv resolves the SQLite file: DB_PATH wins (blank counts as
// unset), otherwise the ./data/freebuff.db default. Callers log the resolved
// value so a mis-pointed env is visible at startup.
func DBPathFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("DB_PATH")); v != "" {
		return v
	}
	return filepath.FromSlash(defaultDBPath)
}

// Open creates the parent dir, opens (or creates) the SQLite file at path,
// and applies pragmas + schema. Older files migrate in place (v1/v3 gain
// their missing tables via the IF NOT EXISTS schema; v2 additionally gains
// the v3 maturity columns via ALTER); any other version mismatch or
// unusable file returns an error and the caller runs live-only. Open never
// fails the boot itself.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: %s: %w", p, err)
		}
	}
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: user_version: %w", err)
	}
	migrate := false
	switch v {
	case 0, schemaVersion:
		// Fresh file or current: apply the schema as-is.
	case 1, 3:
		// v1 -> v4 / v3 -> v4: the schema below is IF NOT EXISTS, so it
		// only adds the missing tables (persistence tables for v1,
		// pool_state for v3) and keeps every existing row. A v1 tokens
		// table is created fresh by that same schema, already carrying
		// the v3 maturity columns.
	case 2:
		// v2 -> v4: tokens exists without the maturity columns.
		migrate = true
	default:
		_ = db.Close()
		return nil, fmt.Errorf("store: schema v%d unsupported (want v%d)", v, schemaVersion)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	if migrate {
		if err := migrateV2ToV3(db); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: stamp version: %w", err)
	}
	return &Store{db: db}, nil
}

// migrateV2ToV3 adds the maturity columns to an existing v2 tokens table.
// Column presence is probed first so a half-migrated file stays idempotent.
func migrateV2ToV3(db *sql.DB) error {
	cols := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(tokens)`)
	if err != nil {
		return fmt.Errorf("store: migrate v2->v3 table_info: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt interface{}
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("store: migrate v2->v3 scan: %w", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: migrate v2->v3 rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: migrate v2->v3 close: %w", err)
	}
	for _, alter := range []struct {
		col string
		ddl string
	}{
		{"maturity_json", `ALTER TABLE tokens ADD COLUMN maturity_json TEXT NOT NULL DEFAULT ''`},
		{"streak_blob", `ALTER TABLE tokens ADD COLUMN streak_blob BLOB`},
	} {
		if cols[alter.col] {
			continue
		}
		if _, err := db.Exec(alter.ddl); err != nil {
			return fmt.Errorf("store: migrate v2->v3 add %s: %w", alter.col, err)
		}
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Purge deletes rows older than the given Unix-millis cutoffs. Retention runs
// from a background goroutine the server wires later — never on the request
// path (ADR-0016). No VACUUM: freed pages are reused by later inserts.
func (s *Store) Purge(logsBefore, quotaBefore, maturityBefore, requestsBefore int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: purge begin: %w", err)
	}
	cuts := []struct {
		table string
		ts    int64
	}{
		{"log_entries", logsBefore},
		{"quota_snapshots", quotaBefore},
		{"maturity_events", maturityBefore},
		{"request_records", requestsBefore},
	}
	for _, c := range cuts {
		if _, err := tx.Exec("DELETE FROM "+c.table+" WHERE ts < ?", c.ts); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: purge %s: %w", c.table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: purge commit: %w", err)
	}
	return nil
}
