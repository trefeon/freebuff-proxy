# Dashboard Guide

The embedded admin web UI gives you live relay state, per-token quotas, an in-browser `.env` Configuration Studio, model catalog inspection, live logs, and quick client setup snippets in a single binary.

Built with **Svelte 5 + Tailwind CSS v4** and self-hosted **Geist + JetBrains Mono** typography (`@fontsource/geist`, `@fontsource/jetbrains-mono` in `frontend/package.json`, wired in `frontend/src/app.css`), it follows the **terminal-cyber-ops** theme (ADR-0018): a dense, dark-mode-only operational console with hairline borders, tabular-numeric metrics, and live status indicators. It is embedded directly into the Go binary at build time (`//go:embed`) with zero runtime Node.js or external CDN dependencies.

## Management policy (WebUI-first, issue #359)

The dashboard (`/admin`) is the default daily path. The CLI stays fully working as the headless and bootstrap path.

- Frozen flag set: no flag is removed or renamed, and every exit code keeps scripting the same way.
- Dashboard-canonical: token add/remove/swap/test/lock, config edit plus reload, restart, logs, metrics, quota, maturity, setup copy blocks, and the update notice live in `/admin`.
- Hide, never delete: pre-serve checks (`-doctor`, `-test-token`, `-validate-tokens`), OS service registration (`-install-service`, `-uninstall-service`, `-service-status`), slot re-auth (`-refresh-token`), client-file setup (`-setup`), and the binary swap (`-update`) stay on the CLI because a browser tab cannot do them. `-help` groups them as advanced with the dashboard twin noted per flag.

---

## Access

Open `http://127.0.0.1:3457/admin` (or your configured `LISTEN_ADDR`). You land on the login page unless login is disabled.

| Setting | Behavior |
|---|---|
| `DASHBOARD_REQUIRE_LOGIN=true` (default) and `ADMIN_TOKEN` set | **Password required**: `ADMIN_TOKEN` is both the bearer token for `/admin/reload` and the login password. Enter it on the login page; a signed `HttpOnly` + `SameSite=Strict` cookie (`fb_admin`) unlocks the dashboard for 24h (`Secure` flag adapts dynamically: `Secure: true` when accessed via HTTPS or a TLS reverse proxy, and `Secure: false` over plain HTTP so self-hosted cloud VPS instances work out-of-the-box without browser drops; `ADMIN_FORCE_SECURE_COOKIES=true` forces `Secure` unconditionally. The password is capped at 256 characters). Failed logins are rate-limited per IP (5 failed attempts to 1-minute lockout) plus a process-wide budget (20 failures per minute, then a global lockout that doubles with each breach, capped at 5 minutes). |
| `DASHBOARD_REQUIRE_LOGIN=false` | **Open mode**: loopback clients reach the dashboard without login. From a non-loopback address, sensitive routes (`/admin/config`, `/admin/logs`) still return `403 Forbidden` to prevent remote secret disclosure. |

The session cookie is stateless (HMAC-signed expiry with a per-process random key): restarting the proxy automatically signs out active sessions.

---

## Pages and navigation (8 sidebar pages plus deep links)

The page set has one source of truth: `NAV_ITEMS` in `frontend/src/lib/nav.js`. The sidebar lists 8 pages. Four more pages are reachable by URL only (`inSidebar: false`): setup, metrics, traces, and playground. Pool accounts are labeled **Account #1, #2, …** everywhere (1-based, in pool order) so the first credential reads as account 1, never index 0.

### 1. Overview

- **System Status Line**: live badge showing active mode (`Pooled`, `Bridge` or `Hybrid`), proxy version, process uptime, and request count.
- **Key Performance Indicators (KPIs)**: 6 tabular-mono counters in pooled mode (Pool total, Busy, Cooldown, Banned, Requests today, Models; `Overview.svelte` renders a 3-card summary in bridge mode instead).
- **Client Integration**: universal base URL (`/v1`) with copy button plus OpenAI and Anthropic endpoint shapes.
- **Bridge Relay Card** (hybrid mode): active bridge-client count.
- **Freebucks Allowance**: per-account daily allowance windows when reported upstream.
- **Diagnostics card**: `POST /admin/diag` runs the same checks as the `-doctor` CLI on the live server.
- **Update badge**: links the release page when a newer release exists. The dashboard never swaps the binary; use `-update` or a rebuild, then restart.

### 2. Tokens

- **Account Table**: every pooled credential as `Account #N` with status badge, session instance, live cooldown countdown, and per-account actions:
  - **Move Up / Move Down**: reorder pool priority (`POST /admin/tokens/swap`); Move Up is disabled on the first account, Move Down on the last. Reorders apply instantly even mid-stream: in-flight requests stay pinned to their account, and recovery paths (session invalidate, cooldowns) follow the lease, never the old index.
  - **Clear**: clears a stale cooldown lock.
  - **Lock / Unlock**: excludes an account from rotation until unlocked.
  - **Remove**: deletes the account from the pool and `.env` (confirm dialog; dismissing sends nothing).
  - Expandable rows: active-session countdown with **Drop Session**, model allowlist pinning, and (with `DEVTOOLS_ENABLED=true`) a session-spawn toolbar with **Make Session**, **Probe**, and **Finish Runs**.
- **Account Risk Cards**: at-risk accounts (active cooldowns, elevated ban risk) with live cooldown countdowns live here, so all account health sits on one page.
- **Token Rotation Policy**: `Drain (safest)` / `Round Robin (1:1)` / `Least Used` / `Random` radios plus the **Auto Failover on Rate Limit (429)** switch, both persisted to `.env`.
- **Add Token Form**: appends new FreeBuff auth tokens directly to `.env`.
- **OAuth Login Wizard**: one-click device-code browser login flow for minting fresh tokens without the CLI.
- **Test All** (`POST /admin/tokens/test-all`): zero-cost per-token validity probe, same as `-test-token`; it also refreshes the cached quota snapshots behind the Quota Tracker page.

### 3. Maturity

Per-account maturity tracking with stacked model and mode selects, per-token touch-model override plus global fallback, and persisted card expansion. Touch runs ride the live request poll path. The touch model defaults to the unmetered flash model so touches never spend premium Freebucks. Automation state (config, slot, warning and relock counters) persists across restarts in the dashboard DB; a token that hits its streak target auto-releases, and one whose streak lapses for 2 straight days re-locks for warming. Endpoints: `POST /admin/tokens/{id}/maturity` (enable/disable), `POST /admin/tokens/{id}/maturity/touch` (manual touch, bypasses slot/throttle), `POST /admin/tokens/{id}/maturity/warn-reset` (clears only the non-advance warning and re-arms the loop — conf…

### 4. Quota Tracker

- **Per-Account Cards** (`Account #1…`): Freebucks allowance windows with per-model hourly prices (cheapest first), period reset countdowns (Pacific midnight; day granularity past 24h, e.g. `27d 10h`), and entitlement tiers. Restart-restored rows are labeled last-seen until the next request refreshes them. A token with a live session keeps serving on it (admitted session bills once at session start; re-polls ride free and DELETE refunds); only fresh sessions spend Freebucks.

### 5. Models

Live catalog of served models with upstream agent bindings and Freebucks hourly prices; 1-click model ID copy actions. The list is the registry the proxy routes on, refreshed on `REGISTRY_REFRESH`.

### 6. Logs

- **Console** (default): live inference-traffic stream (`/v1` only) with structured per-request rows and status chips, auto-refreshing every 1s with pause, manual refresh, clear, and copy controls.
- **Table**: newest 200 ring entries with log-level select (`INFO`, `DEBUG`, `WARN`, `ERROR`), message search (`?msg=`), Hide-admin toggle, clear-filters, rows-per-page select, and Next/Prev pagination.

### 7. Dev Tools

Gated behind `DEVTOOLS_ENABLED=true`: batch chat smoke surface and the session spawner (Make Session, Probe, Finish Runs). The sidebar hides this entry unless the gate is on; the gate is also enforced server-side (playground POST handlers return 404 `devtools_disabled` when off).

### 8. Settings

Intent-driven cards whose values apply live on save (no container restart):

- **Security**: admin password change with inline validation (minimum 6 characters, mismatch detection, show/hide toggles).
- **General**: Anti-Ban Safe Mode, server log level, bridge-mode toggle.
- **Pool**: per-IP rate limit, 429 auto-failover.
- **Upstream**: model aliases, allowed-models filter, reasoning-in-content, model token locks.

`LISTEN_ADDR` and the log-file keys are restart-only: the Settings page marks them, and changing them via reload takes effect on the next start. **Advanced: raw `.env` editor** (collapsible): direct editing with server-side validation; rejected writes roll back. Unsaved-changes banner with **Save Changes** / **Discard**.

### Deep-link-only pages (no sidebar entry)

- **Setup** (`/admin/setup`): mode-aware client setup with universal Base URL, client API key field with **Generate** / **Reset**, per-model 1-click copy buttons, and copy-paste snippets for major AI harnesses: **OpenCode** (`opencode.json`), **Claude Code CLI** (`ANTHROPIC_BASE_URL` and `ANTHROPIC_API_KEY`), **Cursor / VS Code Continue / Cline** (OpenAI endpoint), **Pi Coding Agent** (`models.json`), **9router** (provider setup), **cURL** (instant terminal verification). Copy only, no file writes; the CLI `-setup` is the twin that writes client files.
- **Metrics** (`/admin/metrics`): tabular stat cards with SVG sparklines and a direct link to the raw `/metrics` Prometheus feed.
- **Traces** (`/admin/traces`): recent chat traces with per-phase latency breakdowns (token, model, status, duration, error class).
- **Playground** (`/admin/playground`): renders the same DevTools page component (self-gated when `DEVTOOLS_ENABLED` is off) under a separate id so the deep link still mounts after the registry consolidation. Its chat box forwards through `POST /admin/playground/chat` into the normal chat pipeline.

### Login

`GET /admin` serves the SPA shell; unauthenticated visits land on the login form (or straight into the app when login is disabled). Login POSTs to `POST /admin/login/start`. Password changes go through the Security settings card, which writes `ADMIN_TOKEN` plus `DASHBOARD_REQUIRE_LOGIN=true` back to `.env`.

---

## Docker Usage

The configuration editor writes `./.env` relative to the proxy's working directory. In Docker, bind-mount your `.env` so that edits persist across container restarts:

```yaml
services:
  freebuff-proxy:
    image: ghcr.io/trefeon/freebuff-proxy:latest
    ports:
      - "3457:3457"
    environment:
      - LISTEN_ADDR=:3457
    volumes:
      - ./.env:/app/.env
```

`LISTEN_ADDR=:3457` inside the container binds all interfaces; the compose file maps it to the host. Bind the published port to loopback (`"127.0.0.1:3457:3457"`) when the dashboard should not face the network, since `/healthz` and `/metrics` stay unauthenticated by design for scrapers.

---

## Hardening Recommendations

1. **Set `ADMIN_TOKEN`**: always configure a strong password in production or multi-user environments.
2. **Bind to Loopback**: keep `LISTEN_ADDR=127.0.0.1:3457` unless placed behind a reverse proxy (e.g., Caddy, Nginx) with HTTPS termination.
3. **Secret Redaction**: the dashboard strictly masks tokens and credentials in API responses and logs.

---

**Related Documentation**:

- [README](../README.md)
- [Getting Started](getting-started.md)
- [Client Integration](client-integration.md)
- [Design Specification](../DESIGN.md)
