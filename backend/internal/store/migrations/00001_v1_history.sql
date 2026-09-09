-- 00001_v1_history: the original history tables (ADR-0016): log entries,
-- quota snapshots, maturity events, request records. Verbatim v1 DDL: later
-- migrations add the indexes and persistence tables, so a legacy v1 file
-- (user_version=1) baselines here and runs forward with zero data change.
--
-- +goose Up
CREATE TABLE IF NOT EXISTS log_entries(
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  level TEXT NOT NULL,
  msg TEXT NOT NULL,
  fields TEXT NOT NULL DEFAULT '',
  req_id TEXT NOT NULL DEFAULT ''
);
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
CREATE TABLE IF NOT EXISTS maturity_events(
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  token_idx INTEGER NOT NULL,
  kind TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT ''
);
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

-- +goose Down
DROP TABLE IF EXISTS request_records;
DROP TABLE IF EXISTS maturity_events;
DROP TABLE IF EXISTS quota_snapshots;
DROP TABLE IF EXISTS log_entries;
