# Package Contract: `backend/internal/session`

Task-local contract for agents modifying this package. Load before editing any file here.

## Purpose

Per-token upstream admission state: create, refresh, poll, and end sessions.
Owns session managers, the shared persistent `Store`, poll-404 recovery,
model-lock routing, quota restore, and the admission caps. Pool orchestrates
tokens; session owns the handshake mechanics.

## Public API (stable surface)

- Types: `session_types.go` (session state, tiers, model locks).
- Lifecycle: `session.go` (manager), `session_admission.go` (create/refresh),
  `session_poll.go` (status polls), `session_commit.go` (state commits).
- Persistence: `store.go` (`Store`: token-hash-keyed session + run state,
  0600 on disk; never raw tokens). `SetSessionStore` injection is required by
  BOTH pooled and bridge run managers (post-boot bridge entries included).
- Admission control: global + per-model concurrent-admission caps (wait-or-503);
  `SESSION_PROBE_CACHE_TTL` reuses poll state; `SESSION_RE_ADMIT_LEAD`
  refreshes ahead of expiry while the request rides the old session.

## Allowed dependencies

`telemetry`, `upstream` (archtest matrix). Tests additionally use `testutil`,
`upstream/testmock`.

## Forbidden dependencies

`pool` (would be a cycle), `server`, `convert`, `dashboard`, `registry`,
`modelcat`, `config`, `runs`, stealth, `ratelimit`, `logring`,
`tokenestimate`, `reasoningcache`. Session state must not know about tokens
in the pool sense; pool drives session from above.

## Critical invariants

- One session per token at a time; refreshes are single-flighted (no
  admission stampedes under tool-calling loops).
- Poll-404 / expired sessions recreate cleanly; never serve a request on a
  dead session.
- `SESSION_PROBE_CACHE_TTL=0` disables poll reuse; `SESSION_RE_ADMIT_LEAD`
  must not block the request (refresh runs in the background).
- Model-lock recovery is DELETE → re-POST; locked models pin to their session.
- `SetSessionStore` injection must reach bridge entries created after boot,
  not just pooled ones (PR #348 pattern).
- Store keys are token SHA-256 hashes; raw tokens never touch disk.

## Tests that protect it

`session_lifecycle_test.go`, `session_admission_test.go`,
`session_quota_restore_test.go`, `session_commit_test.go`,
`session_persist_test.go`, `store_test.go`, `glm_guard_test.go`,
`session_handling_test.go` (in pool, cross-layer).

## Safe modification patterns

- New upstream session field: parse tolerantly (compact polls omit quota
  fields), add to `session_types.go`, extend the persist schema with a
  version bump; zero-value tolerance in `Validate`-style checks.
- New admission knob: default must preserve existing behavior; thread through
  config, never env reads here (config is a bottom layer).
- After editing struct literals: `git diff | grep '^-[^-]'` — confirm every
  removed line has an intended re-add (missing fields compile as zero values).