# Harness Compatibility

freebuff-proxy exposes **three wire surfaces on a single base URL** — OpenAI Chat
Completions (`/v1/chat/completions`), OpenAI Responses (`/v1/responses`), and
Anthropic Messages (`/v1/messages`) — plus `GET /v1/models`. Twelve first-party
coding-agent harnesses were tested against those surfaces: **11 are supported**,
one (Gemini CLI) is not, because it speaks a fourth, Gemini-native wire.

| Base URL (everything below) | `http://127.0.0.1:3457/v1` |
|---|---|
| API key placeholder | `YOUR_KEY` |
| Default model | `z-ai/glm-5.3-flash` (unmetered; session-safe default) |
| Example model used below | `deepseek/deepseek-v4-flash` (unmetered) |
| Fallback model | `mimo/mimo-v2.5` |

Served model IDs: `openai/gpt-5.6-luna`, `meta/muse-spark-1.3-contributor`,
`upstage/solar-pro4`, `z-ai/glm-5.3-flash`, `deepseek/deepseek-v4-flash`,
`mimo/mimo-v2.5` (`z-ai/glm-5.2` referral-gated; `google/gemini-3.8-flash`
Pro-only, not served).

Ready-to-edit config templates live in [`examples/harnesses/`](../examples/harnesses/).

## Matrix

| Harness | Wire surface | Works | Notes |
|---|---|---|---|
| opencode | OpenAI Chat / Responses + Anthropic Messages | ✅ | Requests `reasoning.encrypted_content` on Responses; reasoning is still delivered as `reasoning_text` |
| codex | OpenAI Responses (Responses-only) | ✅* | No `wire_api` knob exists anymore; strict `/v1/models` parse falls back non-fatally |
| cline | OpenAI Chat (AI SDK v7) | ✅ | `max_completion_tokens` accepted |
| roo-code | Anthropic Messages + OpenAI Chat / Responses | ✅ | Base URL without `/v1` for the Anthropic path |
| goose | OpenAI Chat + Anthropic Messages | ✅ | `include_usage` always sent; tolerated |
| aider | OpenAI Chat (via LiteLLM) | ✅ | Chat only; prefix-routed model IDs work |
| continue | OpenAI Chat | ✅ | Responses is reserved for the official OpenAI base URL |
| qwen-code | OpenAI Chat or Anthropic Messages | ✅ | `USE_OPENAI` / `USE_ANTHROPIC`; no Responses needed |
| pi | OpenAI Chat / Responses / Anthropic Messages | ✅ | Configure `api: "openai-completions"`; default OpenAI provider is Responses |
| oh-my-pi | Same as pi | ✅ | opencode-go/zen auth is `x-api-key` — accepted |
| kilocode | OpenAI Chat + Anthropic Messages | ✅* | `baseURL` **must** include `/v1`; models come from models.dev |
| gemini-cli | Gemini native (`v1beta generateContent`) | ❌ | Native Gemini only — see [below](#gemini-cli-unsupported) |

\* Works today; a few client quirks are worth knowing (see each section).

---

## opencode

**Wire surface.** OpenAI Responses when the provider's SDK is `@ai-sdk/openai`
(`POST {baseURL}/responses`), OpenAI Chat with `@ai-sdk/openai-compatible`
(`POST {baseURL}/chat/completions`), Anthropic Messages with `@ai-sdk/anthropic`
(`POST {baseURL}/messages`). The base URL is the vendor base — opencode appends
the path (`/v1` must be included in the base URL).

**Config.** `opencode.json` (project root or `~/.config/opencode/opencode.json`):

```json
{
  "provider": {
    "freebuff": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "http://127.0.0.1:3457/v1",
        "apiKey": "YOUR_KEY"
      },
      "models": {
        "deepseek/deepseek-v4-flash": { "name": "DeepSeek V4 Flash" }
      }
    }
  }
}
```

Use `npm: "@ai-sdk/openai"` (same shape) for the Responses surface instead.

**Known limits.** OpenCode always requests `include: ["reasoning.encrypted_content"]`
on the Responses path. The proxy documents-ignores `include` and renders reasoning
as `reasoning_text`, which OpenCode accepts — no encrypted payload is required.
Header/auth: `Authorization: Bearer` (OpenAI) and `x-api-key` (Anthropic) are both
accepted by the proxy middleware.

## codex

**Wire surface.** Responses **only** — `POST {base_url}/responses`. The `wire_api`
config field defaults to Responses, and `wire_api = "chat"` is a hard
configuration error in current Codex; use `base_url` verbatim, no `/v1` munging.

**Config.** `~/.codex/config.toml`:

```toml
model_provider = "freebuff"
model          = "deepseek/deepseek-v4-flash"

[model_providers.freebuff]
name     = "freebuff-proxy"
base_url = "http://127.0.0.1:3457/v1"
env_key  = "FREEBUFF_API_KEY"
```

```bash
export FREEBUFF_API_KEY=YOUR_KEY   # codex reads the env var named by env_key
```

**Known limits.** Codex calls `GET /v1/models?client_version=…` and parses strict
OpenAI `ModelInfo` objects (slug, display_name, supported_reasoning_levels,
shell_type, visibility, …). The proxy returns its own catalog in the generic
OpenAI shape; Codex tolerates a parse miss and falls back to its bundled catalog
(no hard failure — but proxy-served IDs then only work if also in the bundled
set, so keep `model` set explicitly). `response.completed` requires an `id` and
usage triplets — the proxy emits both. `response.function_call_arguments.*` events
are ignored by Codex; the proxy's `custom_tool_call_input` dual emission is what
Codex consumes.

## cline

**Wire surface.** Cline v3 wraps the Vercel AI SDK v7. Any custom provider
("OpenAI Compatible") goes through `@ai-sdk/openai-compatible` →
`POST {baseUrl}/v1/chat/completions`. Provider IDs in the `openai` family use the
Responses API instead; the default for everything else is chat completions.

**Config.** Cline provider settings (UI: **API Provider → OpenAI Compatible**):

```json
{
  "apiProvider": "openai-compatible",
  "openAiBaseUrl": "http://127.0.0.1:3457/v1",
  "openAiApiKey": "YOUR_KEY",
  "openAiModelId": "deepseek/deepseek-v4-flash"
}
```

**Known limits.** Cline sends `max_completion_tokens` for reasoning models (the
proxy accepts it — it is whitelisted in request translation). Model IDs are
free-form (no `/v1/models` validation on this path). Extra headers (`X-Title`,
`User-Agent`, …) are Cline-only and harmless.

## roo-code

**Wire surface.** Three options, all supported: Anthropic Messages (the default
mental model — `POST {baseUrl}/v1/messages`), OpenAI Chat (`POST {baseUrl}/v1/chat/completions`),
OpenAI-native Responses (`POST {baseUrl}/v1/responses`).

**Config.** Roo Code provider settings:

```json
{
  "apiProvider": "openai-compatible",
  "openAiBaseUrl": "http://127.0.0.1:3457/v1",
  "openAiApiKey": "YOUR_KEY",
  "openAiModelId": "deepseek/deepseek-v4-flash"
}
```

Anthropic variant (note: **no `/v1`** — the SDK appends it):

```json
{
  "apiProvider": "anthropic",
  "anthropicBaseUrl": "http://127.0.0.1:3457",
  "apiKey": "YOUR_KEY",
  "anthropicModelId": "deepseek/deepseek-v4-flash"
}
```

**Known limits.** Anthropic beta headers (`fine-grained-tool-streaming`, and
`prompt-caching` when caching is on) are tolerated by the proxy. Model listing
fetchers swallow errors, so `GET /v1/models` failing to list a model is not fatal.

## goose

**Wire surface.** OpenAI Chat (`POST {base}/v1/chat/completions`) with
`OPENAI_BASE_URL`, or Anthropic Messages (`POST {base}/v1/messages`) with
`ANTHROPIC_HOST`. There is no `wire_api` switch — the surface is selected by
model name/provider.

**Config.** `goose.env.example` (export in your shell or `~/.config/goose/.env`):

```bash
OPENAI_BASE_URL=http://127.0.0.1:3457/v1
OPENAI_API_KEY=YOUR_KEY

# Anthropic-Messages alternative — host only, goose appends /v1/messages
#ANTHROPIC_HOST=http://127.0.0.1:3457
#ANTHROPIC_API_KEY=YOUR_KEY
```

**Known limits.** Goose always sends `include_usage: true` on chat requests (the
proxy returns usage — replay-tested). Goose treats an **empty** `finish_reason`
as non-terminal; the proxy always emits `finish_reason`, so streams complete
normally. Goose routes Responses-compatible models by naming convention
(`gpt-5*`/`o*`) when using its OpenAI provider — with the proxy, use the plain
chat provider.

## aider

**Wire surface.** Chat Completions only, via LiteLLM. Model IDs are prefix-routed
(`deepseek/…`, `anthropic/…`, `openai/…`), which matches the proxy's catalog IDs.

**Config.** Either flags or env — `aider.env.example`:

```bash
OPENAI_API_BASE=http://127.0.0.1:3457/v1
OPENAI_API_KEY=YOUR_KEY
```

or on the command line:

```bash
aider --openai-api-base http://127.0.0.1:3457/v1 --openai-api-key YOUR_KEY \
      --model deepseek/deepseek-v4-flash
```

**Known limits.** Aider's fast key validation picks the env var by model prefix
(`deepseek/` → `DEEPSEEK_API_KEY`); if you see a missing-key error, also set
`DEEPSEEK_API_KEY=YOUR_KEY` or pass `--api-key deepseek=YOUR_KEY`. No streaming
strictness beyond normal Chat Completions conventions.

## continue

**Wire surface.** OpenAI Chat Completions by default for any `openai`-family
provider (`POST {apiBase}/chat/completions`).

**Config.** `~/.continue/config.json`:

```json
{
  "models": [
    {
      "title": "DeepSeek V4 Flash (freebuff-proxy)",
      "provider": "openai",
      "model": "deepseek/deepseek-v4-flash",
      "apiBase": "http://127.0.0.1:3457/v1",
      "apiKey": "YOUR_KEY"
    }
  ]
}
```

**Known limits.** Continue uses the Responses endpoint **only** for o-series /
gpt-5+ models against the **official** `https://api.openai.com/v1/` base URL —
generic proxies like this one always get `/chat/completions`, so no Responses
quirks apply. The Anthropic path sends `anthropic-beta: prompt-caching-2024-07-31`
when caching is enabled (tolerated). Dual auth headers (`x-api-key` + `Bearer`)
are both accepted.

## qwen-code

**Wire surface.** Selected by auth type: `USE_OPENAI` → OpenAI Chat
(`POST {base}/chat/completions`, `Authorization: Bearer`); `USE_ANTHROPIC` →
Anthropic Messages in proxy mode (`POST {base}/v1/messages`, Bearer + `x-app: cli`
headers). No Responses surface, no `/v1/models` call at wire time.

**Config.** `qwen.env.example`:

```bash
USE_OPENAI=1
OPENAI_BASE_URL=http://127.0.0.1:3457/v1
OPENAI_API_KEY=YOUR_KEY
OPENAI_MODEL=deepseek/deepseek-v4-flash

# Anthropic-Messages alternative — host only, qwen-code appends /v1/messages
#USE_ANTHROPIC=1
#ANTHROPIC_BASE_URL=http://127.0.0.1:3457
#ANTHROPIC_API_KEY=YOUR_KEY
#ANTHROPIC_MODEL=deepseek/deepseek-v4-flash
```

**Known limits.** Qwen-Code hard-fails on SSE responses whose content type is not
`text/event-stream` (the proxy streams SSE correctly) and treats
`finish_reason == "error_finish"` as a stream error. Both wire paths are
replay-tested.

## pi

**Wire surface.** Per-model `api` field: `openai-completions` (Chat),
`openai-responses` (Responses), `anthropic-messages` (Messages), and more.
`openai-completions` is the least surprising choice here — reasoning is read
from `reasoning_content`/`reasoning`/`reasoning_text`.

**Config.** `~/.pi/agent/models.json`:

```json
{
  "providers": {
    "freebuff": {
      "baseUrl": "http://127.0.0.1:3457/v1",
      "api": "openai-completions",
      "apiKey": "YOUR_KEY",
      "models": [
        { "id": "deepseek/deepseek-v4-flash", "name": "DeepSeek V4 Flash" }
      ]
    }
  }
}
```

Select the provider/model with `/model` in pi, or run `pi --model deepseek/deepseek-v4-flash`.
The `anthropic-messages` alternative takes a base URL **without** `/v1`
(`http://127.0.0.1:3457` — the SDK appends `/v1/messages`).

**Known limits.** pi's default OpenAI provider API is `openai-responses`; both
that and the completions surface work, but completions avoids the
`reasoning.encrypted_content` `include` question entirely. pi's custom
`pi-messages` wire is a fourth surface the proxy does not implement (deferred —
pi itself can speak `openai-completions`). Non-SSE responses also work (the
proxy supports non-streaming chat).

## oh-my-pi

**Wire surface.** Same multi-protocol `api` field as pi (a fork). Opencode
providers (`opencode-go`, `opencode-zen`) authenticate with `x-api-key` only —
the proxy middleware accepts `x-api-key`, so a custom `freebuff` provider works
with either it or `Authorization: Bearer`.

**Config.** Identical to pi, in the oh-my-pi config locations:

```json
{
  "providers": {
    "freebuff": {
      "baseUrl": "http://127.0.0.1:3457/v1",
      "api": "openai-completions",
      "apiKey": "YOUR_KEY",
      "models": [
        { "id": "deepseek/deepseek-v4-flash", "name": "DeepSeek V4 Flash" }
      ]
    }
  }
}
```

**Known limits.** OMP's Responses history is canonical-ordered
(message → calls → outputs) and every assistant turn in thinking mode carries a
`reasoning_text` item — the proxy's Responses translation preserves item order.
The `context_management` anthropic beta is rejected by opencode-zen; the proxy
does not reject it.

## kilocode

**Wire surface.** Provider-chosen: `openai-compatible-chat` (Chat Completions),
`openai-responses` (Responses), Anthropic Messages. The `baseURL` **must include
`/v1`** for the OpenAI paths (`/chat/completions` and `/responses` are appended).

**Config.** Kilo Code config (`~/.kilocode/config.json`):

```json
{
  "providers": {
    "freebuff": {
      "id": "freebuff",
      "baseURL": "http://127.0.0.1:3457/v1",
      "api": "openai-compatible-chat",
      "apiKey": "YOUR_KEY",
      "models": ["deepseek/deepseek-v4-flash"]
    }
  }
}
```

**Known limits.** Model IDs come from the models.dev catalog (or `KILO_MODELS_URL`), **not** from `GET /v1/models` — so make sure the id you use is a models.dev entry (it is, for the ids above); otherwise add it as a custom model. Kilo has a default `chunkTimeout` of ~30s and aborts a stream that goes silent longer than that — the proxy emits a keepalive frame after at most 15s of relay silence, so sparse reasoning pauses stay inside the window. Kilo ends streams on body close rather than on a `[DONE]` line; the proxy closes streams cleanly.

## gemini-cli (unsupported, discontinued)

Gemini CLI speaks the **native Gemini wire** (`v1beta/models/{m}:generateContent`
and `:streamGenerateContent?alt=sse`, auth `x-goog-api-key`) — none of the three
surfaces this proxy exposes, and the CLI itself is **discontinued** (superseded
by Antigravity / Code Assist tooling). It is therefore **not supported**; there
is no workaround short of a fourth, Gemini-native surface, and none is planned.

If you need Gemini-family models: use `google-gemini`-capable harnesses from the
supported list (opencode, roo-code, goose, pi) pointing at a Gemini endpoint, or
run them against this proxy with the served catalog.

---

## Multi-surface auth model (all harnesses)

| Wire surface | Auth header | Harnesses |
|---|---|---|
| Chat Completions (`/v1/chat/completions`) | `Authorization: Bearer <key>` | opencode (openai-compatible), cline, roo-code, goose, aider, continue, qwen-code, pi, oh-my-pi, kilocode |
| Responses (`/v1/responses`) | `Authorization: Bearer <key>` | opencode, codex, cline, roo-code, pi, oh-my-pi, kilocode |
| Anthropic Messages (`/v1/messages`) | `x-api-key: <key>` (Bearer also accepted) | opencode, roo-code, goose, continue, pi, oh-my-pi |

The key value is anything in `API_KEYS` (or any token when running in bridge
mode); it never leaves your machine — the proxy holds the upstream credentials.

## Related docs

- [Getting Started](getting-started.md) — install, token, first request
- [Client Integration](client-integration.md) — SDK/UI-based quick connect
- [Dashboard Guide](dashboard.md) — admin web UI
