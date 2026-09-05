# AGENTS.md — freebuff-proxy

## What this is

A high-performance Go 1.26 gateway that fronts the FreeBuff/Codebuff CLI wire protocol,
providing full dual-protocol compatibility for both **OpenAI** and **Anthropic** API clients.
It enables any AI agent harness (Claude Code CLI, Cline, Roo Code, Cursor, Aider, Windsurf,
OpenCode, OMP, Vercel AI SDK, LiteLLM, LangChain) to interface seamlessly with FreeBuff upstream
models in either Pooled or Bridge mode.

A Svelte 5 SPA dashboard lives in `frontend/`, builds into `backend/internal/dashboard/dist`, and is embedded into the single binary via `go:embed`; it is served under `/admin`. Unmatched `/admin/*` deep links fall back to the SPA index (`index.html`). In dev, run `task frontend:dev` (Vite on `127.0.0.1:5173`, base `/admin/`, proxying `/admin/*` to a local gateway on `127.0.0.1:3457` — override the target via `VITE_PROXY_TARGET`) alongside `task dev`; in prod the built SPA is embedded.
Official reference specifications and SDKs reside in `reference/` (gitignored): `freebuff/` (upstream CLI, synced by scripts), `protocols/` (OpenAI/Anthropic SDKs + specs), `harnesses/` (12 client harnesses, tool-signature corpus), `agents/` (other harnesses/routers), plus FreeBuff ecosystem clones (`freebuff-reverse`, `freebuff2api-*`, `proxy-freebuff`, `freebuff-proxy-*`) that code comments cite as provenance — keep those folder names stable.

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
  - Streaming XML tool-call extraction (`<tool_call>`, `<tool_calls>` plural, `<codebuff_tool_call>`, `<function_call>`, `<|tool_call_start|>`, fenced JSON) into native `tool_calls` deltas with sequential synthetic indices.
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
   - `RATE_LIMIT_FAILOVER` (default on, pooled-only): token-level 429s re-lease onto a healthy token. `turn_spend_limited` is TERMINAL (per-turn breaker, no retry/cooldown/failover) — only a fresh turn flows. RPM admission is atomic at lease grant.

2. **Bridge Mode** (Active when `AUTH_TOKENS` is unset/empty):
   - Zero pre-configured credentials required.
   - Authenticates per-request FreeBuff tokens from `Authorization: Bearer <token>`, `x-api-key: <token>`, or `anthropic-api-key: <token>`.
   - Dynamically leases, caches, and maintains upstream sessions and runs per client token.

---

## Package Map

- `backend/cmd/freebuff-proxy` — Entrypoint, CLI flag parsing (`-doctor`, `-test-token`, `-version`, `-config`, `-setup`).
- `backend/internal/config` — Typed configuration loader, `.env` + JSON precedence, hot-reloading via `atomic.Pointer`.
- `backend/internal/registry` — Upstream model→agent mapping (fallback agents + per-model roots, synced from upstream) and alias resolution (`ResolveModel`); per-model facts (served/paused/premium/efforts) live in `modelcat`.
- `backend/internal/convert` — Pure request/response normalization (whitelist, roles, schemas + `end_turn` injection, SSE sanitize, accumulator + XML extraction, effort). Details: `convert/CONTRACT.md`.
- `backend/internal/server` — HTTP surface + shared completion engine (`engine*.go`, protocol-neutral) with per-surface handlers (`openai*`, `anthropic*`, `responses*`, `models*`, `streamxml_*`, `errors.go`, `middleware.go`, `admin*.go`). Details: `server/CONTRACT.md`.
- `backend/internal/pool` — Token lifecycle, admission, bridge caching, cooldowns, quota windows, and spend tracking (`acquire.go`, `bridge.go`, `cooldown.go`, `quota.go`, `spend.go`, `unfit.go`).
- `backend/internal/session` — Upstream session manager and persistence.
- `backend/internal/upstream` — FreeBuff wire client: session admission, chat relay, rate limit parser, stealth profiles.
- `backend/internal/tokenestimate` — Local `o200k_base` BPE tokenizer for Anthropic token counting and streaming input token estimation.
- `backend/internal/runs` — Run lifecycle: START/FINISH, step counting, drain queues.
- `backend/internal/reasoningcache` — Multi-turn reasoning cache for assistant reasoning restoration.
- `backend/internal/ratelimit` — Per-IP token bucket rate limiter.
- `backend/internal/stealth` — TLS fingerprinting (utls) and header sanitization.
- `backend/internal/telemetry` — Prometheus `/metrics` instrumentation.
- `backend/internal/logring` — In-memory ring buffer for dashboard log streaming.
- `backend/internal/dashboard` — Embedded single-page application (`assets_embed.go` / `assets_stub.go`).

---

## Load-Bearing Invariants

- **Hermetic Test Suite**: Always unset `AUTH_TOKENS` and `ADMIN_TOKEN` when running tests to avoid polluting bridge-mode test environments:
  ```bash
  env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./backend/...
- **Anti-Ban Contract** (canonical: INV-UP-001..005):
  - Upstream session POST sends `x-freebuff-model` and `x-freebuff-instance-id`; chat POST carries **NO** model header.
  - Pinned `ai-sdk` User-Agent on chat requests; honest `FINISH` on run termination.
  - `REQUEST_TIMEOUT` guards response headers only (TTFB) — the streamed body is never cut by it.
- **Tool Stripping Parity** (INV-PROTO-001/006): injected `end_turn` MUST be stripped on every relay; `<tool_calls>` plural extracts like singular.
- **Sequential SSE Content Blocks** (INV-PROTO-002): strictly sequential block lifecycles, never interleaved.
- **Terminal turn spend** (INV-TOKEN-002): `turn_spend_limited` = 429 + code, no Retry-After/cooldown/failover.
- **Atomic RPM admission** (INV-TOKEN-006): counted at lease grant under the owning lock.

---

## Essential Commands

```bash
# Canonical gate (format, vet, build, hermetic Go tests, frontend checks)
task verify

# Full CI-equivalent gate (+ race, frontend build/e2e, dist freshness)
task verify:full

# Build binary
go build -o freebuff-proxy.exe ./backend/cmd/freebuff-proxy

# Run full hermetic test suite
env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./backend/...

# Check code formatting
gofmt -l backend

# Format code
gofmt -w backend

# Run CLI diagnostics
./freebuff-proxy.exe -doctor

# Probe configured token
./freebuff-proxy.exe -test-token
```

## Development Workflow

Protected trunk on `main` — enforced by GitHub, not convention. Direct pushes
are rejected; every change lands as: feature branch (`feat/`, `fix/`,
`docs/`, `refactor/`) → local `task verify` → PR → 4 required CI checks
(`analyze`, `frontend`, `golangci`, `test`, strict) → squash merge
(branch auto-deleted). Never `git push --force`.
- The dashboard SPA is embedded in every build (no build tag) and toggled at runtime via `DASHBOARD_ENABLED`.
- Rebuild the embedded SPA first with `task frontend:build` before compiling the binary.

### Reference repo policy (MANDATORY)

**Always sync `reference/freebuff` to the latest upstream before any reference-driven work.**

```bash
bash scripts/sync-upstream.sh --test-all   # refresh pins + verify parity
bash scripts/sync-upstream.sh --check      # drift-only check
```

- Upstream churns constants frequently (availability windows, session caps, agent maps). Stale reference data produces wrong wire behavior — never trust an unsynced tree.
- If the sync changes pinned snapshots, update `pinnedFallbackAgents` / `pinnedFallbackRootByModel` in `backend/internal/registry/registry.go` (plus `retiredRootOverrides` in `parse.go` for retired roots) to match (the parity test fails on drift), and re-run any drift analysis against in-flight work before continuing.
- Record the upstream SHA used for any decision that encodes reference facts into code.

## Engineering Guardrails (AI-maintained codebase)

The codebase is mature and heavily invariant-laden. Preserve semantics; do not chase rewrites. The guardrails below are the contract for every change.

### Work-type separation
Three work classes, kept in separate commits/PRs — never mixed in one change:
- TYPE A — Feature (new surface)
- TYPE B — Internal refactor (zero behavior change)
- TYPE C — Upstream drift adaptation (reference sync + parity)

### AI change budget
A single task may touch at most: **10 files, 500 LOC, 2 packages, 0 new dependencies, 0 public API changes**. Exceeding any limit → STOP, split the task, request an architecture review before continuing. Scope creep inside a task ("while fixing X, also Y") is a violation.

### Architecture freeze (semantics frozen without explicit approval)
Protocol contracts, token lifecycle, session lifecycle, upstream wire protocol, authentication, model-catalog semantics, SSE ordering, quota accounting, anti-ban invariants, release pipeline. Implementation may be optimized; semantics may not change without a decision note in `docs/decisions/` (ADR format).

### Architecture tests
`backend/internal/archtest` pins the backend package dependency matrix (explicit allowlist per package). A new internal import is a conscious architecture decision: extend the matrix deliberately, never bypass it. Per-package guards also exist: `config/layer_imports_test.go`, `convert/layer_reviewfix_test.go`.

### Tests are the behavioral spec
Never delete or weaken a contract test to make CI green. A failing contract test means the change broke documented behavior — fix the change. Bug fixes ship with a regression test that fails pre-fix and passes post-fix.

### Dashboard freeze
Any visual change must comply with `DESIGN.md` (repo root). No new visual primitives (colors, radii, shadows, type scale, motion) without updating `DESIGN.md` first. Never "make the dashboard nicer" without a spec.

### Package contracts
Before touching a backend package, read `backend/internal/<pkg>/CONTRACT.md` (Purpose / API / allowed + forbidden deps / invariants / protecting tests / safe modification patterns). The contracts are the task-local architecture context; the package map above is the index. Repo-wide view: `docs/architecture/` (OVERVIEW, BOUNDARIES, DEPENDENCIES, INVARIANTS, GLOSSARY); rationale: `docs/decisions/` (ADRs).

## Repository Policy

- This repository is **public**. Only public-safe, secret-free content may ever be committed.
- Dev-only and reverse-engineering artifacts stay **gitignored** (`reference/`, `devdocs/`, `.env*`, `config*.json`, `*.session-state.json`).
- Commits follow Conventional Commits (`feat(...)`, `fix(...)`, `refactor(...)`, `docs(...)`, `test(...)`).
