# Architecture Overview

freebuff-proxy is a Go 1.26 gateway that exposes FreeBuff/Codebuff upstream
models through OpenAI, Anthropic, and Responses API surfaces. Single binary:
the Svelte 5 admin dashboard is embedded via `go:embed` (ADR-0001). The repo
is public; everything committed must be public-safe.

## System map

```text
HTTP / API surfaces
   POST /v1/chat/completions   (OpenAI, stream + JSON)
   POST /v1/responses          (OpenAI Responses, stream + JSON)
   POST /v1/messages           (Anthropic, stream + JSON)
   POST /v1/messages/count_tokens
   GET  /v1/models
   POST /v1/embeddings         (400 unsupported_endpoint)
   GET  /healthz  GET /metrics (no auth)
        └── server (transport, auth, routing, error envelopes)
                 │
                 ├── convert (pure translation, no I/O)
                 ├── pool (token orchestration: selection, cooldown, quota)
                 │     ├── session (per-token admission state)
                 │     ├── runs (agent execution lifecycle)
                 │     └── upstream (wire client + error classification)
                 │           └── stealth (TLS fingerprint, headers)
                 ├── registry (model→agent mapping, alias resolution)
                 ├── modelcat (per-model facts)
                 ├── reasoningcache (multi-turn reasoning restore)
                 └── ratelimit (per-IP bucket)

Admin surface
   /admin (SPA) → dashboard package (data APIs) → server admin handlers
   login: ADMIN_TOKEN → fb_admin cookie (HttpOnly, SameSite=Strict, +Secure
          when TLS) + double-submit CSRF cookie; per-IP login rate limit
   sensitive routes (config editor, logs, token mgmt, reload): loopback-only
   when ADMIN_TOKEN is the factory default `123456`; bearer for /admin/reload

Upstream (FreeBuff)
   session admission POST/GET/DELETE, chat POST (SSE), agent-runs
   START/STEP/FINISH, /api/auth/cli/*, /api/v1/ads, freebuff/streak
```

## Request flow (POST /v1/chat/completions)

1. Access-log + CORS middleware; `requireAuth` loads the config ONCE per
   request (stamps a context snapshot so a concurrent `/admin/reload` cannot
   split one request across two config views) and enforces `API_KEYS` when set.
2. `chatCore` decides pooled vs bridge routing from that same snapshot:
   hybrid mode uses `API_KEYS` match as the discriminator.
3. Pool `Acquire`/`AcquireBridge` picks a token: hot-session-first, rotation
   policy, cooldown skip, quota checks. Returns a `Lease` (must be released or
   abandoned).
4. `convert.NormalizeRequest` translates the body (whitelist, roles, tools,
   `end_turn` injection, reasoning effort); `upstream.Client.Chat` sends the
   enveloped request (no model header on chat) with stealth + jitter.
5. Response: streaming relay (`openai_stream.go`) emits sanitized
   `chat.completion.chunk` events with synthetic sequential tool-call indices
   and strips `end_turn`; non-streaming path accumulates + builds the strict
   OpenAPI 3.1 JSON. Client disconnect cancels the request context; lease is
   abandoned.
6. Errors map to the OpenAI error envelope; classified upstream failures
   transition token state (cooldown/quarantine) per ADR-0003.

Anthropic `/v1/messages` and Responses `/v1/responses` follow the same core
(shared `chatCore`/`relay` helpers) with surface-specific translation and
error envelopes; Anthropic streaming keeps strictly sequential content blocks.

## Configuration lifecycle

Defaults < JSON `-config` < `.env` (platform config dir, `./.env` wins when
present) < environment. Loaded at boot by `cli.Serve`; hot-reloaded via
`POST /admin/reload` or dashboard save (atomic pointer swap; construction-fixed
state like the pool roster survives unchanged until restart). See
ADR-0007 and docs/architecture/BOUNDARIES.md §Config.

## Startup / shutdown (cli.Serve)

- Startup: parse flags → load config → build logger (level precedence:
  `LOG_LEVEL`, `-v` → debug, dev builds default debug) → registry (fallback
  pinned catalog + refresh loop) → one upstream client + session manager per
  token → pool (`Start`) → dashboard/logring wiring → HTTP server.
- Shutdown: on `os.Interrupt`/`SIGTERM` (Ctrl+C and Ctrl+Break both map to
  SIGINT on Windows), the server drains: in-flight requests complete, leases
  release, runs/sessions FINISH honestly, background workers stop via context
  cancellation. Shutdown is graceful, bounded, and pinned by lifecycle tests.

## Ownership summary (enforced by archtest)

| Concern | Owner |
|---|---|
| Transport, auth, routing, error envelopes | `internal/server` |
| Pure request/response translation | `internal/convert` |
| Token selection, cooldowns, quota, bridge cache | `internal/pool` |
| Session admission state | `internal/session` |
| Agent run lifecycle | `internal/runs` |
| Wire client, classification, stealth application | `internal/upstream` (+`stealth`) |
| Model facts | `internal/modelcat` |
| Model mapping/aliases/fallback | `internal/registry` |
| Per-model reasoning effort ladders | `modelcat` + `convert/effort.go` |
| Multi-turn reasoning restore | `internal/reasoningcache` |
| Per-IP rate limiting | `internal/ratelimit` |
| Metrics + logs | `internal/telemetry`, `internal/logring` |
| Admin data APIs + SPA | `internal/dashboard` |
| Config | `internal/config` (bottom layer) |
| CLI modes | `internal/cli/*` |

See DEPENDENCIES.md for the full import matrix and GLOSSARY.md for terms.