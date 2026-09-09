// Package store is the dashboard history backend (ADR-0016): one pure-Go
// SQLite file holding log entries, quota snapshots, maturity events, and
// request records. Leaf package by construction: stdlib + the modernc driver
// + pressly/goose for migrations only, zero internal imports (see archtest
// matrix). History is display and debug data, never control state: a missing
// or corrupt file degrades to live-only views, and retention runs off the
// request path.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"github.com/pressly/goose/v3"
	"io/fs"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// schemaVersion guards the on-disk format. v1 held the history tables only
// (log_entries, quota_snapshots, maturity_events, request_records); v2 adds
// the persistence tables (settings, pages_state, sessions_persist, tokens).
// v3 adds the maturity columns (maturity_json, streak_blob) to tokens.
// v4 adds the pool runtime table (pool_state: opaque blobs keyed by stable
// string keys for ledger counters, admissions, bridge usage/survivors and
// burst hits — see pool_persist.go).
// Open migrates older files in place via the embedded goose migrations
// (migrations/00001..00004, one version per legacy user_version stamp);
// anything else non-zero is rejected so a stale file is ignored instead of
// mis-parsed (mirrors session.storeVersion).
const schemaVersion = 4

// migrationsFS embeds the goose migration chain. Versions are sequential
// 1..schemaVersion on purpose: a legacy file stamped with PRAGMA
// user_version=N baselines migrations 1..N as applied (its objects already
// exist) and runs only the remainder, so every pre-goose file converges
// with zero data change.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// gooseVersionDDL is pressly/goose v3.28.0's sqlite version-table DDL
// (internal/dialects/sqlite3.go CreateTable): the legacy baseline path
// creates it before seeding stamps so goose's own ensure sees the identical
// table it would have created itself.
const gooseVersionDDL = `CREATE TABLE IF NOT EXISTS goose_db_version (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	version_id INTEGER NOT NULL,
	is_applied INTEGER NOT NULL,
	tstamp TIMESTAMP DEFAULT (datetime('now'))
)`

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
// and applies pragmas + the goose migration chain. Older files migrate in
// place with zero data change (a legacy user_version stamp baselines the
// matching migrations as applied; only the remainder runs); any other
// version mismatch or unusable file returns an error and the caller runs
// live-only. Open never fails the boot itself.
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
	switch v {
	case 0, 1, 2, 3, schemaVersion:
		// Fresh file or a supported legacy stamp: goose converges it.
	default:
		_ = db.Close()
		return nil, fmt.Errorf("store: schema v%d unsupported (want v%d)", v, schemaVersion)
	}
	if err := migrateUp(db, v); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: stamp version: %w", err)
	}
	return &Store{db: db}, nil
}

// migrateUp brings any supported file to the latest schema via the embedded
// goose migrations. Pre-goose files (user_version 1..4) carry no version
// rows, so versions at or below the baseline are recorded as applied without
// running — their objects already exist — and only the remainder executes.
// Every row is preserved; only DDL runs.
func migrateUp(db *sql.DB, legacy int) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("store: migrations fs: %w", err)
	}
	provider, err := goose.NewProvider(
		goose.DialectSQLite3,
		db,
		sub,
		goose.WithDisableGlobalRegistry(true),
		goose.WithLogger(goose.NopLogger()),
	)
	if err != nil {
		return fmt.Errorf("store: goose provider: %w", err)
	}
	if base := baselineVersion(db, legacy); base > 0 {
		if _, err := db.Exec(gooseVersionDDL); err != nil {
			return fmt.Errorf("store: goose version table: %w", err)
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version`).Scan(&n); err != nil {
			return fmt.Errorf("store: goose version count: %w", err)
		}
		if n == 0 {
			// Seed 0..base: goose's own convention inserts a zero version
			// on creation, and its sqlite ensure falls back to reading
			// version 0 when the table-exists check is unsupported — the
			// row keeps that fallback from attempting a CREATE over the
			// table just made above.
			for v := 0; v <= base; v++ {
				if _, err := db.Exec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, v); err != nil {
					return fmt.Errorf("store: baseline goose v%d: %w", v, err)
				}
			}
		}
	}
	if _, err := provider.Up(context.Background()); err != nil {
		return fmt.Errorf("store: migrate up: %w", err)
	}
	return nil
}

// baselineVersion maps a legacy PRAGMA user_version stamp to the goose
// baseline: versions at or below it are recorded as applied without running.
// Probes lift the baseline when the file already carries later objects — a
// half-migrated v2 file whose ALTER landed before the old chain stamped, or
// a hand-made unstamped file — so Up never replays a DDL the file has.
func baselineVersion(db *sql.DB, legacy int) int {
	base := legacy
	if base < 3 && hasColumn(db, "tokens", "maturity_json") {
		base = 3
	}
	if base < 4 && hasTable(db, "pool_state") {
		base = 4
	}
	return base
}

// hasTable reports whether a table exists in the file.
func hasTable(db *sql.DB, table string) bool {
	var one int
	return db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&one) == nil
}

// hasColumn reports whether a table carries a column (false when the table
// itself is missing: table_info on a missing table returns zero rows).
func hasColumn(db *sql.DB, table, column string) bool {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if name == column {
			_ = rows.Close()
			return true
		}
	}
	return false
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
