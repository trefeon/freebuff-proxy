# Architecture Glossary

Canonical terminology for freebuff-proxy. AI agents: use these terms exactly;
do not introduce synonyms. When code and this file disagree, fix the code
naming or update this file deliberately; never leave both in circulation.

## Core concepts

| Term | Definition |
|---|---|
| **upstream** | The FreeBuff/Codebuff service behind `UPSTREAM_BASE_URL` (default `https://codebuff.com`). It is a CLI coding-agent protocol, not an OpenAI-shaped API. |
| **client** | Anything that speaks HTTP to this proxy: OpenAI-compatible tools, Anthropic-compatible tools, routers, curl. |
| **token** | One FreeBuff account credential (`cb_...`). Owns an independent daily quota and can be rate-limited or banned independently. Never printed, never persisted raw. |
| **session** | Per-token upstream admission state (instance id, expiry, tier/country, model locks). Created by session admission POSTs, refreshed by polls, ended by DELETE or expiry. One session per token at a time. |
| **run** | One upstream agent execution for a model, shared across requests. Started on first use, lives up to `ROTATION_INTERVAL` (default 6h), then rotated (fresh start, old one drained and FINISHed). |
| **admission** | The upstream session-create handshake that claims the token's daily session slot and returns `rateLimitsByModel` quota data. |
| **pool** | The set of pre-configured upstream tokens (`AUTH_TOKENS`) plus the state the proxy keeps per token: sessions, runs, cooldowns, quotas, spend. |
| **pooled mode** | Requests are served from `AUTH_TOKENS`; the pool picks the token (hot-session-first, rotation policy, cooldown skip). |
| **bridge mode** | No `AUTH_TOKENS`. Each client presents its own token as the bearer credential and the proxy relays with it, caching per-token state (LRU, 32 entries, 72h idle eviction). |
| **hybrid mode** | `AUTH_TOKENS` set AND `BRIDGE_ENABLED` (default). `API_KEYS` discriminates: matching credential → pooled, anything else → bridge relay. |
| **lease** | The token/run reservation a request holds while in flight. The caller MUST release it (`LeaseRelease`) or abandon it on disconnect (`LeaseAbandon`). |
| **cooldown** | Temporary per-token lock after a classified upstream rejection (401 auth, 429 rate limit, `ip_capped`, ban, `country_blocked`). Bans and country blocks are terminal; auth/rate-limit cooldowns expire. |
| **quarantine** | The persistent unfit/banned account tracking (pool/unfit + quarantine state) that keeps dead tokens out of rotation. |
| **quota** | Upstream admission-reported per-model limits (`rateLimitsByModel`): `pacific_day`/`pacific_week` windows, reset times, entitlement breakdowns. Local ledgers additionally track RPM/RPD and `MAX_MESSAGES_PER_DAY`. |
| **model** | A catalog entry addressed as `provider/model` (e.g. `deepseek/deepseek-v4-flash`). Resolved through `registry` to the upstream agent that runs it. |
| **agent** | The upstream CLI agent identity a model maps to (e.g. `codebuff`). Wire-level concept owned by `registry`. |
| **end_turn** | A proxy-injected tool definition required by upstream's `foreign_toolset` schema validation. MUST be stripped before any client-visible output. |
| **reasoning effort** | Client `reasoning_effort`/`reasoning.effort` normalized into upstream reasoning tiers via `modelcat` ladders. |
| **stealth** | Egress masking: TLS fingerprint (uTLS), sanitized headers, request jitter, pinned user agents. Controlled by `SAFE_MODE` and `TLS_FINGERPRINT`. |
| **SAFE_MODE** | Default-on anti-ban preset bundle: CLI-faithful TLS baseline, proxy-header sanitization, 0-200ms jitter, 30m idle rotation. |
| **modelcat** | The single source of truth for per-model facts: served/paused/premium, caps, context windows, effort tiers. |
| **registry** | Model → agent mapping (per-model roots + fallback agents), alias resolution, offline pinned catalog + live refresh. |
| **admin** | The `/admin` surface: SPA dashboard, login, config editor, token management, logs. Distinct from `/v1/*`. |
| **bridge entry** | The per-token cached state (session/run/ledger) of one bridge-mode client token. |
| **spend** | Advisory per-token Pacific-day ledger of "spend units"; surfaced on `/healthz`, enforced nowhere (server-enforced ceiling is future work). |
| **maturity** | Streak-maturity automation (pool/maturity.go): probe-only touches that advance an account's streak without claiming session slots; dry-run by default. |

## Naming conventions in code

- Package names match their responsibility (one concept per package, enforced by `archtest`).
- `internal/upstream` is the wire client; `internal/session` manages state on top of it; `internal/pool` orchestrates both. Never invert these edges.
- `internal/convert` is pure JSON transformation: no I/O, no environment reads, no config imports.
- Config keys are uppercase snake_case env names; JSON `-config` keys mirror them.
- Client-visible error classes (`turn_spend_limited`, `waiting_room`, `model_unavailable`, ...) are wire codes from `internal/upstream`, never free-form strings.

## Terms that are NOT synonyms

- `cooldown` (temporary, expires) vs `quarantine` (persistent unfit tracking) vs `ban` (terminal, upstream-decided).
- `session` (admission state) vs `run` (agent execution) vs `lease` (in-flight reservation).
- `pooled` (AUTH_TOKENS) vs `bridge` (client token) vs `hybrid` (both).
- `quota lock` (local, from parsed 429 reset) vs upstream `429` (the raw signal).
- `modelcat` (facts) vs `registry` (mapping/resolution) vs `catalog` (the pinned constant data registry loads).