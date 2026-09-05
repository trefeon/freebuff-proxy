# Package Contract: `backend/internal/runs`

Task-local contract for agents modifying this package. Load before editing any file here.

## Purpose

Agent-run lifecycle: START/step/rotation, the bounded drain queue, deferred
FINISH workers, and cooldowns (`cooldown.go`, incl. hardban). Pool leases
runs; runs own the execution bookkeeping and the honest-FINISH guarantee.

## Public API (stable surface)

- `runs.go` (run managers, lifecycle), `steps.go` (step counting),
  `drain.go` (`RUNS_DRAIN_QUEUE_CAP`, `RUNS_DRAIN_TTL`, force-drop policy),
  `cooldown.go` (run-scoped cooldowns, hardban).
- FINISH path: bounded deferred-FINISH queue (`RUN_FINISH_QUEUE_SIZE`) with
  synchronous inline fallback bounded by `RUN_FINISH_INLINE_TIMEOUT` when full.

## Allowed dependencies

`session`, `upstream` (archtest matrix). Tests use `testutil`, mock upstream.

## Forbidden dependencies

`pool`, `server`, `config`, `dashboard`. Run managers receive injected
dependencies (notably the session store via `SetSessionStore`-style wiring);
bridge entries get the same injection as pooled ones.

## Critical invariants

- FINISH is attempted on rotation, drain, idle timeout, and shutdown; the
  fallback is best-effort with a bounded timeout, never blocking teardown.
- Drain queues are capped with TTL eviction; older entries are force-dropped,
  not leaked.
- Inflight accounting (`LeaseRelease`/`LeaseAbandon`, `RecordRunStep`,
  `MarkRunFailed`) is nil-safe and exact; a leaked inflight counter wedges a run.
- Bridge run-resume is covered by the same persist tests as pooled
  run-resume (PR #348 pattern: store injection at entry creation).

## Tests that protect it

`runs_test.go`, `drain_queue_test.go`, `runs_edge_test.go`,
`drain_orphan_test.go`, `cooldown_hardban_test.go`,
`lifecycle_shutdown_test.go` (pool-side drain-outside-lock).

## Safe modification patterns

- New run-scoped counter: mirror the drain-queue bounded pattern; expose via
  pool snapshot; extend the window-edge assertions, not just happy paths.
- Never call upstream while holding a pool/bridge lock (evict outside the lock).