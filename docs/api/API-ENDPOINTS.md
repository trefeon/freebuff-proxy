# API Endpoints

Inventory of the public HTTP surface. User-facing behavior: README.
Protocol translation detail: ADR-0005. Admin auth detail:
docs/security/SECURITY-MODEL.md. Update this file in the same commit as any
endpoint change (see AI-CODE-REVIEW.md).

## Conventions

- Auth `API_KEYS` means: required when `API_KEYS` is set; open otherwise.
  In bridge mode (no `AUTH_TOKENS`) the bearer credential IS the upstream
  token and `API_KEYS` is meaningless. In hybrid mode `API_KEYS` match routes
  pooled, anything else routes bridge.
- Errors: OpenAI surfaces use the standard `error` object; `/v1/messages`
  uses `{"type":"error","error":{"type":...,"message":...}}`; both come from
  one writer that picks by request type.
- Streaming uses SSE (`text/event-stream`); non-streaming returns JSON.
  `end_turn` is stripped from every client-visible stream/response.

## `/v1/*` (client surface)

| Method + Path | Auth | Input | Output | Streaming | Errors | Side effects | Tests |
|---|---|---|---|---|---|---|---|
| `POST /v1/chat/completions` | `API_KEYS` | OpenAI chat (stream + non-stream; whitelist enforced, legacy `functions`→`tools`) | `chat.completion` / `chat.completion.chunk` + `[DONE]` | yes | OpenAI envelope; 429+Retry-After on local quota lock; 503 waiting-room; 502 exhausted; 400 unsupported params | session/run lease; token-state transitions on failure | server_api + relay suites, E2E |
| `POST /v1/responses` | `API_KEYS` | Responses API (function tools only; `previous_response_id`/`conversation`/`background`/built-ins → explicit 400s) | Responses object / SSE | yes | OpenAI envelope | same as chat | responses suites, E2E |
| `POST /v1/messages` | `API_KEYS` (+ `anthropic-api-key` header accepted) | Anthropic messages (`max_tokens` defaults 8192 when omitted; `top_k` documented-ignored) | Anthropic message / sequential content-block SSE | yes | Anthropic envelope | same as chat | replay_anthropic, conformance |
| `POST /v1/messages/count_tokens` | `API_KEYS` | Anthropic messages body | local deterministic estimate (o200k_base) | no | 400 malformed | none | tokenestimate + server tests |
| `GET /v1/models` | `API_KEYS` | — | catalog with `available`/`status`/`current_access_tier`; `MODELS_HIDE_UNAVAILABLE` prunes; `MODELS_ALLOW` prunes + 404s others | no | — | none | models tests, registry tests |
| `GET /v1/models/{model...}` | `API_KEYS` | slash-delimited model id | single model row | no | 404 unknown | none | models tests |
| `POST /v1/embeddings` | `API_KEYS` | — | `400 unsupported_endpoint` | no | explicit 400 | none | server_api tests |

## Observability (no auth, by design)

| Method + Path | Output | Notes |
|---|---|---|
| `GET /healthz` | liveness + pool snapshot: status, uptime, models, per-token snapshot incl. per-model `quota` map, `bridge_tokens`, spend | does NOT probe upstream (container stays healthy during upstream outages) |
| `GET /metrics` | Prometheus text: uptime, model count, per-token 24h messages/requests/runs/cooldown, `freebuff_proxy_quota_recent`/`_limit` | dynamic labels escaped; no per-request labels |

## `/admin` (operator surface)

| Method + Path | Auth | Notes |
|---|---|---|
| `GET /admin` (+ SPA assets) | none for assets; pages need session | unmatched `/admin/*` deep links fall back to SPA index |
| `GET/POST /admin/login` | none (per-IP lockout) | constant-time check; issues `fb_admin` + CSRF cookies |
| `POST /admin/api/change-password` | session (BOOTSTRAP EXEMPTION: reachable remotely with factory default — see SECURITY-MODEL gap 1) | requires current password |
| `POST /admin/reload` | Bearer `ADMIN_TOKEN` (falls back to loopback + legacy `API_KEYS` gates when unset) | hot-reload config from disk |
| `POST /admin/config` | session (sensitive: loopback-gated under factory default) | validate + atomic `.env` persist + reload, rollback on rejection |
| `POST /admin/tokens/...` | session (sensitive) | `/add`, `/remove` (last), `/test-all`, per-token `/test`, `/unlock`, `/finish`; persisted to `.env` |
| `POST /admin/mode` | session (sensitive) | runtime mode switch hybrid/pooled/bridge, persisted |
| `POST /admin/smoke` | session (sensitive) | one real chat through the pool: model/token/latency/preview |
| `POST /admin/diag` | session (sensitive) | same checks as `-doctor`, zero-cost probes |
| `/admin/api/*` (logs, stats, quota, traces, models, settings) | session (per-route levels in `admin_manifest.json`) | state-changing POSTs pass the CSRF gate |

Full auth-level table: `backend/internal/dashboard/admin_manifest.json`.