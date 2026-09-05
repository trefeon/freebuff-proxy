# User Lifecycle

End-to-end operator walkthrough for `freebuff-proxy`: install → first run →
add tokens → use → monitor → edit config → rotate/remove → review quota →
update. Commands are the public CLI/HTTP surface; "expected output" lines are
abridged.

## 1. Install

Choose one path for your OS. The installers write `.env` into your platform
config directory and add the binary to your PATH.

**Linux / macOS (bash installer)**

```bash
curl -sSL https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install.sh | bash
```

```text
[1] Easy install         [2] Manual binary        [3] Docker Compose
[4] Bridge mode ...
Installing to ~/.local/bin/freebuff-proxy...
Wrote .env to ~/.config/freebuff-proxy/.env
Quick Integration: Point your AI tools to http://localhost:3457/v1
```

If you already have a `freebuff-proxy` source checkout, `scripts/install.sh`
detects it as dev mode and keeps the legacy `.env`-in-cwd layout.

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install.ps1 | iex
```

```text
Installing freebuff-proxy (latest release)...
Binary+template: %LOCALAPPDATA%\Programs\freebuff-proxy\freebuff-proxy.exe
Live .env:       %APPDATA%\freebuff-proxy\.env
```

Non-interactive flags: `--dir <path>` / `--prefix <dir>` (bash), `-Dir <path>`
(PowerShell) relocate the install; `--skip-token` / `--no-env` / `--force` /
`--env-file <path>` / `--no-prompt` (bash) and their `-*` PowerShell
counterparts mirror the installer steps.

**Docker Compose (dev clone or container path)**

```bash
cp .env.example .env                       # seed the template next to the compose file
# set AUTH_TOKENS in .env
docker compose up -d --build
```

```text
[+] Running 3/3
 ✔ Container freebuff-proxy  Started
```

`scripts/setup-proxy-docker.sh` builds, starts, and prints the exact 9router
config (including the Docker gateway IP). Docker image runs unprivileged,
healthchecked on `/healthz`, `LISTEN_ADDR=:3457`.

**systemd (Linux)**

```bash
sudo useradd --system --home-dir /var/lib/freebuff-proxy --shell /usr/sbin/nologin freebuff-proxy
sudo mkdir -p /etc/freebuff-proxy
sudo install -m 640 -o root -g freebuff-proxy .env /etc/freebuff-proxy/env
sudo install -m 0755 freebuff-proxy /usr/local/bin/freebuff-proxy
sudo cp scripts/freebuff-proxy.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now freebuff-proxy
```

```text
● freebuff-proxy.service - FreeBuff Proxy Bridge
   Active: active (running)
```

**macOS launchd**

```bash
cp scripts/com.freebuff-proxy.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.freebuff-proxy.plist
```

`com.freebuff-proxy.plist` runs `/usr/local/bin/freebuff-proxy` at load,
keeps it alive, and logs to `/tmp/freebuff-proxy.{log,err}`.

## 2. First run

Dashboard first (running proxy): the Overview diagnostics card (`POST /admin/diag`) is the sanity check, and the **Setup** page shows copy blocks for every client. CLI second: `-doctor` is the headless sanity check (config, port, DNS/TLS, registry, per-token zero-cost probes), and `-setup` is the interactive client configurator (Continue / opencode / aider) — it does **not** write the proxy `.env`.

```bash
./freebuff-proxy -doctor
```

```text
freebuff-proxy doctor diagnostic tool
=====================================
[ok] Configuration loaded & validated successfully
[ok] AUTH_TOKENS: 1 token(s) configured
[ok] Token #1 format valid (42 chars)
[ok] Listen address :3457 is available
[ok] DNS lookup for www.codebuff.com resolved (104.18.1.2, ...)
[ok] TLS connection to www.codebuff.com:443 succeeded
[ok] Egress region: US (203.0.113.7)
[ok] Model registry offline fallback loaded (6 models, 6 agents)
[ok] Registry live refresh succeeded (6 models)
[ok] Token #1 validity probe succeeded
Summary: 10 passed, 0 warnings, 0 failed
```

A missing token or unreachable upstream is reported honestly: warnings do not
change the exit code (still `0`); a config error or failed reachability check
makes it exit `1`.

Dashboard first: the **Setup** page (`http://127.0.0.1:3457/admin/setup`) shows per-model copy blocks — copy, no file writes. CLI second (writes client files):

```bash
./freebuff-proxy -setup
```

```text
freebuff-proxy interactive client setup
======================================
[-] Continue (~/.continue/) not found on this system
[-] opencode (~/.config/opencode/) not found on this system
[-] aider not found on this system
Setup complete! Configured 0 client tool(s).
Base URL: http://localhost:3457/v1
```

Start the proxy:

```bash
./freebuff-proxy
```

```text
  freebuff-proxy 1.4.0 is running!
  API endpoint:  http://127.0.0.1:3457/v1
  Health check:  http://127.0.0.1:3457/healthz
```

## 3. Add tokens

Dashboard first: open `http://127.0.0.1:3457/admin`, log in with `ADMIN_TOKEN`, then use the Tokens page (in-browser OAuth login wizard, then **Add Token**). CLI second: generate heads-up and append to `.env` (`AUTH_TOKENS` is a comma-separated list), then reload.

**Dashboard** — open `http://127.0.0.1:3457/admin`, log in with `ADMIN_TOKEN`,
then **Tokens → Add Token to Pool**. The wizard runs the same headless OAuth
flow and persists the token to `.env`. The `/admin/tokens/*` routes are
session-cookie dashboard endpoints (they return an HTMX fragment, not a JSON
API), so a human uses the dashboard; a script uses `gen-token.* --append`.

**CLI generate (headless OAuth)**

```bash
./scripts/gen-token.sh --append        # Linux/macOS
.\scripts\gen-token.cmd                # Windows (menu; Enter = append)
```

```text
Browser login...
Token cb_... added to ~/.config/freebuff-proxy/.env
```

`AUTH_TOKENS=` empty means bridge mode (clients bring their own token).

## 4. Use clients

Point an OpenAI client at `/v1`, and an Anthropic client at `/v1/messages`
(the proxy also exposes `/v1/responses` and `/v1/messages/count_tokens`).

**OpenAI-shaped**

```bash
curl -N http://127.0.0.1:3457/v1/chat/completions \
  -H "Authorization: Bearer <api-key-or-omitted>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

```text
data: {"id":"...","object":"chat.completion.chunk","model":"deepseek/deepseek-v4-flash",...}
data: {"choices":[{"delta":{"content":"The","role":"assistant"}}...]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: [DONE]
```

**Anthropic-shaped**

```bash
curl -N http://127.0.0.1:3457/v1/messages \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}'
```

```text
event: message_start
data: {"type":"message_start","message":{"model":"deepseek/deepseek-v4-flash",...}}
event: content_block_delta ...
event: message_stop
data: {"type":"message_stop"}
```

## 5. Monitor

Dashboard first: the admin dashboard at `http://127.0.0.1:3457/admin` gives the live overview (token risk cards), Traces (per-request routing outcome), Logs, Metrics, and Prometheus `GET /metrics`. CLI second: health is a single JSON read — `GET /healthz` returns `status`, `mode`, model count, per-token snapshot (incl. per-model `quota`), and `bridge_tokens`.

```bash
curl -s http://127.0.0.1:3457/healthz
```

```json
{"status":"ok","mode":"hybrid","uptime_seconds":42.1,"models":6,
 "tokens":[{"token":1,"status":"healthy","quota":{"deepseek/deepseek-v4-flash":{"limit":5,"recent":2}}}],
 "bridge_tokens":0,"bridge_entries":[]}
```

The quota gauges live at `GET /metrics` as `freebuff_proxy_quota_recent`/`_limit`.
If the port is bound non-loopback and `ADMIN_TOKEN` is unset, sensitive dashboard routes require a loopback client.

## 6. Edit config

Edit `.env` by hand and hot-reload, or use the dashboard **Configuration
Studio** (`POST /admin/config`, which validates and persists `.env`, then
hot-reloads — it rolls back on rejection).

```bash
curl -X POST http://127.0.0.1:3457/admin/reload -H "Authorization: Bearer $ADMIN_TOKEN"
```

```json
{"status":"ok","message":"configuration reloaded","auth_tokens":2,"safe_mode":true}
```

Some keys are **restart-only** (e.g. `LISTEN_ADDR`, `ACTING_USER_ID`) — the
Config Studio marks them; changing them via reload takes effect on the next
start. An invalid value is rejected and the previous `.env` content is left
intact (no partial write).

## 7. Rotate / remove tokens

Keep the pool draining one key at a time; when a key hits its quota it is
locked locally until reset. Remove or swap keys in the dashboard **Tokens**
page (**Remove** pops the last token; the pool is construction-fixed, so an
`.env` edit adds/removes on the next reload or restart). The `/admin/tokens/*`
routes are session-cookie dashboard endpoints (HTMX fragment, not a JSON API).

Dashboard first: the Tokens page OAuth login wizard mints a fresh token in the browser and persists it to `.env`. CLI second: `-refresh-token N` re-authenticates a stale token in `.env` via the headless GitHub login flow:

```bash
./freebuff-proxy -refresh-token 0
```

## 8. Quota Tracker review

Each model's live session quota surfaces on `/healthz` (per-token `quota`
map) and `/metrics` (`freebuff_proxy_quota_recent` / `freebuff_proxy_quota_limit`).
Dashboard first: the Quota Tracker page plus **Tokens → Test All** (`POST /admin/tokens/test-all`) give the same live read in the browser. CLI second: `-test-token` gives a one-shot, zero-cost read that prints the quota and exits `0` (healthy) or `1` (bad).

```bash
./freebuff-proxy -test-token
```

```text
freebuff-proxy: token OK — quota: 2/5 pacific_day, resets 2026-08-31T07:00:00Z
```

A `429 rate_limited` resets at Pacific midnight (07:00 UTC); the proxy locks
the token locally and answers `429` in `<1ms`, so routers fail over to the
next token. `403 account_banned` / `country_blocked` is terminal — rotate the
account out.

## 9. Update

Dashboard first: the Overview update badge links the release when a newer one exists — the dashboard never swaps the binary. CLI second: **self-updater** (SHA-256 verified against the release `checksums.txt`; a release without the checksums manifest is refused).

```bash
./freebuff-proxy -update
```

```text
freebuff-proxy self-updater
===========================
Current version: 1.4.0 (linux/amd64)
Latest release: 1.4.1
Downloading freebuff-proxy_1.4.1_linux_amd64.tar.gz...
Verifying SHA-256 against checksums.txt... OK
Installing... done
```

**Dev clone (git pull + rebuild)**

```bash
git pull --ff-only
docker compose up -d --build
```

```text
 ✔ Container freebuff-proxy  Started
```

For a systemd/launchd install, restart the service after the swap
(`systemctl restart freebuff-proxy` / `launchctl kickstart -k
gui/$(id -u)/com.freebuff-proxy`).
