-- 00004_v4_pool_state: the v4 pool runtime table (pool_persist.go): opaque
-- blobs keyed by stable string keys (pool/ledger/<sha256hex>,
-- pool/admissions, pool/burst, pool/bridge/usage, pool/bridge/survivors).
-- Value columns hold raw JSON/bytes the pool marshals itself; the store
-- never interprets them (leaf package: stdlib + the sqlite driver only,
-- zero internal imports).
--
-- +goose Up
CREATE TABLE IF NOT EXISTS pool_state(
  key TEXT PRIMARY KEY,
  value BLOB NOT NULL DEFAULT x'',
  updated_at INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE IF EXISTS pool_state;
