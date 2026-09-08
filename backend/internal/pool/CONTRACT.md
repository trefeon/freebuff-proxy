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
- Views: `Snapshot() []TokenSnapshot`, `PoolSnapshot()`, `BridgeSnapshot() []BridgeTokenSnapshot`, `BridgeCount()`, maturity snapshot carried on `TokenSnapshot`.
- Boot seed: `SeedQuotaSnapshot([]QuotaSeedRow)` (ADR-0024, CLI-orchestrated boot push of latest persisted rows per token/model; idempotent, never downgrades live data).
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
- History sink (`history_test.go`): maturity config/touch/release/warn events
  emit outside token locks; the sink must never block or call back into the
  pool (nil sink = persistence-free).
- Maturity (pool/maturity.go): `MATURITY_ENABLED` default ON (global kill-switch), dry-run default (probe-only, zero session slots claimed), touch mode opt-in per token (premium-short spends from the account's metered pool, no global gate), per-token `touch_model` override via `SetMaturity` (empty = global `MATURITY_TOUCH_MODEL` fallback; shape-only check at save, served/unmetered semantics fail closed in the fire path), jittered daily slot in the account's own timezone, restart-safe 6h throttle, stops firing after 3 consecutive non-advancing days, never touches quarantined/banned/cooling/country-blocked accounts. Touch must fail closed on priced models (`skip:touch-priced`). Touch-model candidates come from the served catalog ordered by Freebucks cost class (unmetered-capable first, premium-pool last) — the registry carries no price data, so never invent prices.
- Spend ledger records events only — the $ ceiling is enforced elsewhere (server-enforced).
- Quota auto-probe (pool/quota_autoprobe.go, ADR-0022): `QUOTA_AUTO_PROBE` default ON (GroupPool, live-apply, no RestartOnly), rides every maintainTick pass alongside maturity (no new goroutine, idle stretches included), deterministic slot = FNV(token-index + Pacific day) spread over [reset-2h, reset), earliest-future reset from the cached quota map, in-memory `quotaProbeDay` per entry (restart may double-probe once), session-less `ProbeToken` warn-only with a 10s bound, day marked on attempt (exactly one GET/token/day), unknown-reset unseeded tokens get one staggered boot probe instead of waiting for a manual one (ADR-0024 recovery). `Validate` needs no case (bool zero = off; production default ON comes from `Load`).
- Burst balance (pool/burst.go, ADR-0023): `BURST_BALANCE_ENABLED` default OFF (GroupPool, live-apply, no RestartOnly), `BURST_WINDOW` default 1m, `BURST_THRESHOLD` default 20, `BURST_MAX_TOKENS` default 2 (validated >= 2). In-memory per-model sliding window: hits appended on pooled lease grant, pruned on every maintainTick pass alongside maturity/autoprobe (a restart starts un-tripped). While a model's live-window count exceeds its threshold, selection for THAT model switches to least_used among healthy tokens, capped at BURST_MAX_TOKENS distinct accounts (over-cap tokens demoted, never excluded — failover still reaches them). Entry/exit WARN-logged per model. Disabled or below-threshold: no state recorded, order untouched (byte-identical default path).
- Quota boot seed + first-probe recovery (pool/quota_bootseed.go, ADR-0024): `SeedQuotaSnapshot` fills the live view from latest-persisted rows (CLI-orchestrated pre-Start; pool never imports store); seeded rows carry probe time, show last-known instantly, and teach the scheduler reset_at. Unknown-reset AND unseeded tokens get one staggered boot probe (token-index hash over [boot, boot+5m), session-less `ProbeToken` warn-only, once per process via `quotaBootProbed`, day marked so no same-day double with scheduler slots; zero boot time (never Started) never fires. `QUOTA_AUTO_PROBE=false` kills the boot probe; seed display still applies.
- Quota visit bulk probe (pool/quota_visitprobe.go, ADR-0025): `QuotaVisitProbeMaxAge` 1h const (no env knob), pool-scoped in-memory `lastBulkProbe` behind `bulkProbeMu` (restart re-probes on the next stale visit). `ProbeAll` force-probes every token via session-less `ProbeToken` (8s bound each, warn-logged failures) and stamps; `ProbeAllIfStale(ctx, maxAge)` claims the slot before probing so concurrent tabs share one pass, else no-op. Pure `quotaVisitProbeDue` helper pins the never/fresh/stale/future boundary. Ignores the `QUOTA_AUTO_PROBE` kill-switch (explicit user-visit action like the manual button, not scheduler work).

## Tests that protect it

`pool_acquire_test.go` (selection/failover), `acquire_order_test.go` (quota-first ordering), `pool_edge_test.go`, `pool_cooldown_test.go`, `pool_quota_test.go` + `pool_quota_window_test.go` (Pacific-midnight roll), `pool_request_test.go` (RPM/RPD window semantics), `pool_bridge_test.go` + `bridge_admission_test.go` + `bridge_singleflight_test.go` + `bridge_gaps_test.go` (bridge cache/eviction), `quarantine_test.go`, `hardban_test.go`, `unfit_test.go`, `spend_test.go`, `glm_referral_test.go`, `model_locks_test.go`, `maturity_test.go`, `lifecycle_shutdown_test.go` (bridge drain outside lock), `pool_swap_test.go`, `pool_remove_test.go`, `random_rotation_test.go`, `admission_leader_election_test.go`, `session_handling_test.go`, `pool_webhook_test.go`, `quota_tracker_test.go`.
`quota_bootseed_test.go` (ADR-0024: seed fills view + scheduler reset learning, bad-row drops, stale-seed no-downgrade, idempotent re-push, boot slot spread/determinism, due gates incl. zero-boot skip, virgin-fires-once, seeded-skips, disabled-skips).
`quota_autoprobe_test.go` (ADR-0022 scheduler: slot determinism/window-spread, due day-gate/disabled/unknown-reset/stale-catch-up gates, earliest-future reset selection, once-per-day + maintainTick wiring against the mock upstream, disabled-delta-zero proof).
`burst_test.go` (ADR-0023: disabled byte-identical selection, trip + per-model isolation, max-tokens cap, window-expiry recovery, grant-path wiring through real Acquires, maintainTick pruning, live-disable restoring drain).
`quota_visitprobe_test.go` (ADR-0025: due never/fresh/stale/future boundaries, never-probed fires past the scheduler kill-switch, fresh skips with zero upstream touch, stale re-arms after maxAge, manual force probes every token + stamps + never gated).

## Safe modification patterns

- New config knobs: `config` is a bottom layer (rejects all internal imports via `layer_imports_test.go`) — add shape-only `Validate` checks there; semantic checks (served/unmetered) live in the pool fire path + admin endpoint. Register the key in `keycatalog.go` (byte-ascending order) + `dotenvKeys`, then regen fixtures with `FP_REGEN_FIXTURE=1`.
- Adding a ledger counter: mirror `rpmCount`/`dayRequestCount` — prune in place, expose through `roster` + bridge cache under the owning lock, and extend `pool_request_test.go` window-edge assertions.
- `-race` does not run on Windows locally (cgo issue) — race-sensitive changes MUST be verified on Linux CI.
- After editing struct literals: `git diff | grep '^-[^-]'` and confirm every removed line has an intended re-add (dropped fields compile as zero values).
- Bridge-run persistence: new run managers must receive the session store via the same injection pattern as pooled entries (`SetSessionStore`); cover bridge run-resume in the persist tests.