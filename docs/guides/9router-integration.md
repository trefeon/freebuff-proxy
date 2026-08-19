# Wiring freebuff-proxy into 9router

This guide shows how to connect `freebuff-proxy` to **9router** as a custom OpenAI-compatible provider so you can route FreeBuff's free AI coding models through 9router.

```
9router (localhost:20128)
   │  /v1/chat/completions (Bearer api_key, model "freebuff/deepseek/deepseek-v4-flash")
   ▼
freebuff-proxy (localhost:3457)
   │  CLI session envelope & stealth headers
   ▼
Codebuff / FreeBuff Upstream
```

---

## ⚡ 60-Second Quick Setup

### Step 1: Ensure freebuff-proxy is Running
Verify the proxy is reachable before configuring 9router:
```bash
curl http://127.0.0.1:3457/healthz
# Expected response: {"status":"ok","models":15,...}
```

### Step 2: Add Custom Provider in 9router
1. Open 9router dashboard: **http://localhost:20128/dashboard/providers**
2. Under **Custom Providers (OpenAI/Anthropic Compatible)**, click **Add OpenAI Compatible**.
3. Fill in the form:

| Field | Value | Notes |
| :--- | :--- | :--- |
| **Name** | `freebuff` | Display label |
| **Prefix** | `freebuff` | Model prefix (e.g. `freebuff/deepseek/deepseek-v4-flash`) |
| **API Type** | **Chat Completions** | Responses API is also supported; Chat Completions is the tested default |
| **Base URL** | `http://127.0.0.1:3457/v1` | *See Docker table below if 9router runs in a container* |
| **API Key (Check)** | `not-needed` *(or your `cb_...` token)* | Used by the green **Check** validation button |
| **Model ID** | *(leave empty)* | Proxy provides its own `/v1/models` catalog |

Click **Save / Create**.

---

### Step 3: Add API Key / Connection Row

After creating the `freebuff` node, open it and click **Add API Key**:

| Field | Value (Pooled Mode) | Value (Bridge Mode) |
| :--- | :--- | :--- |
| **Name** | `Default Pool` | `Account 1 (cb_...)` |
| **API Key** | `not-needed` | Your actual FreeBuff token (`cb_...`) |
| **Default Model** | `deepseek/deepseek-v4-flash` | `deepseek/deepseek-v4-flash` |
| **Priority** | `1` | `1` (for Account 1), `2` (for Account 2) |

> [!IMPORTANT]
> **Connection Strategy with Multiple Keys (Bridge Mode)**:
> If you add multiple FreeBuff accounts in 9router, set connection strategy to **Fallback / Priority (Fill the first)**: **NEVER Round-Robin**.
> Round-robin drains all accounts simultaneously and triggers anti-farm ban detection. Fallback uses one account until its daily quota (`429`) is exhausted, then smoothly fails over to the next key.

> [!NOTE]
> **Keep the account count behind one public IP modest**: upstream caps distinct active users per egress IP (`ip_capped` → HTTP `429` admission). Accounts created from the same signup network (≥8 per /24) or sharing a mailbox (≥3) are permanently capped at a lower trust tier, so spread keys across different networks / egress IPs rather than stacking them on one connection. See the [Getting Started Guide](getting-started.md#important-safety-warning) for the full evidence-backed ban-avoidance rules.

---

## 🌐 Network & Base URL Matrix

The proxy listens on port `3457`. The Base URL in 9router depends on where 9router is running:

| Deployment Scenario | Base URL to enter in 9router |
| :--- | :--- |
| **9router and proxy on same host (native processes)** | `http://127.0.0.1:3457/v1` |
| **9router in Docker, proxy on host** | `http://host.docker.internal:3457/v1` *(or `http://172.17.0.1:3457/v1`)* |
| **Both 9router and proxy in Docker** | `http://freebuff-proxy:3457/v1` *(if in same Docker network)* |
| **Proxy on a remote VPS / LAN machine** | `http://<server-ip>:3457/v1` *(ensure port 3457 is open)* |

---

## 🤖 Recommended Models to Add

> Full per-model translation reference (wire ids, effort ladders, agent roots,
> capacity gates, and known gaps): [Model Translation Layer](model-translation-layer.md).

In the 9router provider node, you can add any of these models from the proxy catalog:

| Model ID in 9router | Description | Access Tier |
| :--- | :--- | :--- |
| `deepseek/deepseek-v4-flash` | High speed general coding model | **Full tier only** (limited tier restricted 2026-08-18) |
| `deepseek/deepseek-v4-pro` | Deep reasoning coding model | Full tier |
| `mimo/mimo-v2.5` | Fast lightweight coding model | **All tiers** (sole limited-tier model; all limited-tier requests coerced here server-side) |
| `openai/gpt-5.6-luna` | Deep reasoning + multimodal | Full tier |
| `minimax/minimax-m3` | High context window | Full tier |
| `z-ai/glm-5.2` | Advanced agentic model | Rate limited (5/20h) |

Clients calling 9router address these models as `freebuff/<model-id>`, for example:
```json
{
  "model": "freebuff/deepseek/deepseek-v4-flash",
  "messages": [{"role": "user", "content": "Write a python function"}]
}
```

> **Note for limited-tier accounts:** All model requests from limited-tier IPs are coerced to `mimo/mimo-v2.5` by the upstream server regardless of the model ID sent. For limited-tier setups, configure `mimo/mimo-v2.5` as the default model in 9router instead of `deepseek/deepseek-v4-flash` (which is restricted to full-tier only as of 2026-08-18).

---

## 🧪 Testing Your Setup

### 1. Test via cURL through 9router
```bash
curl -N http://localhost:20128/v1/chat/completions \
  -H "Authorization: Bearer <your-9router-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "freebuff/deepseek/deepseek-v4-flash",
    "messages": [{"role": "user", "content": "Hello from 9router!"}],
    "stream": true
  }'
```

### 2. Test in 9router Dashboard
Go to 9router **Chat** tab, select provider `freebuff` and model `freebuff/deepseek/deepseek-v4-flash`, and send a test message.

---

## 🛠️ Troubleshooting & Common Pitfalls

| Symptom | Root Cause | Solution |
| :--- | :--- | :--- |
| **Every request returns 404** | Old proxy binary without `/v1/responses` or `/v1/messages`, or an unsupported endpoint such as `/v1/embeddings` (400 `unsupported_endpoint`). | Upgrade to the latest release. Chat Completions, Responses, and Messages all work on v0.10.0+. |
| **Connection Refused on Base URL** | Proxy is not running or bound only to loopback inside Docker. | Run `curl http://127.0.0.1:3457/healthz`. In Docker, ensure `LISTEN_ADDR=:3457`. |
| **"URL not allowed" during Check** | 9router SSRF guard blocks private IPs when accessed from remote browser. | Ignore the check and click **Create** anyway, then add the API Key in the next modal. |
| **502 `upstream_auth_rejected`** | Token in `.env` or the 9router connection is expired or invalid. | Regenerate a token via `.\scripts\gen-token.cmd` (or `./scripts/gen-token.sh`). |
| **429 Rate Limited** | Daily account quota exhausted (resets at Pacific Midnight / 07:00 UTC). | In Bridge mode, 9router will auto-fallback to your next key. |
| **Truncated Reasoning / Tool Calls** | Model ran out of token generation budget. | Increase `max_tokens` (≥ 4000) in your client settings. |
| **Model shows as `region_limited`** | Limited-tier account (non-Tier-1 country IP). | Use `mimo/mimo-v2.5` as default model, or route through a residential Tier-1 proxy. See [Getting Started — Access Tiers](getting-started.md#access-tiers--workarounds). |

---

## 🔗 Related Guides
- [Getting Started Guide](getting-started.md)
- [Client Integration Guide](client-integration.md)
- [Admin Dashboard Guide](dashboard.md)
