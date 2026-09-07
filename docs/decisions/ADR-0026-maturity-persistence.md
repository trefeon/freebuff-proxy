# ADR-0026 — Maturity automation persistence + release/relock lifecycle

Date: 2026-09-08. Status: Accepted.

## Context

Maturity automation lived entirely in pool memory: a restart wiped
enabled/target/mode/touch-model, the day's slot, the 3-day no-advance
warning, and the streak cache — every deploy re-armed warming tokens from
zero and re-rolled slots. Two lifecycle holes compounded it:

1. A token that hit its target auto-released and forgot it: a later streak
   lapse looked identical to never-enrolled, so nobody re-warmed it.
2. The no-advance warning, once raised, could only be cleared by
   re-saving the whole card (which also resets slot-adjacent state).

## Decision

- Persist per-token automation state in the dashboard DB `tokens` table
  (`maturity_json` TEXT blob + `streak_blob` BLOB, schema v3 with v2→v3
  ALTER migration). Rows are keyed by token SHA-256 hash; raw tokens
  never touch disk (same rule as the session store).
- Boundary: the pool exposes a `MaturityStore` callback interface
  (`SetMaturityStore` + `RestoreMaturity`); CLI adapts `*history.Store`
  through a nil-safe `poolMaturityStore` and restores once at boot,
  warn-only. The pool never imports the store (archtest leaf rule);
  boot failure keeps the in-memory path, never fails startup.
- Blob shape (`maturityPersisted`) is JSON-stable, snake_case, additive:
  old rows unmarshal after new counters land; corrupt rows warn and
  stay never-enrolled.
- Release stamps `released_target`; a released token whose streak sits
  below target for 2 consecutive days (`below_target_days` in the blob,
  so restarts never reset the episode) re-locks for warming. One bad
  day is noise; recovery resets the counter. Flagged accounts
  (quarantine/ban/cooling/country-block) never re-lock, and a manual
  disable clears the watch so it never re-locks behind the operator.
- `ClearMaturityWarn` + `POST /admin/tokens/{id}/maturity/warn-reset`
  (manifest row, additive) resets only the warn loop — config untouched —
  with a Reset warning button on warned cards.

## Consequences

Restarts preserve warming progress, slots, warnings, and relock
episodes. Re-enabled warming tokens re-lock out of rotation on restore,
mirroring `SetMaturity`. Worst-case added DB load: one row write per
token per maintain tick plus one row per mutation.

## Invariants

- No new pool→store or dashboard→server import edge (pinned by
  archtest + the per-file import proof in the Wave-B report).
- `MATURITY_TOUCH_MODEL` stays the visible global default; per-token
  override falls back to it when empty (ADR-0021 unchanged).
- Slot seeds follow the account timezone, never UTC; the Pacific
  fallback inside `maturityLocation` is preserved, not extended.

## Affected packages

`pool` (state, tick, relock, callbacks), `store` (schema v3, token
maturity rows), `cli` (adapter + boot restore), `server` (warn-reset
endpoint), `dashboard` (touch_model card field, manifest row),
`config` (visible touch-model key).

## Related tests

`maturity_persist_test.go` (restart restore, relock, warn re-arm),
`tokens_test.go` (v2→v3 migration, blob round-trip),
`dashboard_maturity_test.go` (touch_model card round-trip),
`admin_maturity_test.go` (warn-reset route), `maturity_store_test.go`
(boot adapter + warn-only restore).
