# Wiring freebuff-proxy into 9router

**Goal**: make 9router (and any OpenAI client) use FreeBuff's free models through
freebuff-proxy. Your FreeBuff auth token acts as the credential behind the proxy; 9router sees
an ordinary **OpenAI-compatible custom provider**.

```
9router (localhost:20128)
   │  /v1/chat/completions  (Bearer api_key, model "freebuff/deepseek-v4-flash")
   ▼
freebuff-proxy (:3457, OpenAI-compatible surface)
   │  x-freebuff-model / x-freebuff-instance-id / codebuff_metadata envelope
   ▼
codebuff.com (FreeBuff free tier, token-bound)
```

This guide documents every option that exists in 9router v0.5.50 (2026-08-05) for adding and
running a custom provider. Option names and defaults below were verified against the 9router
source code (dashboard modals, API routes, `open-sse/` fallback engine, `.env.example`).

---

## 1. Prerequisites: the proxy must be running and reachable

1. Get a token (see README, *Getting a token*: `freebuff.llm.pm` or
   `~/.config/manicode/credentials.json`).
2. Run freebuff-proxy **with the token** (any of these):
   - **Same machine as 9router (recommended):** build + run, or the systemd unit below
   - **Docker:** `docker compose up --build -d` (the compose file sets `LISTEN_ADDR=:3457`
     and publishes port 3457 on the host)
   - **Remote host / VPS:** run it there and open the firewall/NSG for port 3457
3. Verify before touching 9router:

   ```bash
   curl http://127.0.0.1:3457/healthz     # {"models":15,"tokens":[...],"uptime_seconds":N}
   curl http://127.0.0.1:3457/v1/models   # the model catalog (~15 at boot)
   ```

**Base URL, which one to use in 9router:** the proxy listens on port **3457**; only the
*host* part of the URL changes:

| Where the proxy runs | Base URL in 9router |
|---|---|
| **Same machine as 9router**, plain process (binary / systemd) **or** Docker Compose | `http://127.0.0.1:3457/v1` |
| **9router itself runs in Docker** on the same host | `http://<docker-bridge-gateway>:3457/v1`; from inside a container the host is the bridge gateway (commonly `172.17.0.1`/`172.18.0.1`; find it: `docker network inspect <proxy-network> --format '{{(index .IPAM.Config 0).Gateway}}'`) |
| **Another machine on your LAN** (e.g. a homelab box) | `http://<that-host-ip>:3457/v1` (e.g. `http://192.168.10.3:3457/v1`) |
| **A VPS / remote server** | `http://<vps-ip-or-domain>:3457/v1` (firewall must allow 3457; if the container binds loopback, set `LISTEN_ADDR=:3457` in its env) |

**When in doubt, use `http://127.0.0.1:3457/v1`**; it is correct whenever the proxy runs on
the same machine as 9router *as a plain process*. If 9router is containerized, use the
gateway IP above (the `scripts/setup-proxy-docker.sh` installer detects and prints it for
you).

Example systemd unit (Ubuntu/Debian, same host as 9router):

```ini
[Unit]
Description=freebuff-proxy (FreeBuff OpenAI-compatible bridge)
After=network-online.target

[Service]
Type=simple
User=<your-user>
WorkingDirectory=/home/<your-user>/freebuff-proxy   # .env auto-loads from here
ExecStart=/home/<your-user>/freebuff-proxy/freebuff-proxy
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
```

```bash
sudo cp freebuff-proxy.service /etc/systemd/system/ && sudo systemctl enable --now freebuff-proxy
```

### 1.1 Bridge mode (no proxy token)

Don't want to put your FreeBuff token in the proxy's `.env`? Leave `AUTH_TOKENS=` **empty**
(bridge mode) — the proxy holds no token of its own and relays each request with the token
the client sends:

- 9router **API Key** (node + Add API Key): use **your FreeBuff token** (not a placeholder).
  9router forwards it as `Authorization: Bearer <key>`, and the proxy relays that exact
  token upstream. One 9router key = one proxy session = one FreeBuff account.
- Everything else (Base URL, API Type, Model ID, Default Model) is unchanged from sections 1/3.
- `/v1/models` and `/healthz` need no header; `API_KEYS` is ignored in bridge mode.
- Sessions/runs are created lazily per token and reused; the cache is capped at 32 tokens
  with LRU eviction and ~2h idle eviction. Ban/quota errors stay per account (403/429/503).
- Want several FreeBuff accounts? Add several 9router keys — each carries its own token.

Verify with curl before touching 9router (no token needed for models/healthz):

```bash
curl http://127.0.0.1:3457/healthz   # {"models":15,"tokens":[],"bridge_tokens":0,...}
curl -N http://127.0.0.1:3457/v1/chat/completions \
  -H "Authorization: Bearer <your-freebuff-token>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

---

## 2. Install 9router

**Option A, npm (quickest):**

```bash
npm install -g 9router
9router          # dashboard opens at http://localhost:20128
```

**Option B, from source:**

```bash
git clone https://github.com/decolua/9router && cd 9router
cp .env.example .env          # then set JWT_SECRET and INITIAL_PASSWORD (default is 123456!)
npm install
npm run build
PORT=20128 HOSTNAME=0.0.0.0 npm run start
```

**Option C — Docker (published image):**

Multi-platform images (`linux/amd64`, `linux/arm64`) are published to Docker Hub
(`decolua/9router`) and GHCR (`ghcr.io/decolua/9router`):

```bash
docker run -d \
  --name 9router \
  -p 20128:20128 \
  -v "$HOME/.9router:/app/data" \
  -e DATA_DIR=/app/data \
  decolua/9router:latest
```

→ Open http://localhost:20128. Container defaults: `PORT=20128`, `HOSTNAME=0.0.0.0`.

- **Data persistence:** `$HOME/.9router/db/data.sqlite` on the host maps to
  `/app/data/db/data.sqlite` in the container. Set `JWT_SECRET` and `INITIAL_PASSWORD`
  via `-e` to keep sessions stable across restarts.
- **Useful commands:** `docker logs -f 9router`, `docker restart 9router`,
  `docker stop 9router && docker rm 9router`, `docker pull decolua/9router:latest` to update.
- **9router in Docker + freebuff-proxy on the host:** use the compose bridge gateway as
  the proxy's Base URL (section 1) — the container reaches the host via the gateway IP
  (commonly `172.17.0.1`/`172.18.0.1`).
- **Freebuff-proxy also in Docker:** both on the same bridge network; the proxy's compose
  publishes port 3457, so use the proxy container's network-gateway Base URL
  (print it with `scripts/setup-proxy-docker.sh`).
- The reference repo ships a `docker-compose.yml` that also starts a **headroom** companion
  container (`ghcr.io/chopratejas/headroom`, port 8787, `HEADROOM_URL=http://headroom:8787`)
  for the RTK headroom saver — optional; freebuff-proxy does not need it.

Data lives in `DATA_DIR` (SQLite). Default port: **20128** (dashboard `/dashboard`, API `/v1`).
On first run (v0.5.50+) 9router auto-provisions a **Default Key** for the dashboard.

### Environment options (from `.env.example` / source)

| Variable | Default | Notes |
|---|---|---|
| `JWT_SECRET` | auto-generated file | Dashboard session signing. Required for stable sessions; without it 9router writes `DATA_DIR/jwt-secret` and sessions reset when it rotates |
| `INITIAL_PASSWORD` | `123456` | Dashboard login password on first run. **Change it.** |
| `DATA_DIR` | `~/.9router` | Data directory. Windows: `%APPDATA%/9router`; Docker: `/app/data`. Unix-style paths are ignored on Windows |
| `PORT` | `20128` | HTTP port |
| `HOSTNAME` | `0.0.0.0` (prod) | Bind address |
| `NODE_ENV` | `production` | `development` enables dev-only behavior |
| `API_KEY_SECRET` | auto | Secret for hashing client API keys at rest |
| `MACHINE_ID_SALT` | auto | Salt for machine-bound key generation |
| `ENABLE_REQUEST_LOGS` | off | Per-request logging |
| `OBSERVABILITY_ENABLED` | off | OpenTelemetry export (documented inconsistently; check the env example) |
| `AUTH_COOKIE_SECURE` | off | Set when serving dashboard over HTTPS |
| `REQUIRE_API_KEY` | on | When off, clients can call `/v1` without a key ("local mode") |
| `BASE_URL` / `CLOUD_URL` |  | Public URL / cloud mode (not needed for local use) |
| `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` / `NO_PROXY` |  | Outbound proxy for 9router's own upstream calls (not per-provider) |
| `SEARXNG_URL` |  | Optional search backend for `/v1/search` |
| `HEADROOM_URL` | `http://localhost:8787` | Companion token-saver service (see RTK section) |

Runtime timeouts (configurable): `STREAM_STALL_TIMEOUT_MS` (360000), `STREAM_FIRST_CHUNK_TIMEOUT_MS`
(200000), `FETCH_CONNECT_TIMEOUT_MS` (60000). CLI flags: `--port`, `--no-browser`,
`--skip-update`.

Dashboard settings (Settings page, stored in the DB): `requireLogin`, `requireApiKey`
(both default on), `authMode` (`password` or `oidc`), tunnel/tailscale/cloud-sync,
outboundProxy, observability, quotaVisibility, dnsToolEnabled.

---

## 3. Add freebuff-proxy as a provider (dashboard)

1. Open **http://localhost:20128/dashboard/providers**
2. Under **Custom Providers (OpenAI/Anthropic Compatible)** click **Add OpenAI Compatible**
3. Fill the form. Field-by-field (verified against `AddCompatibleModal.js`):

   | Field | Value for freebuff-proxy | Why (from 9router source) |
   |---|---|---|
   | **Name** | `freebuff` | Required. Friendly label only. |
   | **Prefix** | `freebuff` | Required. Becomes the model-id prefix: model combos are `freebuff/<model-id>` |
   | **API Type** | **Chat Completions** | `chat` only. The proxy implements `/v1/chat/completions`; **Responses API (`responses`) is NOT supported** and makes every request 404 |
   | **Base URL** | see the host table in section 1; must end in `/v1` | 9router appends `/models`, `/chat/completions` to it at runtime |
   | **API Key (for Check)** | any non-empty value (e.g. the FreeBuff token) | Used ONLY by the **Check** button; it is **not saved** with the node. Empty proxy `API_KEYS` accepts any value |
   | **Model ID (optional)** | **leave empty** | Fallback model used for validation when the provider has no `/models` endpoint. The proxy has `GET /v1/models`, so the /models check succeeds and enumerates the catalog (15 models at boot) |

   What the **Check** button does: `POST /api/provider-nodes/validate` sends `{baseUrl,
   apiKey, type, modelId}` and 9router does `GET {base}/models` (Bearer), falling back to
   `POST {base}/chat/completions`; 10s timeout. Green badge = proxy reachable.

   > **SSRF guard: local vs remote dashboard.** The validation endpoint runs
   > `assertPublicUrl`, which rejects private base URLs (`127.0.0.1`, `172.17.x`, `192.168.x`,
   > `10.x`), but **only when the dashboard request is not local**. From a browser on the
   > same machine as 9router, `http://127.0.0.1:3457/v1` validates fine. From a *different
   > machine* (e.g. browsing `http://192.168.10.3:20128` from your laptop), the same URL is
   > rejected with "URL not allowed" even though the proxy is reachable. The **Create**
   > button does not validate the URL (only name/prefix/baseUrl are required), so create the
   > node anyway; the real configuration happens in the next step (Add API Key), which
   > validates through `POST /api/providers/validate` with **no SSRF guard**, so private URLs
   > pass there. If you insist on Check passing from a remote dashboard, use a public URL
   > (reverse proxy or tunnel).

   **Create** saves `POST /api/provider-nodes` `{name, prefix, apiType:"chat", baseUrl,
   type:"openai-compatible"}`; no key is stored on the node, so you must add one next.

4. After **Create**, open the `freebuff` node and click **Add API Key** (verified against
   `AddApiKeyModal.js`):

   | Field | Value | Notes |
   |---|---|---|
   | **Name** | any label (e.g. `freebuff account`) | Required for compatible providers; the submit button is disabled without it |
   | **API Key** | any non-empty value | Same rule as section 3. Required |
   | **Region (optional)** | leave empty | Only for providers that define regions |
   | **Default Model** | `deepseek/deepseek-v4-flash` | **Required for compatible nodes.** The **raw** model id the proxy endpoint expects (no `freebuff/` prefix; 9router passes it to `POST {base}/chat/completions` verbatim) |
   | **Priority** | `1` (default) | Integer. Lower number wins; with several keys it is priority-order fallback (or round-robin, see section 5) |
   | **Proxy Pool** | `__none__` (default) | Optional outbound proxy pool for this connection (9router's own proxies, not the proxy's `HTTP_PROXY`) |

   **Bulk mode**: paste `name|apiKey` lines into the textarea to add many keys at once
   (a bare key on a line is auto-named `Key N`). Save payload:
   `{name, apiKey, defaultModel, priority, proxyPoolId, testStatus}`.

   **Multiple keys per node**: supported. Each key becomes a separate connection row used in
   priority order, or round-robin if you enable the provider's round-robin toggle. You can
   point several FreeBuff tokens here, but the proxy already pools tokens itself, so one
   connection with a placeholder key is enough.

   The **Default Model** is used for the node's validation and inference tests. Other models
   from `/v1/models` (section 6) can be added to the node afterwards and are addressed as
   `freebuff/<model-id>`.

Equivalent raw config shape (config-file or headless setups; this is the object 9router
persists in its DB):

```json
{
  "freebuff": {
    "base_url": "http://127.0.0.1:3457/v1",
    "api_key": "any-placeholder-or-your-API_KEYS-value",
    "models": ["deepseek/deepseek-v4-flash"]
  }
}
```

---

## 4. Manage the node after creation (all options)

On the provider page, per node and per connection (verified against `providers/[id]/page.js`):

- **Edit node**: change name / prefix / API type / base URL. Edits are propagated into every
  connection's `providerSpecificData.{prefix, apiType, baseUrl, nodeName}`.
- **Delete node**: cascades to all connections.
- **Connection row**:
  - **Active toggle** (`isActive`): take the key out of rotation without deleting it.
  - **Priority up/down**: reorder keys; with round-robin off, 9router tries them in order.
  - **Test** per connection: `POST /api/providers/[id]/test` runs a one-off probe with that
    key (this is the SSRF-free validation path).
  - **Test all** (one by one), **bulk delete**.
  - **Apply Proxy**: bind a single proxy pool, rotate across pools, or unbind.
- **Round Robin toggle** (per provider): rotates across connections per request instead of
  strict priority. Global default sticky limit: `stickyRoundRobinLimit: 3` (same connection
  serves up to N consecutive requests before rotating).
- **Models section** (compatible providers use the custom-models list):
  - **Add Model** (`AddCustomModelModal`): `POST /api/models/custom {providerAlias, id,
    type}`; add any catalog id from section 6.
  - **Aliases**: map an alias to a real model id (`POST /api/models/alias`) so you can expose
    e.g. `freebuff/flash` instead of `freebuff/deepseek/deepseek-v4-flash`.
  - **Disable / enable** individual models without deleting them.
  - **Test model**: probe a single model through the node.
  - **Suggested models import**: pull the catalog the proxy advertises via `/v1/models`.

---

## 5. Combos and fallback tiers

A **combo** is an ordered list of model strings with a name (`^[a-zA-Z0-9_.\-]+$`, unique);
`kind` is optional. **Tiers are a README concept, not a field**: ordering is just list
position, so a custom provider's models slot in anywhere (e.g. `freebuff/...` after your paid
providers, before nothing, or as a dedicated free tier).

- **Global strategy** (`comboStrategy`, default `fallback`): try models in list order.
- **Per-combo overrides** (`fallbackStrategy`): `round-robin` or `fusion` (combine several
  responses; needs `judgeModel` / `fusionTuning`).
- **Round-robin stickiness**: `comboStickyRoundRobinLimit`, default 1.
- **Capability auto-switch**: 9router reorders for vision/pdf/audio/video based on request
  content; the capacity adapter defaults to vision/audio enabled with
  `oc/mimo-v2.5-free` fallback.

**Retire / skip / backoff rules** (verified against `accountFallback.js` + `errorConfig.js`):

| Signal | 9router behavior |
|---|---|
| `401` / `402` / `403` / `404` from a connection | Connection locked for 2 minutes; next connection tried |
| `429` rate limit | Exponential backoff: 2s x 2^n, max 5 min |
| Text rules: rate limit / quota / capacity / overloaded | Same backoff, applied at level <= 15 |
| "No credentials" | Connection skipped for 2 minutes |
| Any other error (incl. `502` / `503` / `504`) | No status rule matched; next connection tried with a 30s transient cooldown (the 5s cooldown in the source applies only to "request not allowed" text errors) |
| All models failed | Client gets `503` with the earliest `retryAfter` |
| Per-model locks | A failing model is locked per `modelLock_<model>` so other models keep working |

The proxy itself also returns `503 waiting_room_queued` + `Retry-After` and
`429 rate_limited` + `resetAt`, which 9router's fallback engine understands.

---

## 6. RTK token saver and companion savers

**RTK token saver** compresses `tool_result` content **in place, before format translation**
(OpenAI `role:"tool"` messages, Claude `tool_result` blocks, Responses
`function_call_output`). Filters: git-diff, git-status, git-log, grep, find, ls, tree,
dedup-log, smart-truncate, read-numbered, search-list. It auto-detects content (peeks 1KB),
never grows or empties a result, and respects `MIN_COMPRESS_SIZE` / `RAW_CAP`.

- **Default: enabled** (`rtkEnabled: true`). Toggle in Dashboard -> Endpoint settings.
- **Per-request bypass**: send header `x-9router-token-saver: off`.
- **Why it matters here**: compressing `tool_result` before it reaches the proxy saves
  FreeBuff quota too. Leave it on.

Companion savers (settings-row flags, off by default): **headroom** (`headroomEnabled`,
`headroomUrl` default `http://localhost:8787`), **caveman** (`cavemanEnabled/Level`),
**ponytail** (`ponytailEnabled/Level`), pxpipe.

---

## 7. Client access (other OpenAI clients)

Any OpenAI client can use 9router as its endpoint:

| Setting | Value |
|---|---|
| Base URL | `http://localhost:20128/v1` |
| API key | any key from the dashboard Keys page (or none if `requireApiKey` is off, "local mode") |
| Auth | `Authorization: Bearer <key>` |

Public API prefixes: `/v1`, `/v1beta`, `/api/v1`, `/codex`. Available routes (9router side):
`/v1/chat/completions`, `/v1/responses` (+`/compact`), `/v1/messages` (+`/count_tokens`),
`/v1/models` (+`/[kind]`, `/info`), `/v1/embeddings`, `/v1/audio/{speech,transcriptions,
voices}`, `/v1/images/generations`, `/v1/videos/*`, `/v1/search`, `/v1/web/fetch`.

Model ids on the client side: `<provider-prefix>/<model>` (e.g.
`freebuff/deepseek/deepseek-v4-flash`), or a combo name alone.

> The proxy only implements chat completions + models. If you call 9router's `/v1/responses`
> or `/v1/messages` with a freebuff model, 9router falls back to other providers or errors.

---

## 8. Model catalog (boot fallback, 2026-08-12)

Served by `GET /v1/models` (parsed from `CodebuffAI/codebuff` TS sources, refreshed every 6h,
fallback at boot). Register any subset in 9router:

| Model ID | Notes |
|---|---|
| `deepseek/deepseek-v4-flash` | CLI default; full **and** limited access; fastest |
| `deepseek/deepseek-v4-pro` | full access; deeper reasoning |
| `openai/gpt-5.6-luna` | full access; deep reasoning + image support |
| `minimax/minimax-m3` | full access; fast + image support |
| `mimo/mimo-v2.5` | full **and** limited access |
| `z-ai/glm-5.2` | earned sessions; **rate-limited to 5 sessions / 20h** (HTTP 429 `rate_limited`) |
| `poolside/laguna-s-2.1`, `openrouter/poolside/laguna-s-2.1` | catalog additions since 2026-08; pending tier probing |
| `inclusionai/ling-3.0-flash:free` | catalog addition; pending tier probing |
| `crof/greg-2-ultra`, `crof/greg-2-super` | catalog additions; pending tier probing |
| `anthropic/claude-fable-5` | catalog addition; may be restricted per tier |
| `google/gemini-2.5/3.1/3.5-flash-lite` | specialist subagents (file finding/research); not a general chat model |

> The live catalog refreshes every 6h and can differ slightly from this boot fallback;
> always trust `GET /v1/models` for the current list.

Quota reality (local dev note: `docs/research/freebuff-limitations.md`, gitignored):
- **Limited mode** (some regions / VPN / datacenter IPs): DeepSeek V4 Flash + MiMo 2.5 only,
  6 one-hour sessions/day.
- **Full mode**: all models; ~5 one-hour sessions/day for premium models (MiniMax unlimited,
  GLM 5/20h). One proxy session serves many requests across models, so a normal day burns 1-3.
- The proxy does **not** tier-filter `/v1/models`; a model that errors upstream is a
  tier/quota issue, not a proxy bug.

---

## 9. Verification

Through 9router (the model id carries the provider prefix):

```bash
curl -N http://localhost:20128/v1/chat/completions \
  -H "Authorization: Bearer <9router-api-key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"freebuff/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

Also verify in the 9router dashboard chat: pick `freebuff/deepseek-v4-flash` and send a
message.

**9router settings that matter:**
- **RTK token saver**: leave enabled (it compresses `tool_result` before it reaches the
  proxy; saves FreeBuff quota too).
- **max_tokens**: reasoning models think before they answer; set a generous `max_tokens`
  (>= 4k) for `deepseek-*`, `gpt-5.6-luna`, `glm-5.2` or tool calls get truncated
  (`finish_reason: "length"`, observed live).
- **Fallback tiers**: freebuff fits a FREE tier under your paid providers in 9router
  combo/fallback chains (see section 5).

---

## 10. Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `502` wrapping `403 free_mode_cli_required` | Upstream's CLI-only gate on the free tier (since ~2026-08-03). Not a 9router or proxy misconfiguration: `/healthz` and `/v1/models` stay 200 and sessions/runs still succeed. Nothing in the proxy config bypasses it; see the README FAQ for the full list of what was tested against a live token |
| `502` wrapping `401`/`404 Invalid API key or user not found` | The token in the proxy `.env` is invalid, expired, or the account no longer exists. Replace it with a fresh token |
| 9router: connection refused on base_url | Proxy not running, wrong port, or firewall. Check `systemctl status freebuff-proxy` / `docker compose ps`; `curl http://127.0.0.1:3457/healthz`. If the proxy runs via plain `docker run`, it also needs `-e LISTEN_ADDR=:3457` or it binds loopback inside the container |
| "URL not allowed" on **Check** from a remote browser | The SSRF guard rejects private URLs for non-local dashboard requests (section 3). Click Create anyway and validate via **Add API Key**, which has no SSRF guard; or use a public URL |
| 401 from the proxy | `API_KEYS` is set in the proxy `.env` and 9router's api_key does not match one of them |
| `400 model_not_found` (proxy) | Model not in the registry catalog. Check `/v1/models` |
| `404 unknown model` (9router) | The model combo is not registered. Re-add the model in the provider config |
| `503 waiting_room_queued` + Retry-After | FreeBuff waiting room (quota/hourly). Normal; 9router/opencode retry automatically |
| `429` with `rate_limited` | GLM 5/20h cap, or token daily quota (6 sessions on the limited tier). Switch model or wait; 9router backs off with the proxy's `resetAt` |
| `502 upstream_unavailable` | Token in 30-min cooldown after a 401, or all tokens failed. Check `healthz` |
| `403` `account_banned` / `{"status":"banned"}` | The FreeBuff account was banned upstream (terminal per official source; see the README **WARNING**). The proxy skips the token during the ban window (upstream `resumes-at`, or 24h) and re-probes once after it; if it still fails, rotate to a new account with an established GitHub login and a clean IP |
| Model streams `reasoning_content` | By design (CLI-faithful). 9router handles it; don't strip it |
| 9router shows provider disconnected | Proxy restarted mid-flight; sessions recover transparently on the next request |
| Every request returns `404` | Provider API type is set to **Responses**; the proxy only implements Chat Completions (section 3) |
| Can't log in to the dashboard | `INITIAL_PASSWORD` default is `123456`; change it. If `JWT_SECRET` rotates, sessions reset |

---

## 11. References

- This project: README (getting a token, config table), this guide. The PRD, reference
  analysis and quota-research notes are **local-only dev docs** (gitignored).
- 9router: github.com/decolua/9router (`npm i -g 9router`, dashboard on :20128, data in
  `~/.9router/`; v0.5.50, 2026-08-05). Option inventory in this guide was verified against
  `AddCompatibleModal.js`, `AddApiKeyModal.js`, `providers/[id]/page.js`,
  `open-sse/{services/combo.js,services/accountFallback.js,rtk/index.js}`,
  `src/lib/db/repos/settingsRepo.js`, `.env.example`, CHANGELOG.
- ToS note: FreeBuff's terms prohibit third-party wrappers/endpoints; educational/personal
  use only, bans possible. Keep usage modest.
