# Client Integration Guide

Connect any OpenAI-compatible AI tool or coding assistant to `freebuff-proxy`.

**Server Base URL:** `http://localhost:3457/v1`  
**API Key:** `not-needed` (or your FreeBuff token in bridge mode)

Any OpenAI-compatible client works: OpenCode, pi, 9router, LiteLLM, or your own scripts (Python / Node.js below).

---

## Bridge Mode vs Pooled Mode

+ **Pooled Mode:** Set `AUTH_TOKENS=token1,token2` in the proxy's `.env`. The proxy drains keys one at a time: it prefers the token with a live session and only moves on when one is rate-limited, never aggressively rotating healthy keys. Clients can use any placeholder API key. (Not the out-of-the-box default: with `AUTH_TOKENS` unset the proxy starts in bridge mode — unless a CLI token is auto-discovered.)
+ **Bridge Mode (Routers & Multi-User):** Leave `AUTH_TOKENS=` empty in `.env`. The proxy acts as a zero-storage relay. **API Routers ([9router](9router-integration.md), OmniRouter, One API, LiteLLM) send actual FreeBuff tokens in `Authorization: Bearer <freebuff-token>`.** The proxy lazily creates sessions per client token with LRU caching, rate limits, and health tracking; cached bridge entries are visible in `GET /healthz`.
---

## 1. opencode

Add to `opencode.json` or `~/.config/opencode/opencode.json`:

```json
{
  "providers": {
    "freebuff": {
      "type": "openai",
      "options": {
        "baseURL": "http://localhost:3457/v1",
        "apiKey": "not-needed"
      },
      "models": [
        { "id": "deepseek/deepseek-v4-flash", "name": "DeepSeek Flash" }
      ]
    }
  }
}
```

---

## 2. pi (Coding Agent CLI)

Add a provider to `~/.pi/agent/models.json`:

```json
{
  "providers": {
    "freebuff": {
      "baseUrl": "http://localhost:3457/v1",
      "api": "openai-completions",
      "apiKey": "not-needed",
      "models": [
        { "id": "deepseek/deepseek-v4-flash", "name": "DeepSeek Flash" }
      ]
    }
  }
}
```

Pick the model with `/model` inside pi, or start it directly with `pi --model deepseek/deepseek-v4-flash`. The file is re-read each time you open `/model`, so no restart is needed.

---

## 3. Python (Official OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:3457/v1",
    api_key="not-needed"
)

response = client.chat.completions.create(
    model="deepseek/deepseek-v4-flash",
    messages=[{"role": "user", "content": "Write a python function to check prime numbers."}],
    stream=True
)

for chunk in response:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="")
```

---

## 4. Node.js (Official OpenAI SDK)

```javascript
import OpenAI from 'openai';

const openai = new OpenAI({
  baseURL: 'http://localhost:3457/v1',
  apiKey: 'not-needed',
});

const response = await openai.chat.completions.create({
  model: 'deepseek/deepseek-v4-flash',
  messages: [{ role: 'user', content: 'Say hello in TypeScript!' }],
  stream: true,
});

for await (const chunk of response) {
  process.stdout.write(chunk.choices[0]?.delta?.content || '');
}
```

---

---

## 5. Cursor IDE

1. Open **Cursor Settings** -> **Models**.
2. Turn off other models and click **Add custom model**: `deepseek/deepseek-v4-flash`.
3. In **OpenAI API Key**, enter any placeholder (e.g. `not-needed`).
4. Click **Override OpenAI Base URL** and set: `http://localhost:3457/v1`.

---

## 6. VS Code (Continue / Cline / Roo Code)

### Continue Extension (`~/.continue/config.json`)
```json
{
  "models": [
    {
      "title": "FreeBuff DeepSeek Flash",
      "provider": "openai",
      "model": "deepseek/deepseek-v4-flash",
      "apiBase": "http://localhost:3457/v1",
      "apiKey": "not-needed"
    }
  ]
}
```

### Cline / Roo Code
- **API Provider**: OpenAI Compatible
- **Base URL**: `http://localhost:3457/v1`
- **API Key**: `not-needed`
- **Model ID**: `deepseek/deepseek-v4-flash`

---

## 7. Chatbox / NextChat / LibreChat / Jan

- **API Host / Base URL**: `http://localhost:3457/v1`
- **API Key**: `not-needed`
- **Model Name**: `deepseek/deepseek-v4-flash`

---

## 8. API Routers & Aggregators (9router, OmniRouter, One API, LiteLLM)

For multi-account management or multi-user API routing:

1. **Proxy Setup:** Run the proxy in **Bridge Mode** (leave `AUTH_TOKENS=` empty in `.env`).
2. **Router Setup (9router / OmniRouter):**
   + **Provider Type:** OpenAI Compatible
   + **Base URL:** `http://localhost:3457/v1` (or container host `http://host.docker.internal:3457/v1`)
   + **API Keys:** Add your actual **auth token(s)** as the node API keys in 9router or OmniRouter.
   + **Connection strategy:** with several keys, configure **fallback / priority (fill the first)**: never round-robin, which burns every account's quota at once and is a high-risk signal for account bans (see the [9router guide](9router-integration.md)).
3. **Routing Behavior:** When 9router or OmniRouter routes a request, it sends the key as `Authorization: Bearer <token>`. The proxy lazily creates and caches upstream free sessions for each token without saving any token to disk.

---

## Tool-Name Tolerance (agentic clients)

FreeBuff's trust system permanently caps any account it sees sending a
non-official tool schema (`third_party_client` sticky cap: trust level floors
at `verified`, spend ceiling drops to the restricted $0.50/day cohort), and
its free-mode gate can downgrade tool-bearing requests with no official tool
to a free fallback model. The proxy neutralizes this for you:

- Well-known third-party harness tool names are renamed to their official
  FreeBuff equivalents on the upstream wire — `read`/`read_file`/`view` →
  `read_files`, `write`/`write_to_file` → `write_file`,
  `bash`/`execute`/`execute_command`/`shell` → `run_terminal_command`,
  `edit`/`replace_in_file` → `str_replace`, `ls`/`list_files` →
  `list_directory`, `grep`/`search_files` → `code_search`,
  `todo`/`todowrite` → `write_todos`.
- Your parameter schemas pass through untouched, and the model's tool calls
  come back carrying YOUR original names — clients need zero changes.
- Unmapped custom tools pass through unchanged.

This is why Cline/Roo/Claude Code-style tool loops are safe to run through
the proxy without risking your account's trust tier.

---

## Access Tiers

FreeBuff assigns access tiers at the Cloudflare edge based on TCP source IP GeoIP (not HTTP headers):

- **Full tier** (`accessTier: "full"`): Tier-1 countries (US, UK, DE, JP, CA, etc.) with residential ASN. Access to premium models (`DeepSeek V4 Flash 07/31`, `GPT-5.6 Luna`, `DeepSeek V4 Pro`). **5 premium sessions/day base** (resets every 24h). Note: Upstream caps V4 Pro and Luna to 1 session/day, and MiniMax M3 is temporarily unavailable.
- **Limited tier** (`accessTier: "limited"`): Non-Tier-1 countries. All model requests coerced to `mimo/mimo-v2.5` (`MiMo 2.5`). **MiMo 2.5 stays unlimited across all tiers**.
Check your tier: the `/healthz` response includes access tier info when the last session admission carried it. The dashboard Overview page also shows it.

See [Getting Started — Access Tiers & Workarounds](getting-started.md#access-tiers--workarounds) for how to reach full tier from a limited-tier location.

---

## Default model

`deepseek/deepseek-v4-flash` is the default for full-tier accounts. As of 2026-08-18, it is restricted to full-tier only (upstream announcement).

For limited-tier accounts, **all model requests are coerced to `mimo/mimo-v2.5` by the upstream server** regardless of the model ID sent in `x-freebuff-model`. The proxy passes your requested model through unchanged — the coercion happens at FreeBuff's server layer, not in the proxy. The CLI exhibits identical behavior: it sends `deepseek/deepseek-v4-flash` and receives `model: mimo/mimo-v2.5` in the admission response (verified via MITM TLS interception).

Only request models your account's tier and region actually offers: out-of-tier picks are refused or downgraded (`model_unavailable`, `session_model_mismatch`), and the requested model id is correlated with your egress IP's region.

Query `http://localhost:3457/v1/models` for the full live catalog.
---

## Related docs

- [Getting Started](getting-started.md): 5-minute setup walkthrough
- [9router Integration](9router-integration.md): wiring the proxy into 9router
- [README](../README.md): overview, config reference, quick start
