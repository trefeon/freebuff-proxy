# ADR-0004 — Session and run lifecycle

Status: Accepted

Context:
Upstream is a session protocol: a token admits a session (claiming a daily
slot), then agent runs execute inside it. Creating sessions is expensive (handshake,
quota data) and upstream holds one session per token at a time. The proxy must
reuse sessions aggressively, keep runs bounded in lifetime, and never leave
runs half-finished.

Decision:
- **Session** (`internal/session`): per-token admission state (instance id,
  expiry, tier/country, model locks). Created on demand, refreshed by polls
  (`SESSION_PROBE_CACHE_TTL` reuse window, `SESSION_RE_ADMIT_LEAD` pre-emptive
  refresh), ended by expiry or explicit DELETE. Single-flight refresh prevents
  admission stampedes.
- **Run** (`internal/runs`): one agent execution per model per token. Starts on
  first use; lives `ROTATION_INTERVAL` (default 6h) then rotates (fresh start,
  old one drained and FINISHed). Idle runs finish after `IDLE_ROTATION_TIMEOUT`
  (SAFE_MODE sets 30m). Drain uses a bounded queue (`RUNS_DRAIN_QUEUE_CAP`,
  `RUNS_DRAIN_TTL`); FINISH is best-effort, with an inline timeout fallback when
  the queue is full.
- **Lease** (`internal/pool`): the in-flight reservation. Caller MUST
  `LeaseRelease` on completion or `LeaseAbandon` on client disconnect.
- **Persistence** (`SESSION_PERSIST`): session metadata and active runs (never
  raw tokens) persist to a 0600 file keyed by token SHA-256 so a restart adopts
  state instead of re-admitting (saves a daily slot).
- **Shutdown**: sessions/runs FINISH honestly on graceful shutdown; drains wait
  for in-flight work.

Reasoning:
Reusing sessions and runs is what makes the pool fast (no handshake per
request) and quiet (no admission spam). Bounding run lifetime and finishing
honestly on rotation is both anti-ban hygiene and resource hygiene: a long-lived
abandoned run looks like automation and leaks upstream state. The
session/run/lease split matches the three distinct lifetimes and gives each its
own package and tests.

Alternatives considered:
- Fresh session per request: rejected; quota burn and latency.
- Runs without rotation: rejected; unbounded lifetime risk.

Consequences:
- Model locks bind a session to a model; lock recovery is DELETE → re-POST.
- Poll-404 and expired sessions must recreate cleanly; these paths have dedicated tests.
- Any new background worker must join the shutdown drain, or tests/ops see leaked goroutines.

Invariants:
- One session per token at a time; never create in parallel without the global/per-model admission caps.
- FINISH must be attempted on rotation, drain, idle timeout, and shutdown; missing FINISH is a bug.
- Persisted state must never contain raw tokens.
- Bridge entries get the same session store injection as pooled entries (post-boot entries included) — FUTURE WORK (PR #348 slice-1 plan; `bridge_cache.go` still builds runs via `runOptions(cfg)` with no store, so post-boot bridge entries lose run persistence until the injection mirrors the pooled `SetSessionStore` path).

Affected packages:
`internal/session`, `internal/runs`, `internal/pool` (acquire, drain), `internal/server` (relay lifecycle).

Related tests:
`session_lifecycle_test.go`, `session_admission_test.go`, `session_poll.go` tests, `session_persist_test.go`, `store_test.go`, `runs_test.go`, `drain_queue_test.go`, `drain_orphan_test.go`, `lifecycle_shutdown_test.go`, `pool_lifecycle.go` tests.