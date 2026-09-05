# Package Contract: `backend/internal/pool`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Multi-token front door for chat requests. Owns token selection order, session admission, run leasing, cooldowns/quarantines, per-token quota windows (RPM/RPD/daily messages), spend tracking, the bridge-mode token cache, model-lock routing, and streak-maturity automation. Port of freebuff2api-quorinex `run_manager.go` (Acquire half) with the upstream/session/runs split of this project.

## Public API (stable surface)

- Construction: `New(cfg, clients, sessions, reg) (*Pool, error)`, `SetConfig(*config.Config)`, `SetSessionStore(*session.Store)`, `SetNotifier(*notify.Sender)`, `Start(ctx)`, `Shutdown(ctx)`.
- Serving: `Acquire(ctx, model) (*Lease, error)`, `AcquireBridge(ctx, clientToken, model) (*Lease, error)`, `Chat(ctx, lease, opts, body) (io.ReadCloser, error)`.
- Lease lifecycle (all nil-safe): `LeaseRelease`, `LeaseAbandon`, `RecordRunStep`, `MarkRunFailed`, `RecordSpend`.
- Invalidation: `InvalidateSession(WithReason)`, `InvalidateRun`, `InvalidateLeaseSession(WithReason)`, `InvalidateLeaseRun`, `InvalidateBridgeSession(WithReason)`, `InvalidateBridgeRun`.
- Cooldowns: `CooldownToken*` / `CooldownLease*` / `CooldownBridge*` (auth / rate-limit / IP-capped / ban / country-blocked); `UnlockToken`, `LockToken`, `UnlockLockToken`, `LockBridgeEntry`, `UnlockBridgeEntry`.
- Probes: `ProbeNewToken(ctx, token)`, `ProbeToken(ctx, idx)`.
- Token management: `AddToken`, `RemoveLastToken`, `RemoveTokenAt`, `RemoveAllTokens`, `SwapTokens`, `MoveToken`, `TokenCount`, `SetTokenAccountInfo`, `EnsureTokenSession`.
- Views: `Snapshot() []TokenSnapshot`, `PoolSnapshot()`, `BridgeSnapshot() []BridgeTokenSnapshot`, `BridgeCount()`, `PremiumQuotaForToken/Bridge` (+ backward-compat aliases), maturity snapshot carried on `TokenSnapshot`.
- Maturity: `SetMaturity`, `MaturityTouchNow`; type `MaturitySnapshot`.
- Unfit: `MarkModelUnfit`, `ClearModelUnfit(Before)`, `ModelUnfit`.

## Allowed dependencies

`config`, `notify`, `phasetiming`, `runs`, `session`, `upstream`, `registry`, `modelcat` (maturity.go only). Tests additionally use `testutil`, `upstream/testmock`.

## Forbidden dependencies

`server`, `convert`, `dashboard`, `stealth`, `ratelimit`, `logring`, `tokenestimate`, `reasoningcache`, `cmd/*`. Pool sits below `server`; `server` calls INTO pool, never the reverse. A new internal import here is a layering event — review before adding.

## Critical invariants

- Failover error-bucket precedence (PRD §6): ban > country-blocked > model-IP-limited > rate-limit > waiting-room > daily cap. Only when NO bucket matches any token does the pool return a combined error. A queued token surfaces 503 + Retry-After as soon as no higher bucket is populated.
- 401 auth rejection → 30-min cooldown for that token, try the next. Run-invalid/session-invalid recoveries are NOT handled here: the caller (server) retries once via a fresh Acquire after invalidating.
- Lease contract: caller MUST call `LeaseRelease` (or `LeaseAbandon` on client disconnect) when the request completes or fails; it decrements the run's inflight counter.
- Swap-safety: token indexes are display positions; leases hold entry pointers, never indexes.
- Quota semantics: RPM = ADMITTED requests (rolling 60s window); RPD = SUCCESSFUL chats in the current Pacific day (bucket rolls at Pacific midnight); `MAX_MESSAGES_PER_DAY` = rolling 24h successful chats. 0 = unlimited. RPM/RPD counters live in the ledger, never in the static cache.
- Bridge cache: every read AND write of entries/ledgers runs under `bridgeMu`; `rpmCount`/`dayRequestCount` prune/roll in place and MUST be called under the owning lock (data-race fix 2026-09-06). Never call upstream/network while holding `bridgeMu` (evict outside the lock).
- `roster.Load()` once per call — never cache the pointer across calls.
- Maturity (pool/maturity.go): `MATURITY_ENABLED` default ON (global kill-switch), dry-run default (probe-only, zero session slots claimed), unmetered touch models only (premium-short gated by `MATURITY_ALLOW_PREMIUM` + per-token opt-in), jittered daily slot in the account's own timezone, restart-safe 6h throttle, stops firing after 3 consecutive non-advancing days, never touches quarantined/banned/cooling/country-blocked accounts. Touch must fail closed on priced models (`skip:touch-priced`).
- Spend ledger records events only — the $ ceiling is enforced elsewhere (server-enforced).

## Tests that protect it

`pool_acquire_test.go` (selection/failover), `acquire_order_test.go` (quota-first ordering), `pool_edge_test.go`, `pool_cooldown_test.go`, `pool_quota_test.go` + `pool_quota_window_test.go` (Pacific-midnight roll), `pool_request_test.go` (RPM/RPD window semantics), `pool_bridge_test.go` + `bridge_admission_test.go` + `bridge_singleflight_test.go` + `bridge_gaps_test.go` (bridge cache/eviction), `quarantine_test.go`, `hardban_test.go`, `unfit_test.go`, `spend_test.go`, `glm_referral_test.go`, `model_locks_test.go`, `maturity_test.go`, `lifecycle_shutdown_test.go` (bridge drain outside lock), `pool_swap_test.go`, `pool_remove_test.go`, `random_rotation_test.go`, `admission_leader_election_test.go`, `session_handling_test.go`, `pool_webhook_test.go`, `quota_tracker_test.go`.

## Safe modification patterns

- New config knobs: `config` is a bottom layer (rejects all internal imports via `layer_imports_test.go`) — add shape-only `Validate` checks there; semantic checks (served/unmetered) live in the pool fire path + admin endpoint. Register the key in `keycatalog.go` (byte-ascending order) + `dotenvKeys`, then regen fixtures with `FP_REGEN_FIXTURE=1`.
- Adding a ledger counter: mirror `rpmCount`/`dayRequestCount` — prune in place, expose through `roster` + bridge cache under the owning lock, and extend `pool_request_test.go` window-edge assertions.
- `-race` does not run on Windows locally (cgo issue) — race-sensitive changes MUST be verified on Linux CI.
- After editing struct literals: `git diff | grep '^-[^-]'` and confirm every removed line has an intended re-add (dropped fields compile as zero values).
- Bridge-run persistence: new run managers must receive the session store via the same injection pattern as pooled entries (`SetSessionStore`); cover bridge run-resume in the persist tests.