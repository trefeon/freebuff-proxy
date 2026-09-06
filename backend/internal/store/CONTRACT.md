# Package Contract: `backend/internal/store`

Task-local contract for agents modifying this package. Load before editing any file here.

## Purpose

Dashboard history backend (ADR-0016): one pure-Go SQLite file holding log
entries, quota snapshots, maturity events, and request records. Display and
debug data only, never control state. The dashboard query service (added in a
later slice) merges these views with live pool state; no page reads SQL
directly.

## Allowed dependencies

Stdlib + `modernc.org/sqlite` driver only (archtest matrix: zero internal
imports). `CGO_ENABLED=0` must keep working on all 3 OS x 2 arch targets.

## Forbidden dependencies

Every internal package. The store defines its own record types; callers map
(`logring.Entry` → `LogEntry` at the spill site). No imports from `server`,
`dashboard`, `pool`, `config`, or `logring` — the dependency arrow points
inward only.

## Critical invariants

- History degrades, never blocks: missing/corrupt/version-mismatched files
  return an error from `Open` and the caller runs live-only. The store never
  fails the boot.
- Retention runs off the request path (background goroutine wires `Purge`);
  no `VACUUM` on the hot path — freed pages are reused by later inserts.
- All timestamps are Unix millis UTC (`Millis`); `user_version` guards the
  schema (bump + mismatch-reject, mirroring `session.storeVersion`).
- `RecordRequest` upserts: one `req_id` keeps its latest row.
- User filter text never reaches LIKE unescaped (`escapeLike`).

## Tests that protect it

`store_test.go`: fresh-open version stamp, unknown-version rejection,
garbage-file rejection, log round-trip + filters (order, level, LIKE
metacharacters, req_id), quota/maturity ordering + cutoffs, request upsert,
purge freshness boundary.

## Safe modification patterns

- New table: extend `schema` + add record type + write/query methods + a
  round-trip test in one commit; bump `schemaVersion` when an existing
  table shape changes (old files reject loudly, never mis-parse).
- New filter: extend `LogFilter` + one WHERE clause + one test row proving it.
