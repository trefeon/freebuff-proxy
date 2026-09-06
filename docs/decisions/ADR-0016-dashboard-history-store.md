# ADR-0016 — Dashboard history store (pure-Go SQLite, hot-ring spill)

Status: Accepted

Context: the dashboard renders every view live from pool state (`backend/internal/dashboard/dashboard_data.go`) and keeps logs in a bounded memory ring (`backend/internal/logring`). Restarts wipe quota history, maturity events, and log context, which makes recurring pool failures undebuggable. The 7 pages also need filtered range queries (by token, model, level, time) that in-memory views cannot serve.

Decision: add `modernc.org/sqlite` (pure-Go `database/sql` driver) as the single history store. Four tables: `log_entries`, `quota_snapshots`, `maturity_events`, `request_records`. `logring` stays the hot path; one async spiller batch-writes to `log_entries` (WAL mode, `synchronous=NORMAL`). Retention is a rolling delete (`DELETE ... WHERE ts < cutoff`, no `VACUUM` on the hot path): 7d logs, 30d quota/maturity, all behind config knobs. Existing admin routes keep paths and shapes (`admin_manifest.json` parity untouched); history arrives as query params (`since/until/level/token`) plus two new rows (`/admin/api/quota/history`, `/admin/api/maturity/history`).

Reasoning: `.goreleaser.yml` pins `CGO_ENABLED=0` across 3 OS x 2 arch, so `mattn/go-sqlite3` cannot ship. KV stores (bbolt/Pebble) were rejected because the dashboard needs multi-criteria range queries; hand-rolled secondary indexes on KV repeat what SQL provides. The store absorbs the `session/store.go` JSON-file shape over time instead of adding a second persistence story.

Alternatives considered: bbolt (simpler, but every filtered view needs a custom index); append-only JSONL log files (fast writes, but the Logs page needs level/message/req_id filtering without full scans); no store (keeps the restart-amnesia that motivated this ADR).

Consequences: first non-stdlib storage dependency (`go.mod` +1 direct). Schema changes bump a `PRAGMA user_version` guard, mirroring `session.storeVersion`. Spiller lag is observable via a `dashboard_store_lag_seconds` gauge. Page rewires (Overview through Settings) consume the query service one slice at a time; no page reads SQL directly.

Invariants: endpoint paths/shapes stay manifest-stable; secrets never land in history tables (redaction from effective-config views applies); retention deletes run outside request handlers; a corrupt store file degrades to live-only views, never a failed boot.

Affected packages: new `internal/store` (owns schema, spill, retention, queries); `internal/dashboard` (query service + two history routes); `internal/logring` (spill hook).

Related tests: store round-trip + retention tests (new package); `TestAdminRoutesParity` stays green throughout; per-page slices pin their history queries.
