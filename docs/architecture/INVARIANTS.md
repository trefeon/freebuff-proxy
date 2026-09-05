# Load-Bearing Invariants Register

Behavioral contracts that must never change accidentally. IDs group by area.
Every invariant has: why it exists, current implementation, tests that protect
it, what would break it. Treat this file as a contract, not prose.
Precedence: this register is canonical. Package `CONTRACT.md` files elaborate
the same invariants task-locally and must not contradict them; the root
`AGENTS.md` entrypoint states them in one line each with pointers here.
When in doubt, the ID below wins.
## Protocol translation

### INV-PROTO-001 — `end_turn` never reaches a client
Why: the proxy injects `end_turn` to satisfy upstream `foreign_toolset`
schema validation; emitting it to clients breaks every harness.
Implementation: `convert/schemacache.go` (injection),
`convert/sse.go StripEndTurnToolCalls`; both OpenAI and Anthropic relays call it.
Protected by: `convert_schema_test.go`, stream tests in `server_api_test.go`.
What breaks it: a new relay path that skips stripping.

### INV-PROTO-002 — Sequential Anthropic content blocks
Why: Anthropic streaming requires strictly ordered
`content_block_start → content_block_delta → content_block_stop` lifecycles;
interleaved blocks corrupt client state machines.
Implementation: `server/anthropic.go` relay.
Protected by: `replay_anthropic_test.go`, streaming E2E tests.
What breaks it: emitting deltas for a block whose `_stop` already fired, or
starting a second block before the first closes.

### INV-PROTO-003 — Three surfaces share one internal representation
Why: chat/completions, responses, and messages must not drift into
three parallel translation implementations with divergent behavior.
Implementation: shared `chatCore`, `convert.NormalizeRequest`,
shared error writer picking the envelope by request type.
Protected by: `server_api_test.go`, conformance tests (`conformance_pi_omp_test.go`).
What breaks it: a per-surface copy of normalization logic.

### INV-PROTO-004 — Explicit 400s, not silent drops
Why: silently dropping unsupported params (OpenAI `n>1`, `audio`,
`web_search_options`, ...) misleads clients.
Implementation: whitelist + explicit 400 mapping in `convert/convert.go`.
Protected by: `convert_req_test.go`.
What breaks it: adding a param to the whitelist without translation + tests.

### INV-PROTO-005 — Strict OpenAI response shape
Why: strict OpenAPI 3.1 compliance (`refusal: null`, `logprobs: null`,
standard `usage`).
Implementation: accumulator `Finish()` + chunk sanitization in `convert`.
Protected by: `convert_stream_test.go`, `accumulator` tests.
What breaks it: emitting non-conformant shapes from a new relay path.
### INV-PROTO-006 — `<tool_calls>` plural dialect extracts like singular
Why: some models wrap the same payload in `<tool_calls>`; leaking it as
literal text breaks tool execution in every harness.
Implementation: `xmlShapeToolCalls` shape + fifth block-regex group in
`convert/accumulator_xml.go`; payload parsing is shape-agnostic.
Protected by: `TestExtractXMLToolCallsPluralDialect`,
`TestXMLStreamExtractorPluralDialect`,
`TestXMLStreamExtractorPluralDanglingFlush`.
What breaks it: a new wrapper dialect without the three coordinated edits
(shape table + regex + dangling scrub).
## Upstream wire behavior (anti-ban contract)

### INV-UP-001 — Chat carries NO model header
Why: upstream fingerprinting; the chat request must look like the official CLI.
Implementation: `upstream/client_chat.go` (headers built without model).
Protected by: wire tests, E2E parity.
What breaks it: adding `x-freebuff-model` to the chat path.

### INV-UP-002 — Session POST carries `x-freebuff-model` + `x-freebuff-instance-id`
Why: admission must identify the model and instance.
Implementation: `upstream` session admission path.
Protected by: `client_chat_test.go`, session admission tests.

### INV-UP-003 — Honest FINISH on run termination
Why: abandoned runs look like automation and leak upstream state.
Implementation: `runs` drain/rotation/shutdown paths attempt FINISH; bounded
queue + inline timeout fallback.
Protected by: `runs_test.go`, `drain_queue_test.go`, `lifecycle_shutdown_test.go`.
What breaks it: skipping FINISH on a new exit path.

### INV-UP-004 — Pinned user agents
Why: `ai-sdk/openai-compatible/1.0.0/codebuff` (chat),
`Freebuff-CLI/1.0.0` (ads), `Bun/1.3.14` (session/auth). `CLI_VERSION` is
informational only and must never leak into the wire.
Implementation: `upstream` header construction.
Protected by: wire/header tests.
What breaks it: wiring `CLI_VERSION` into the UA.
### INV-UP-005 — `REQUEST_TIMEOUT` guards headers only, never the body
Why: a request-context deadline cut healthy long streams at the 15m default.
Implementation: transport `ResponseHeaderTimeout`; no deadline on the chat
request context; body runs until upstream EOF or caller cancel.
Protected by: `TestChatStreamSurvivesPastRequestTimeout`,
`TestChatTTFBTimeoutStillAborts`.
What breaks it: re-attaching any deadline to the chat request context.
## Token lifecycle

### INV-TOKEN-001 — Terminal classes never become cooldowns
Why: `banned`/`country_blocked` are account-terminal; treating them as
transient cooldowns re-hammers a dead token and masks the signal.
Implementation: pool quarantine + hardban handling.
Protected by: `quarantine_test.go`, `hardban_test.go`.
What breaks it: a new error class landing in the cooldown switch by mistake.

### INV-TOKEN-002 — `turn_spend_limited` is terminal for the turn
Why: retrying a poisoned turn re-trips the breaker for minutes; only a fresh
turn can succeed.
Implementation: `upstream` classify → terminal `ErrTurnSpendLimited`; server
maps to 429 + `rate_limit_error`/`turn_spend_limited`, NO Retry-After, NO token
cooldown, NO failover spin.
Protected by: `TestClassifyTurnSpendLimited`, `TestChatAttemptTurnSpendTerminal`,
`TestReplayMessagesTurnSpendLimited`.
What breaks it: re-adding Retry-After or token cooldown to this class.

### INV-TOKEN-003 — Quota lock answers locally, <1ms, zero upstream traffic
Why: the zero-spam guarantee; routers need fast local 429s.
Implementation: pool quota lock keyed on parsed `resetAt` (Pacific midnight).
Protected by: `pool_quota_test.go`, `pool_quota_window_test.go`.
What breaks it: sending the request upstream anyway for locked tokens.

### INV-TOKEN-004 — Failover error-bucket precedence
ban > country-blocked > model-IP-limited > rate-limit > waiting-room > daily cap.
Why: the surfaced error must be the most actionable class, not the first
token's.
Implementation: `pool/acquire.go` bucket aggregation.
Protected by: `pool_acquire_test.go`, `pool_edge_test.go`.
What breaks it: reordering buckets or adding one without updating the PRD map.

### INV-TOKEN-005 — Lease contract: caller releases or abandons
Why: inflight counters and run teardown depend on it.
Implementation: `LeaseRelease`/`LeaseAbandon`; nil-safe.
Protected by: pool lease tests, lifecycle tests.
What breaks it: a new handler path that forgets release on error.
### INV-TOKEN-006 — RPM admission is atomic with the lease grant
Why: check-in-Acquire / record-in-Chat let concurrent bursts pass the cap
before any record landed.
Implementation: `tryAdmitRequest` (roster lock) / `bridgeTryAdmitRequest`
(`bridgeMu`) at grant time; full window releases the run and counts the
token rate-limited.
Protected by: `TestAcquireRPMAdmitBurstIsAtomic`,
`TestBridgeRPMAdmitBurstIsAtomic`, `TestAcquireCountsAdmissionAtGrant`.
What breaks it: moving the record back to the chat path.
## State and concurrency

### INV-STATE-001 — One config snapshot per request
Why: a reload landing mid-request must not split routing decisions across two
config views.
Implementation: `cfgSnapshotKey` stamped by `requireAuth`; `cfg.Load()` once.
Protected by: `middleware` tests.
What breaks it: re-loading config inside a handler that already saw a snapshot.

### INV-STATE-002 — Bridge cache mutations under `bridgeMu`, network outside it
Why: data races and lock-hold-while-blocking.
Implementation: every read/write of bridge entries/ledgers under `bridgeMu`;
eviction runs outside the lock; RPM/day counters prune in place under the lock.
Protected by: `bridge_*_test.go`, race CI.
What breaks it: any new bridge state access outside the lock.

### INV-STATE-003 — Roster/ledger counters live in the pool, not static caches
Why: construction-fixed pool + runtime token adds/removes.
Implementation: `pool/roster`, `pool_ledger.go`.
Protected by: `pool_swap_test.go`, `pool_remove_test.go`, quota window tests.
What breaks it: caching the roster pointer across calls.

### INV-STATE-004 — Shutdown is graceful and bounded
Why: orphaned sessions/runs leak upstream state; hangs block service stops.
Implementation: `cli.Serve` signal handling (os.Interrupt + SIGTERM; Ctrl+Break
→ SIGINT on Windows), context cancellation, drain.
Protected by: lifecycle shutdown tests, `TestCtrlBreakDrainsGracefully` (Windows).
What breaks it: a background worker not bound to the shutdown context.

## Security

### INV-SEC-001 — Secrets never logged, dumped, or shown in full
Why: public repo + real credentials in `AUTH_TOKENS`/bridge bearers.
Implementation: redaction in `upstream/dump.go`, `upstream` response logging,
dashboard effective-config table (set/unset + counts).
Protected by: `TestDumpRedactsTokenHeaders`, `TestDoUpstreamResponseLogsAndPreservesBody`.
What breaks it: logging a raw request body or a new credential header.

### INV-SEC-002 — Admin auth ≠ authorization ≠ CSRF ≠ rate limiting
Why: each control closes a different hole; none substitutes for another.
Implementation: `ADMIN_TOKEN` login (constant-time, per-IP rate limit, bounded
concurrent slots), `fb_admin` cookie (HttpOnly, SameSite=Strict, Secure when
TLS), double-submit CSRF cookie + `X-CSRF-Token` on state-changing POSTs,
loopback gate for sensitive routes under the factory default token.
Protected by: `admin_*_test.go` (login, csrf, openmode, require-login,
password-remote, internal).
What breaks it: weakening any single control "because the other exists".

## Config and assets

### INV-CFG-001 — Precedence chain is deterministic
defaults < JSON `-config` < `.env` < environment; documented in README + ADR-0007.
Protected by: `config_test.go`, `config_env_test.go`.
What breaks it: reordering load sources silently.

### INV-CFG-002 — `.env` writes are atomic and 0600, validation rolls back
Why: a truncated or half-applied `.env` bricks the operator's config.
Implementation: `adminSaveMu` + temp-file rename; validate-before-write.
Protected by: `admin_password_test.go`, dashboard config tests.
What breaks it: writing the file before validating, or in-place truncation.

### INV-CFG-003 — Committed dashboard dist is fresh
Why: the SPA is go:embed'd; a stale bundle sails silently into the binary.
Implementation: CI rebuilds frontend and runs `git diff --exit-code -- backend/internal/dashboard/dist`.
Protected by: `ci.yml` + `release.yml` frontend jobs.
What breaks it: committing dist from an older frontend, or editing dist by hand.

### INV-CFG-004 — Registry pins match the recorded upstream SHA
Why: stale pinned constants produce wrong wire behavior.
Implementation: `sync-upstream.sh` + `catalog_test.go` parity + drift CI.
Protected by: `registry_test.go`, `catalog_test.go`, upstream-drift workflow.
What breaks it: hand-editing pinned testdata without sync.

## Cross-platform

### INV-XPLAT-001 — Windows is a first-class target
Ctrl+Break drains (SIGINT), Task Scheduler service, `.cmd` wrappers, exe-adjacent
`.env` warning. Protected by `cli_windows_test.go`, `TestCtrlBreakDrainsGracefully`.
What breaks it: Unix-only assumptions in paths/signals/service code.