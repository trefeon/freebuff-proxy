-- 00003_v3_maturity: the v3 maturity columns on tokens. Verbatim the old
-- migrateV2ToV3 ALTERs. This migration only ever RUNS on files baselined
-- below 3 (v1/v2 stamps without the columns — the baseline probe lifts
-- half-migrated files to 3, so the ADD COLUMN never meets an existing
-- column); on every other file it is recorded-applied, never executed.
--
-- +goose Up
ALTER TABLE tokens ADD COLUMN maturity_json TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN streak_blob BLOB;

-- +goose Down
ALTER TABLE tokens DROP COLUMN streak_blob;
ALTER TABLE tokens DROP COLUMN maturity_json;
