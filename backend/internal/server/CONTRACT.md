# Package Contract: `backend/internal/server`

Task-local contract for agents modifying this package. Load before editing any file here. This is the busiest package in the repo — coordinate file ownership with parallel work before touching shared files.

## Purpose

The HTTP surface of the bridge: OpenAI chat completions + responses, Anthropic messages, model catalog, embeddings, health, and the embedded admin dashboard. Owns client auth, request sanitization (via `convert`), the protocol-neutral completion engine (acquire → upstream → relay), retry-once recovery, protocol-aware error envelopes, SSE relays, metrics, and admin auth/config/token endpoints.

## Public API (stable surface)

- `Server` (atomic `cfg` pointer — `/admin/reload` swaps it mid-flight, so every handler `Load()`s once per request), `Handler()` route table, `NewServer(...)`.
- Completion engine (`engine.go`, `engine_attempt.go`): `chatCore`, `chatAttempt`, `chatBackend` interface (pooled + bridge adapters share one recovery loop), `relayFunc`, `engine_sse.go` (`relayReadLoop`, `lineChunk`, `keepaliveInterval` 15s).
- Per-surface handlers (protocol policy lives HERE, never in the engine): `openai.go` + `openai_stream.go` + `openai_chunk_pipeline.go` + `openai_stream_rewrite.go` + `streamxml_openai.go`; `responses.go` + `responses_stream.go`; `anthropic.go` + `anthropic_stream.go` + `anthropic_json.go` + `anthropic_count.go` + `anthropic_errors.go` + `streamxml_anthropic.go`; `models.go` + `models_codex.go`.
- Cross-cutting: `errors.go` + `engine_helpers.go` (error class/status/envelope mapping), `middleware.go` (auth, CORS, access log, body caps), `health.go`, `gzip.go`.
- Admin: `admin.go`, `admin_auth.go` (HMAC cookie, login rate limit, loopback gates), `admin_env.go` (config editor), `admin_handlers.go`, `admin_tokens*.go` (token management), `admin_maturity.go`, `admin_csrf_test.go` neighbors.
- Limits: `maxRequestBody` 32MB, `maxStreamLine` 16MB scanner buffer.

## Allowed dependencies

`config`, `convert`, `dashboard`, `logring`, `pool`, `ratelimit`, `reasoningcache`, `registry`, `tokenestimate`, `updatecheck`, `upstream`, `phasetiming`, `session`, `runs`. Tests additionally use `testutil`. Server is the top of the internal stack and imports everything below it.

## Forbidden dependencies

`backend/cmd/*`, and cycles in the other direction — no internal package may import `server` (only `cmd` calls into it). A protocol-specific branch added to the shared engine is a debugging regression by definition (see below).

## Critical invariants

- Protocol split (engine.go header): `chatCore`/`chatAttempt`/SSE plumbing are protocol-neutral. Each surface owns its handler, wire translation, stream relay, and error envelope. An Anthropic bug must never require reading OpenAI relay code.
- Retry-once recovery (`chatAttempt`): session/run-invalid → invalidate + fresh acquire ONCE; `session_superseded` → surface immediately, never re-admit in-request (burns a fresh daily slot into the superseding instance); `turn_spend_limited` → TERMINAL (429 + code, no Retry-After, no cooldown, no failover); auth/ban/rate-limit/ip-capped/country → cooldown + surface; retryable `UpstreamError` (e.g. deployment window) → 503, no blind retry; canceled ctx → surface, never re-acquire. Rate-limit failover (`RATE_LIMIT_FAILOVER`, default on) re-acquires once onto a healthy token — pooled only, never in bridge mode.
- Tool Stripping Parity: the proxy-injected `end_turn` pseudo-tool MUST be stripped on EVERY relay (OpenAI, Anthropic, Responses) before emitting downstream.
- Anthropic streaming: strictly sequential block lifecycles (`content_block_start` → deltas → `content_block_stop`); thinking blocks carry `"signature": ""`; `message_start` carries estimated input tokens; `event: ping` keepalive.
- Error envelopes are protocol-aware: `/v1/messages` failures use the Anthropic envelope through the shared `chatCore → writeError → writeClientError` path (no parallel switch needed); OpenAI surfaces use the OpenAI shape (`refusal: null`, `logprobs: null`).
- Request correlation: every upstream attempt shares the server's `req_id` (D1); retry chains log once with real `backoff_ms`.
- Client disconnect cancels the upstream body read (context propagation); keepalives hold the downstream connection during long reasoning pauses.
- Dashboard security: open mode (`ADMIN_TOKEN` unset or factory default) is loopback-only (403 remote) for config/logs/token routes; login rate limit 5 fails/min/IP; stateless HMAC cookie; CSRF double-submit on admin POSTs.

## Tests that protect it

Harness conformance (`conformance_pi/omp/codex/cline/kilocode/qwen/roo/aider/goose/continue/keepalive/opencode` + `harness_compatibility`), wire replays (`replay_openai/anthropic/responses/ops` incl. `TestReplayMessagesTurnSpendLimited`), engine (`chat_attempt_test`, `chat_core_snapshot_test`, `engine_helpers_test`), relays (`relay_binding/unclosed_xml/xml/protocol_fixes`, `anthropic_stream_blocks`, `stream_completeness`), surfaces (`server_chat/hybrid/bridge/api`, `agentic_mimo_e2e`, `omp_mimo_simulation`, `fallback_transparency`, `request_params`, `model_unavailable`, `health_quota`, `server_models`, `wire_metrics`, `ratelimit`, `env`, `configmeta`, `access_logs`, `gzip`, `lifecycle`), dashboard + admin (`dashboard_test/pages/edge`, `admin_auth/csrf/openmode/password/routes/restart/require_login/internal/tokens`).

## Safe modification patterns

- New protocol surface: own handler + stream + JSON files (mirror the openai/anthropic trio), plus a `conformance_<harness>_test.go` proving a real client works end to end — never wedge it into the engine.
- New error class: `engine_helpers.go` errClass + status + BOTH envelopes + replay tests on both surfaces (OpenAI + Anthropic) asserting type/code/message/headers.
- New admin route: register in the route table AND extend `admin_routes_test.go` (it pins the full table); sensitive routes go behind the loopback gate.
- Frontend change: rebuild `backend/internal/dashboard/dist` from a clean tree (stash parallel `frontend/src/` WIP first) and commit it atomically with the source.
- Parallel-work hazard: this package sees the heaviest concurrent editing (`admin_tokens*.go`, `dashboard_test.go`, `anthropic_errors.go` broke builds mid-work more than once). Never edit or commit files owned by a parallel session; if `go vet`/`go build` fails on files you didn't touch, re-run per-package before assuming your change broke it.