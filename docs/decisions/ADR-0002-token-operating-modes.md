# ADR-0002 — Pooled, bridge, and hybrid token modes

Status: Accepted

Context:
The upstream service ties quota to per-account tokens. Two deployment shapes
emerged: a solo operator pooling several of their own accounts, and a shared
router (e.g. 9router) whose users each bring their own token. One binary must
serve both, and the default must not force either shape on the other.

Decision:
- **Pooled**: `AUTH_TOKENS` set; requests served from the pool. Token picked by
  hot-session-first + rotation policy (`drain` default) + cooldown skip.
- **Bridge**: `AUTH_TOKENS` empty; the client's bearer credential is relayed
  upstream and per-token state is cached (LRU max 32, 72h idle eviction).
- **Hybrid**: `AUTH_TOKENS` set AND `BRIDGE_ENABLED` (default true); `API_KEYS`
  discriminates: a credential matching an entry is pooled, anything else is
  bridged. Missing credentials are rejected 401 only when `API_KEYS` is set.
- `BRIDGE_ENABLED=0` yields locked-down pooled-only (pre-hybrid behavior).

Reasoning:
One code path (pool + bridge cache side by side) covers all three shapes; the
mode is configuration, not a build variant. Hybrid-on-default means an existing
pooled install keeps working while accepting BYO-token clients, which was the
observed shared-router usage. `API_KEYS` is the discriminator in hybrid mode;
that is what makes the mixed case deterministic.

Alternatives considered:
- Separate binaries per mode: rejected; ops overhead and config drift.
- Bridge-only detection by token shape (`cb_...`): rejected; opaque and fragile.

Consequences:
- Auth semantics differ by mode; `middleware.go` is the single place that decides.
- Bridge entries are second-class citizens in the admin UI (bounded cache, no pool management).
- Hybrid mode means a stray bearer token is treated as a bridge credential and validated upstream; operators must understand that (see memory of 502 storms from stale process env).

Invariants:
- Pooled path never uses the client credential; bridge path never uses `AUTH_TOKENS`.
- `API_KEYS` is only a discriminator; it never gates bridge-mode-only instances.
- Bridge cache eviction never runs network calls while holding `bridgeMu`.

Affected packages:
`backend/internal/server/middleware.go`, `backend/internal/pool` (`bridge_cache.go`, `acquire.go`).

Related tests:
`pool_bridge_test.go`, `bridge_admission_test.go`, `bridge_singleflight_test.go`, `bridge_gaps_test.go`, `server_api_test.go` (hybrid auth matrix).