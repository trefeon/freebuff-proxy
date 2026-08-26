# AGENTS.md — freebuff-proxy

## What this is

A high-performance Go 1.26 gateway that fronts the FreeBuff/Codebuff CLI wire protocol,
providing full dual-protocol compatibility for both **OpenAI** and **Anthropic** API clients.
It enables any AI agent harness (Claude Code CLI, Cline, Roo Code, Cursor, Aider, Windsurf,
OpenCode, OMP, Vercel AI SDK, LiteLLM, LangChain) to interface seamlessly with FreeBuff upstream
models in either Pooled or Bridge mode.

A Svelte 5 SPA dashboard is embedded via `go:embed` in `internal/dashboard` and served under `/admin`.
Official reference specifications and SDKs reside in `reference/` (gitignored).

---

## Supported Protocol Surfaces

### 1. Anthropic Messages API
- `POST /v1/messages` — Full streaming SSE and non-streaming responses.
  - Streaming events: `message_start` (with estimated `input_tokens`), `content_block_start` (thinking/text/tool_use), `content_block_delta` (thinking_delta, text_delta, input_json_delta, signature_delta), `content_block_stop`, `message_delta` (with `output_tokens`), `message_stop`, and standard `event: ping` keepalive.
  - Automatic stripping of proxy-injected `end_turn` pseudo-tools so Claude Code CLI never fails on undeclared tools.
  - Thinking blocks contain required `"signature": ""` for strict TypeScript/Zod schema compliance.
  - Formatted Anthropic error envelopes: `{"type": "error", "error": {"type": "...", "message": "..."}}`.
  - Version header support (`anthropic-version: 2023-06-01`).
- `POST /v1/messages/count_tokens` — Local deterministic token estimation using `o200k_base` BPE tokenizer.

### 2. OpenAI Compatible API
- `POST /v1/chat/completions` — Streaming SSE (`chat.completion.chunk`) and non-streaming (`chat.completion`).
  - Strict OpenAPI 3.1 schema compliance: `refusal: null`, `logprobs: null`, and standard `usage` object.
  - Streaming XML tool-call extraction (`<tool_call>`, `<codebuff_tool_call>`, `<function_call>`, `<|tool_call_start|>`, fenced JSON) into native `tool_calls` deltas with sequential synthetic indices.
  - Legacy function normalization: auto-translates `functions` and `function_call` into `tools` and `tool_choice`.
  - Reasoning effort ladders (`minimal`, `low`, `medium`, `high`, `xhigh`) and think tag extraction (`<think>...</think>`).
- `POST /v1/responses` — OpenAI Responses API translation.
- `GET /v1/models` — Full catalog model list with live availability and tier annotations.
- `GET /v1/models/{model...}` — Single model retrieval endpoint supporting slash-delimited model names.
- `POST /v1/embeddings` — Embeddings proxy endpoint.

---

## Operating Modes

1. **Pooled Mode** (Default when `AUTH_TOKENS` is set):
   - Proxy manages a fixed pool of upstream FreeBuff tokens.
   - Handles admission coercion, rate limit backoffs, Pacific midnight resets, country blocks, and token rotation.
   - Client authentication via optional `API_KEYS`.

2. **Bridge Mode** (Active when `AUTH_TOKENS` is unset/empty):
   - Zero pre-configured credentials required.
   - Authenticates per-request FreeBuff tokens from `Authorization: Bearer <token>`, `x-api-key: <token>`, or `anthropic-api-key: <token>`.
   - Dynamically leases, caches, and maintains upstream sessions and runs per client token.

---

## Package Map

- `cmd/freebuff-proxy` — Entrypoint, CLI flag parsing (`-doctor`, `-test-token`, `-version`, `-config`, `-setup`).
- `internal/config` — Typed configuration loader, `.env` + JSON precedence, hot-reloading via `atomic.Pointer`.
- `internal/registry` — Model catalog synced from upstream; alias resolution and the `ServedModels` gate.
- `internal/convert` — Pure conversion logic:
  - `convert.go` — Request normalization, parameter whitelisting, role rewriting (`developer` → `system`), legacy function normalization.
  - `accumulator.go` — Non-streaming response assembler, XML tool call extractor, `Finish()` JSON builder.
  - `effort.go` — Reasoning effort extraction, thinking budget scaling, think tag stripping.
  - `schemacache.go` — Tool JSON schema normalization ($ref inlining, schema caching) and `end_turn` tool injection.
  - `sse.go` — SSE frame encoder/decoder, chunk sanitization, and end-turn stripping.
  - `streamxml.go` — Incremental XML stream parser.
- `internal/server` — HTTP router and handlers:
  - `server.go` — Router initialization, middleware wiring, lifecycle management.
  - `chat.go` — OpenAI chat completions handler, `relayStream`, `relayJSON`, and token lease acquisition.
  - `anthropic.go` — Anthropic `/v1/messages` and `/v1/messages/count_tokens` handlers, request translation, `relayAnthropicStream`, `relayAnthropicJSON`.
  - `responses.go` — OpenAI Responses API handler and event streamer.
  - `models.go` — `GET /v1/models` and `GET /v1/models/{model...}` handlers.
  - `streamxml.go` — Streaming XML tool call extraction relays for chat and Anthropic.
  - `errors.go` — Protocol-aware error formatting (OpenAI vs Anthropic) and PRD error mapping.
  - `middleware.go` — Auth validation (`Authorization`, `x-api-key`, `anthropic-api-key`), CORS, access logging.
  - `admin*.go` — Dashboard authentication, CSRF, config editor, token management API.
- `internal/pool` — Token lifecycle, admission, bridge caching, cooldowns, quota windows, and spend tracking (`acquire.go`, `bridge.go`, `cooldown.go`, `quota.go`, `spend.go`, `unfit.go`).
- `internal/session` — Upstream session manager and persistence.
- `internal/upstream` — FreeBuff wire client: session admission, chat relay, rate limit parser, stealth profiles.
- `internal/tokenestimate` — Local `o200k_base` BPE tokenizer for Anthropic token counting and streaming input token estimation.
- `internal/runs` — Run lifecycle: START/FINISH, step counting, drain queues.
- `internal/reasoningcache` — Multi-turn reasoning cache for assistant reasoning restoration.
- `internal/ratelimit` — Per-IP token bucket rate limiter.
- `internal/stealth` — TLS fingerprinting (utls) and header sanitization.
- `internal/telemetry` — Prometheus `/metrics` instrumentation.
- `internal/logring` — In-memory ring buffer for dashboard log streaming.
- `internal/dashboard` — Embedded single-page application (`assets_embed.go` / `assets_stub.go`).

---

## Load-Bearing Invariants

- **Hermetic Test Suite**: Always unset `AUTH_TOKENS` and `ADMIN_TOKEN` when running tests to avoid polluting bridge-mode test environments:
  ```bash
  env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./...
  ```
- **Anti-Ban Contract**:
  - Upstream session POST sends `x-freebuff-model` and `x-freebuff-instance-id`.
  - Upstream chat POST carries **NO** model header.
  - Pinned `ai-sdk` User-Agent on all chat requests.
  - Honest `FINISH` on run termination.
- **Tool Stripping Parity**:
  - The proxy injects `end_turn` to satisfy upstream schema requirements.
  - `end_turn` MUST be stripped before emitting to downstream clients (both OpenAI and Anthropic relays).
- **Sequential SSE Content Blocks**:
  - Anthropic streaming protocol requires strictly sequential block lifecycles (`content_block_start` → `content_block_delta` → `content_block_stop`). Never interleave unclosed blocks.

---

## Essential Commands

```bash
# Build binary
go build -o freebuff-proxy.exe ./cmd/freebuff-proxy

# Run full hermetic test suite
env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./...

# Check code formatting
gofmt -l cmd internal

# Format code
gofmt -w cmd internal

# Run CLI diagnostics
./freebuff-proxy.exe -doctor

# Probe configured token
./freebuff-proxy.exe -test-token
```

## Development Workflow

Single unified trunk on `main`. All features, CLI utilities, and embedded dashboard components live in this repository.

- Feature branches: `feat/<name>`, `fix/<name>`, or `refactor/<name>`.
- Pull requests target `main`.
- Never `git push --force` on `main`.
- The proxy can be compiled with or without the embedded dashboard via Go build tags (`-tags dashboard`) and configured at runtime via `DASHBOARD_ENABLED`.

### Reference repo policy (MANDATORY)

**Always sync `reference/freebuff` to the latest upstream before any reference-driven work.**

```bash
bash scripts/sync-upstream.sh --test-all   # refresh pins + verify parity
bash scripts/sync-upstream.sh --check      # drift-only check
```

- Upstream churns constants frequently (availability windows, session caps, agent maps). Stale reference data produces wrong wire behavior — never trust an unsynced tree.
- If the sync changes pinned snapshots, update `fallbackAgents` / `fallbackRootByModel` in `internal/registry/registry.go` to match (the parity test fails on drift), and re-run any drift analysis against in-flight work before continuing.
- Record the upstream SHA used for any decision that encodes reference facts into code.

## Repository Policy

- This repository is **public**. Only public-safe, secret-free content may ever be committed.
- Dev-only and reverse-engineering artifacts stay **gitignored** (`reference/`, `devdocs/`, `.env*`, `config*.json`, `*.session-state.json`).
- Commits follow Conventional Commits (`feat(...)`, `fix(...)`, `refactor(...)`, `docs(...)`, `test(...)`).
