-- 00002_v2_persist: the v2 persistence tables (settings, page snapshots,
-- sessions, token meta) plus the history lookup indexes. The indexes ride
-- this migration (not 00001) because v1 files never had them: the old
-- hand-rolled chain applied the full IF NOT EXISTS schema, so a migrated v1
-- file gained them here. The tokens table is verbatim v2 (no maturity
-- columns); 00003 adds those. A legacy v2 file (user_version=2) baselines
-- here and every statement is a no-op for it.
--
-- +goose Up
CREATE INDEX IF NOT EXISTS idx_log_ts ON log_entries(ts);
CREATE INDEX IF NOT EXISTS idx_log_level_ts ON log_entries(level, ts);
CREATE INDEX IF NOT EXISTS idx_quota_lookup ON quota_snapshots(token_idx, model, ts);
CREATE INDEX IF NOT EXISTS idx_maturity_lookup ON maturity_events(token_idx, ts);
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
  created_at INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE IF EXISTS tokens;
DROP TABLE IF EXISTS sessions_persist;
DROP TABLE IF EXISTS pages_state;
DROP TABLE IF EXISTS settings;
DROP INDEX IF EXISTS idx_req_ts;
DROP INDEX IF EXISTS idx_maturity_lookup;
DROP INDEX IF EXISTS idx_quota_lookup;
DROP INDEX IF EXISTS idx_log_level_ts;
DROP INDEX IF EXISTS idx_log_ts;
