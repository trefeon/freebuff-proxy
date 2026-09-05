# Package Contract: `backend/internal/upstream`

Task-local contract for agents modifying this package. Load before editing any file here.

## Purpose

The codebuff.com wire client for ONE token: session create/poll/release, run START/FINISH, chat-completions relay with the CLI request envelope required to pass the free-mode gate (`403 free_mode_cli_required`), typed error classification (mirrors proxy-freebuff's recovery matrix), TLS fingerprint stealth, CLI login flows, waiting-room ad chain, token health probes, and parsing of quota/freebucks/streak/standing payloads.

## Public API (stable surface)

- Construction: `New(token, cfg)`, `NewWithIndex(token, idx, cfg)`, `NewForAuth(cfg)`, `NewClientID()`; `SetTransport(rt)` (test seam, production never calls it); `SetMock` / `IsMock` (simulated upstream via `testmock`).
- Wire calls: `ChatCompletions(ctx, ChatOptions, body) (io.ReadCloser, error)`, `CreateSession(ForModel)`, `GetSession(WithOpts)`, `ProbeAccount`, `GetStreak`, `EndSession`, `StartRun`, `FinishRun(ctx, runID, status, totalSteps, steps, errorMessage)`, `FireWaitingRoomChain`, `FetchAccountInfo`.
- Login: `StartCLILogin`, `StartCLILoginIsolated`, `StartCLILoginWithFingerprint`, `PollCLILogin`, `ProtocolGitHubLogin` (+ `CLILoginCode/User/Status`).
- Introspection: `TokenKey()` (sha256, safe for filenames/logs — the token itself never appears in persisted state), `BaseURL()`, `TransientRetries()`, `CapacityDeferredRetries()`, `FingerprintRotations()`, `RateLimitEvents()`, `PendingWaitingRoomChain()`, `ConsumeWaitingRoomChain()`.
- Types: `SessionState` (+ `ModelQuota`, `FreebucksInfo` and its windows/wallet/ceiling/allowance/price-change blocks, `SessionReferral`, `SubscriptionInfo`, `AvailabilityWindow`, `SessionStanding`, `StreakInfo`, `RunStep`), `ChatOptions`, `TokenHealth` + states.
- Errors: typed structs with sentinel matching — `ErrAuthRejected`, `ErrRateLimited` (`RateLimitError`), `ErrBanned` (`BanError`), `ErrCountryBlocked`, `ErrCredits`, `ErrIpCapped`, `ErrTurnSpendLimited` (`TurnSpendLimitError`), `SessionSupersededError`, `WaitingRoomError`, `WaitingRoomRequiredError`, `UpstreamError` (with `Retryable`), `CapacityDeferredError`, `LimitedIpError`, `SessionLimitError`.

## Allowed dependencies

`config`, `stealth`, `telemetry`, `logring` (wire metrics only), `login` (subpackage). Tests additionally use `testutil`, `testmock`.

## Forbidden dependencies

`pool`, `server`, `convert`, `dashboard`, `runs`, `session`, `registry`, `notify`, `ratelimit`, `tokenestimate`, `reasoningcache`, `modelcat` (non-test), `cmd/*`. Upstream is the leaf wire layer: session/run lifecycle and pooling orchestrate IT, never the reverse.

## Critical invariants (anti-ban contract)

- Session POST: bare-fetch shape — Authorization + optional `x-freebuff-model` ONLY (session.go). `GetSession` adds `x-freebuff-instance-id` (+ `x-freebuff-compact-session` when compact). Chat POST carries NO model/instance header (model rides only in body metadata).
- `ProbeAccount`: GET with NO `x-freebuff-instance-id` — zero-cost, claims no session slot, burns no daily allowance.
- Chat POST: pinned `ai-sdk` User-Agent, Bearer-only (never `x-codebuff-api-key`). Envelope: `codebuff_metadata`, `provider.data_collection=deny`, forced `stream:true`, JSON-quoted `"cb_easp"` stop sentinel. Other calls default to the Bun UA; `/api/auth/cli/*` uses the login UA.
- `ACTING_USER_ID` is only safe when it equals the token's own account id; any other value impersonates a foreign user.
- `REQUEST_TIMEOUT` bounds only the wait for response headers (TTFB) via the transport's `ResponseHeaderTimeout` — the streamed body runs until upstream EOF or caller cancel (request-context deadlines cut healthy long streams; fixed 2026-09-06).
- Transient retries: ONLY transport-level failures (dial/TLS/reset/EOF) retry, bounded by `TRANSIENT_RETRIES`, with fingerprint rotation + 200-600ms backoff. Classified errors (429/403/401, session/run invalids, waiting room, status >= 400) and non-replayable bodies NEVER retry.
- `turn_spend_limit` is TERMINAL: classify returns it before the retryable branch — no cooldown, no failover, no Retry-After downstream.
- Honest `FINISH` on run termination (anti-ban); one step recorded per completed chat call; per-run step counters and trace ids stay per-run on retry onto a fresh run.
- Dump/debug paths must never log the token (`SetTransport` is a test seam only).

## Tests that protect it

`client_chat_test.go` (stream survival past REQUEST_TIMEOUT, stalled-header abort, envelope wire contract, client-cancel abort), `client_retry_test.go` (flaky transport, backoff, deadline precedence), `client_session_test.go` (session lifecycle, control-timeout precedence), `tokenhealth_test.go` (health states incl. turn-spend soft), `probe_account_parity_test.go`, `wire_time_unify_test.go`, `effort_mirror_test.go`, `freebucks_parse_test.go`, `notices_test.go`, `inject_envelope_test.go`, `client_stealth_test.go` (CLI UA pin), `egress_stealth_test.go`, `egress_device_block_test.go`, `signal_guard_test.go`, `ratelimit_cooldown_test.go`, `availability_test.go`, `session_startrun_cancel_test.go`, `login/fingerprint_test.go`, `wire_metrics_test.go`, `testdata/` (email-domain lists).

## Safe modification patterns

- New error class: typed struct + sentinel + classify() early-return + pool fire-path case + server `errors.go` branch — all four, or the error falls through to a generic 502.
- Wire shape changes: sync `reference/freebuff` first (`scripts/sync-upstream.sh`), cite the upstream path + SHA in the comment, pin with a parse test (see `freebucks_parse_test.go`, `probe_account_parity_test.go`).
- Header/UA/envelope changes are ban-risk: match the CLI exactly, never invent headers, never route egress through a proxy (the upstream hard-blocks proxy/VPN/Tor).
- New wire call: add a mock path in `testmock` first, then the client method, then the caller — never develop against the live wire.
- Timeouts: TTFB belongs on the transport; body lifetime belongs to the caller context; control calls keep their own tight ctx deadlines.