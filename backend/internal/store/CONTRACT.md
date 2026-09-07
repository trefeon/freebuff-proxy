# Package Contract: `backend/internal/store`

Task-local contract for agents modifying this package. Load before editing any file here.

## Purpose

Unified dashboard persistence backend (ADR-0016, extended): one pure-Go
SQLite file holding history views (log entries, quota snapshots, maturity
events, request records) PLUS durable control state (settings, page
snapshots, persisted sessions, token meta). History stays display and debug
data; the v2 tables are restart-surviving state the dashboard and pool
consult at boot. The dashboard query service (added in a later slice)
merges these views with live pool state; no page reads SQL directly.

## File layout

- `store.go`: `Open` (pragmas + schema + v1→v2 migrate), `Close`,
  `DBPathFromEnv` (`DB_PATH`, default `./data/freebuff.db`).
- `history.go`: history writes/queries (`AppendLogs`, `RecordQuota`,
  `RecordMaturity`, `RecordRequest`, `QueryLogs`, `QuotaHistory`,
  `MaturityHistory`).
- `settings.go`: `SetSetting` / `GetSetting` / `DeleteSetting` (missing-row delete is a no-op) / `ListSettings` (key-ordered map; callers filter their namespace, e.g. the `config:` overlay prefix owned by `internal/config`).
- `pages.go`: `PutPageState` / `GetPageState` (page_id → raw JSON snapshot).
- `sessions.go`: `SaveSession` / `LoadSession` / `SaveSessionRuns` /
  `DeleteSession` (token-hash-keyed opaque blobs mirroring
  `session.Store` semantics) + `ImportLegacySessionFile` (legacy
  `.freebuff-session-state.json` import with `.bak` archive).
- `tokens.go`: `TokenMeta` + `UpsertTokenMeta` / `GetTokenMeta` /
  `ListTokenMetas` / `DeleteTokenMeta` (value-hash-keyed, raw hashes only).

## Allowed dependencies

Stdlib + `modernc.org/sqlite` driver only (archtest matrix: zero internal
imports). `CGO_ENABLED=0` must keep working on all 3 OS x 2 arch targets.

## Forbidden dependencies

Every internal package. The store defines its own record types and keeps
value columns as opaque JSON; callers map (`logring.Entry` → `LogEntry`
at the spill site, `session` state → blobs at the persist site). No imports
from `server`, `dashboard`, `pool`, `config`, `session`, or `logring` —
the dependency arrow points inward only.

## Critical invariants

- Persistence degrades, never blocks: missing/corrupt/version-mismatched
  files return an error from `Open` and the caller runs live-only. The
  store never fails the boot.
- Schema guard: `user_version` v1 files migrate in place to v2 (the DDL is
  `IF NOT EXISTS`, so v1 history rows survive); any other non-zero version
  rejects loudly, never mis-parses (mirrors `session.storeVersion`).
- Retention runs off the request path (background goroutine wires `Purge`);
  no `VACUUM` on the hot path — freed pages are reused by later inserts.
  `Purge` touches history tables ONLY; settings/pages/sessions/tokens are
  control state and are never purged.
- All timestamps are Unix millis UTC (`Millis`).
- `RecordRequest` upserts: one `req_id` keeps its latest row.
- User filter text never reaches LIKE unescaped (`escapeLike`).
- Raw tokens never touch disk: session/token rows are keyed by SHA-256
  hashes computed by the caller. Empty keys/hashes are rejected with an
  error, never stored.
- Absent rows report `ok=false` with a nil error; real DB failures return
  a wrapped error (callers decide degrade-vs-warn, never the store).
- `SaveSession` with two empty blobs deletes the row (mirrors
  `session.Store.Save(nil)`); `UpsertTokenMeta` preserves `CreatedAt`.
- Legacy import archives, never deletes: the source renames to `.bak`
  (a stale `.bak` is replaced first — Windows rename needs it); a missing
  file is a no-op and a parse failure leaves the file in place.

## Tests that protect it

`store_test.go`: fresh-open version stamp, unknown-version rejection,
garbage-file rejection, log round-trip + filters (order, level, LIKE
metacharacters, req_id), quota/maturity ordering + cutoffs, request upsert,
purge freshness boundary. `migrate_test.go`: v1-file in-place migration
(rows kept, version stamped, new tables usable), `DBPathFromEnv`
(env-wins + default). `settings_test.go`, `pages_test.go`,
`tokens_test.go`, `sessions_test.go`: per-API round-trips, overwrite and
delete semantics, empty-key rejection, legacy import (inline temp-file
fixture only — never repo fixtures) + missing-noop + garbage-keeps-file.

## Safe modification patterns

- New table: extend `schema` + add record type + write/query methods + a
  round-trip test in one commit; bump `schemaVersion` with an explicit
  migrate case when an existing table shape changes (old files reject
  loudly, never mis-parse).
- New filter: extend `LogFilter` + one WHERE clause + one test row proving it.
- New JSON-backed table: keep values opaque (`string` in, `string` out);
  parsing belongs to the caller, never to this package.
