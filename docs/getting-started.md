# Getting Started with freebuff-proxy (5-Minute Guide)

This guide takes you from zero to a working OpenAI-compatible proxy connected to FreeBuff.

---

## What is freebuff-proxy?

`freebuff-proxy` is a local bridge server. It sits between your favorite coding tools (OpenCode, pi, 9router, LiteLLM, or your own scripts) and FreeBuff's free AI models. Your tools talk standard OpenAI API to `localhost:3457`, and the proxy manages sessions and tokens behind the scenes.

```
+-------------------+      OpenAI API      +-------------------+      FreeBuff      +-------------------+
| Your AI Client    | -------------------> | freebuff-proxy    | -----------------> | codebuff.com      |
| (OpenCode / pi)  | <------------------- | (localhost:3457)  | <----------------- | (Free Models)     |
+-------------------+      SSE Streams     +-------------------+     CLI Envelope   +-------------------+
```

---

## What you'll do (the flow)

Using freebuff-proxy is five steps, most of them one command:

1. **Get a FreeBuff account + token (`cb_...`)**: the official CLI or `scripts/gen-token.*` does this for you.
2. **Install the proxy**: one command (below).
3. **Choose your mode**: one user with your own account(s) → **pooled** (`AUTH_TOKENS=cb_...`); a router serving many users → **bridge** (leave `AUTH_TOKENS=` empty).
4. **Run and verify**: `./freebuff-proxy`, then `curl http://127.0.0.1:3457/healthz`.
5. **Connect your AI tool**: point it at `http://127.0.0.1:3457/v1`, model `deepseek/deepseek-v4-flash`.

---

## Important Safety Warning

Using this proxy conflicts with Codebuff's terms of service. Upstream abuse detection scans for automation patterns and suspends accounts. Detection is documented in the open-source FreeBuff client: per-request IP scoring, per-account trust levels with sticky caps, daily spend ceilings, and mass sweeps against known farm shapes. The rules below are the evidence-backed dos and don'ts:

| ✅ Do | ❌ Don't |
|---|---|
| **Keep `SAFE_MODE=true`** (default; anti-ban stealth: TLS fingerprint, header sanitization, request jitter, idle rotation) | **Don't** run 24/7 on heavy unattended automated tasks |
| Use a **normal residential connection** | **Don't use a VPN / proxy / Tor**. FreeBuff determines access tier via **TCP source IP GeoIP at the Cloudflare edge** — not HTTP headers. Header spoofing (`X-Forwarded-For`, `CF-Connecting-IP`) is impossible because Cloudflare overwrites them at L4. VPN and datacenter IPs are detected via MaxMind/Spur Intelligence ASN database (`ipPrivacySignals: ["vpn"]`) and placed in a restricted cohort ($0.50/day spend ceiling, ≈1 session/day) or hard-blocked |
| Request **only models your tier/region offers** | **Don't request out-of-region models**: on limited-tier accounts (non-Tier-1 countries), **all model requests are coerced to `mimo/mimo-v2.5` server-side** regardless of what you send in `x-freebuff-model`. Verified via MITM: CLI sends `deepseek/deepseek-v4-flash`, server responds with `model: mimo/mimo-v2.5` in admission. This is upstream behavior, not proxy behavior |
| Keep **one modest account** | **Don't create spam clusters**: upstream caps distinct active sessions per egress IP (`ip_capped`); accounts from the same signup network (≥8 per /24) or mailbox (≥3) are permanently capped at lower trust levels |
| **Use one key until it is rate-limited** | **Don't rotate several healthy keys aggressively** (farming signal) |
| Register with a **real email address** (e.g. Gmail) | **Don't use temp-mail**. Documented ban cohort: 6,699 of 7,129 accounts on flagged domains already banned |
| Read a `429` as **quota, resets Pacific midnight** (proxy locks the token locally, answers in `<1ms`) | **Don't confuse it with a ban**: only `403` `banned`/`country_blocked` means the account is gone; use a new established account |
| Budget **4-5 keys for 24h of coding** | **Don't** expect more than one key ≈ one day of moderate use |

---

## Access Tiers, Models & Upstream Quotas

FreeBuff assigns an access tier at the Cloudflare edge based on your TCP source IP's GeoIP location:

- **Full tier** (`accessTier: "full"`): Tier-1 countries (US, UK, DE, JP, CA, AU, etc.) with a residential/ISP ASN. Access to all premium models including `deepseek/deepseek-v4-flash`. **5 premium sessions/day base** (resets every 24h / Pacific midnight; streaks and trust ladders can raise this further).
- **Limited tier** (`accessTier: "limited"`): Non-Tier-1 countries (e.g. `countryCode: ID` → `countryBlockReason: "country_not_allowed"`). All model requests coerced to `mimo/mimo-v2.5` (`MiMo 2.5`). **3 limited sessions/day** (level ladder up to **7**).

### Current Upstream Model Status & Quotas

> **📢 Official Freebuff Upstream Notice**:
> *"DeepSeek costs have spiked, so limits are tighter for now: V4 Pro and GPT-5.6 Luna are 1 session a day, V4 Pro pauses at peak times, and MiniMax M3 is unavailable. MiMo 2.5 stays unlimited. —Freebuff Team"*

| Category | Model Name | Wire Model ID | Specs & Upstream Quota Policy |
|---|---|---|---|
| **Premium** | **DeepSeek V4 Flash 07/31** *(Recommended)* | `deepseek/deepseek-v4-flash` | **Smart & Fast**, Reasoning: `high`, `NEW`. Uses standard 5 sessions/day premium pool. |
| **Premium** | **GPT-5.6 Luna** | `openai/gpt-5.6-luna` | **Strong all-around**, Reasoning: `high`, Images. **Strictly capped at 1 session/day**. |
| **Premium** | **DeepSeek V4 Pro** | `deepseek/deepseek-v4-pro` | **Deep reasoning**, Reasoning: `high`. **Strictly capped at 1 session/day; pauses at peak times**. |
| **Unlimited**| **MiMo 2.5** | `mimo/mimo-v2.5` | **Balanced**, Images. **Unlimited across all tiers** (sole active model on limited tier). |
| **Referral** | **GLM 5.2** | `z-ai/glm-5.2` | **Top open-source agentic model**. Referral-gated (+1 session per friend referred; streak bonuses). |
| **Disabled** | **MiniMax M3** | `minimax/minimax-m3` | **Temporarily Unavailable** upstream due to server-side cost constraints. |
### Workarounds for limited-tier IPs

**Option A — Tailscale / WireGuard exit node (free, best option):**
Route traffic through a residential connection in a Tier-1 country. If you have a device (home PC, family member's machine) in the US/UK/DE/JP, install [Tailscale](https://tailscale.com/) on both machines and enable exit node on the remote one. All proxy traffic exits from that residential IP. Cost: $0.

**Option B — VPS/LAN: residential egress at the network layer (direct egress only):**
The proxy is **direct-egress only** — there is no HTTP/SOCKS proxy support (the upstream transport forces `transport.Proxy = nil`, and `.env` values are never exported to process env). For a VPS/LAN box whose own IP is datacenter or non-Tier-1, route the whole box through a WireGuard/Tailscale tunnel to a residential exit node in a Tier-1 country so its own routing egresses residential; the proxy needs no config, it just rides the tunnel. Never mint tokens from datacenter egress either (issue #140 P0) — residential egress is the safe minting path.

**Option C — Multi-token pooling (no VPN needed):**
Stay on limited tier but maximize throughput. Set `AUTH_TOKENS=token1,token2,token3,token4,token5` in `.env` with 4-5 accounts. Each gets 3 sessions/day on `mimo/mimo-v2.5` (base; the 0.0.150 trust-level ladder can raise a token up to 7/day), giving you ~12-15 usable sessions per day.

> **Shared-network cap (issue #140 P2b):** all pooled tokens in one deployment
> share the proxy's egress, so its accounts sit on one /24 by construction.
> Upstream's `shared_signup_network` trust cap **permanently limits accounts**
> once ~8 share a subnet — `-doctor` prints a shared-network advisory whenever
> two or more tokens are configured. For full-trust isolation, route distinct
> accounts through distinct residential exits (Option A per machine).

See the [Getting Started — Access Tiers](#access-tiers--workarounds) section for per-model quota pools and effort ladders.

**Do NOT use any of these — they trigger the restricted cohort or an outright ban:**
- Commercial VPN (NordVPN, ExpressVPN, Surfshark, etc.)
- Datacenter VPS (AWS, DigitalOcean, Hetzner, Vultr)
- Tor exit nodes

All of the above resolve to known datacenter/VPN ASNs in MaxMind/Spur Intelligence and produce `ipPrivacySignals: ["vpn"]` at the Cloudflare edge.


## Step 1: Install & Start the Proxy

### Option A: Portable Release ZIP (Recommended for Windows / Beginners)
1. Download the latest ZIP from [**GitHub Releases**](https://github.com/trefeon/freebuff-proxy/releases) (e.g. `freebuff-proxy_..._windows_amd64.zip`).
2. Extract the folder and:
   - **Windows**: Double-click `start-proxy.cmd`.
   - **Linux / macOS**: Open terminal in the folder and run `./start-proxy.sh`.
3. Press Enter to sign in via your browser. Your token is saved automatically!

---

### Option B: One-Command Online Installer

**Linux / macOS Terminal:**
```bash
curl -sSL https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.sh | bash
```

**Windows PowerShell:**
```powershell
irm https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.ps1 | iex
```

---

### Option C: Docker Compose

```bash
cp .env.example .env
# Edit .env and set AUTH_TOKENS=your_token
git fetch --tags 2>/dev/null || true
VERSION=$(git describe --tags 2>/dev/null || echo dev) docker compose up -d --build
```

---

## Step 2: Verify It Works

Run the diagnostic tool or curl:

```bash
# Diagnostic doctor check: config, port, DNS/TLS, registry, plus a
# zero-cost validity probe per token (no upstream session is claimed):
./freebuff-proxy -doctor

# Standalone token probe: zero-cost GET probe on the first token (no
# session claimed), prints live quota, exit 0/1 (handy for installers):
./freebuff-proxy -test-token

# Quick health check (JSON: status, uptime, model count, per-token snapshot):
curl http://localhost:3457/healthz

# Prometheus metrics scrape endpoint:
curl http://localhost:3457/metrics

# List available models:
curl http://localhost:3457/v1/models
```

`/healthz` returning status `200` means the proxy is running and reachable. It does **not** validate your token. Use `./freebuff-proxy -test-token` (or the dashboard smoke test on the Overview page) to prove a token is valid before your first chat; `-doctor` runs the same zero-cost per-token validity probe by default.

`/healthz` also reports each token's live per-model quota (`quota` map) when the last session admission carried it.

## Step 3: Connect Your Favorite AI Client

Point your AI tool to:
- **Base URL:** `http://localhost:3457/v1`
- **API Key:** `not-needed` (or your token in bridge mode)
- **Model:** `deepseek/deepseek-v4-flash` (full-tier only — limited-tier IPs are coerced to `mimo/mimo-v2.5`; see [Access Tiers](#access-tiers--workarounds))

Fastest path: run `./freebuff-proxy -setup` to write the client config automatically.

See the [Client Integration Guide](client-integration.md) for copy-paste config for OpenCode, pi, 9router, LiteLLM, and more.

---

## Troubleshooting

Run `./freebuff-proxy -doctor` to diagnose problems automatically.

| Error / Symptom | Cause & Fix |
|---|---|
| `403` + `free_mode_cli_required` | The request was missing the CLI system prompt marker or envelope. The proxy injects this automatically. Update to the latest version. |
| `502` + `upstream_auth_rejected` | Token in `.env` is expired or invalid. Catch it before the first chat: `./freebuff-proxy -test-token` (or `-doctor`) probes the token with a zero-cost GET request and fails with a clear message. Then re-run `freebuff` to log in and update `AUTH_TOKENS`, or swap the token live on the dashboard Tokens page (no restart). |
| Connection refused | Proxy is not running, or in Docker without `LISTEN_ADDR=:3457`. |
| `403 account_banned` | Account suspended upstream. Token is dead; use a new established account. |
| `502` + `free_mode_legacy_luna_agent` | The conversation uses a retired Luna agent. Start a new conversation. The proxy automatically retries with a fresh session. |

---

## Related docs

- [Client Integration](client-integration.md): OpenCode, pi, 9router, LiteLLM, or your own scripts
- [9router Integration](9router-integration.md): wiring the proxy into 9router
- [README](../README.md): overview, config reference, quick start
