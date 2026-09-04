# Dashboard Guide

The embedded admin web UI gives you live relay state, per-token quotas, an in-browser `.env` Configuration Studio, model catalog inspection, live logs, and quick client setup snippets in a single binary.

Built with **Svelte 5 + Tailwind CSS v4** and self-hosted **IBM Plex Sans & IBM Plex Mono** typography, it follows an **"instrument panel"** design system: a dense, dark-mode-only operational console with hairline borders, tabular-numeric metrics, and live LED status indicators. It is embedded directly into the Go binary at build time (`//go:embed`) with zero runtime Node.js or external CDN dependencies.

---

## Access

Open `http://127.0.0.1:3457/admin` (or your configured `LISTEN_ADDR`). You land on the login page unless `ADMIN_TOKEN` is unset.

| Setting | Behavior |
|---|---|
| `ADMIN_TOKEN` set | **Password required**: `ADMIN_TOKEN` is both the bearer token for `/admin/reload` and the login password. Enter it on the login page; a signed `HttpOnly` + `SameSite=Strict` cookie (`fb_admin`) unlocks the dashboard for 24h (`Secure` flag adapts dynamically: `Secure: true` when accessed via HTTPS or a TLS reverse proxy, and `Secure: false` over plain HTTP so self-hosted cloud VPS instances work out-of-the-box without browser drops; `ADMIN_FORCE_SECURE_COOKIES=true` forces `Secure` unconditionally. The password is capped at 256 characters). Failed logins are rate-limited per IP (5 failed attempts $\to$ 1-minute lockout) plus a process-wide budget (20 failures per minute, then a global lockout that doubles with each breach, capped at 5 minutes). |
| `ADMIN_TOKEN` unset | **Open mode (local-only safe default)**: When running locally without `ADMIN_TOKEN`, the dashboard is open. When accessed from a non-loopback IP without `ADMIN_TOKEN`, sensitive routes (`/admin/config`, `/admin/logs`) return `403 Forbidden` to prevent remote secret disclosure. |

The session cookie is stateless (HMAC-signed expiry with a per-process random key): restarting the proxy automatically signs out active sessions.

---

## Pages & Navigation (9 Sections)

The dashboard provides 9 focused operational sections. Pool accounts are
labeled **Account #1, #2, …** everywhere (1-based, in pool order) so the
first credential reads as account 1, never index 0.

### 1. Overview
- **System Status Line**: Live badge displaying active mode (`Pooled`, `Bridge` or `Hybrid`), proxy version, process uptime, and request count.
- **Key Performance Indicators (KPIs)**: 6 tabular-mono counters:
  - Total Pool Tokens
  - Active / Busy Tokens
  - Tokens in Cooldown
  - Banned Tokens
  - Requests Served Today
  - Served Models Count
- **Client Integration**: Universal base URL (`/v1`) with copy button plus OpenAI and Anthropic endpoint shapes.
- **Bridge Relay Card** (hybrid mode): active bridge-client count.
- **Freebucks Allowance**: per-account daily allowance windows when reported upstream.

### 2. Tokens
- **Account Table**: Every pooled credential as `Account #N` with status badge, session instance, live cooldown countdown (ticks every second; `45s`, `2m 10s`, `expiring…`, rolling over to `27d 10h` past 24h), and per-account actions:
  - **Move Up / Move Down**: Reorder pool priority (`POST /admin/tokens/swap`); Move Up is disabled on the first account, Move Down on the last. Reorders apply instantly even mid-stream: in-flight requests stay pinned to their account, and recovery paths (session invalidate, cooldowns) follow the lease, never the old index.
  - **Clear**: Clears a stale cooldown lock.
  - **Lock / Unlock**: Excludes an account from rotation until unlocked.
  - **Remove**: Deletes the account from the pool and `.env` (confirm dialog; dismissing sends nothing).
  - Expandable rows: active-session countdown with **Drop Session**, model allowlist pinning, and (with `DEVTOOLS_ENABLED=true`) a session-spawn toolbar with **Make Session**, **Probe**, and **Finish Runs**.
- **Account Risk Cards**: At-risk accounts (active cooldowns, elevated ban risk) with live cooldown countdowns — moved here from Overview so all account health lives on one page.
- **Token Rotation Policy**: `Drain (safest)` / `Round Robin (1:1)` / `Least Used` / `Random` radios plus the **Auto Failover on Rate Limit (429)** switch, both persisted to `.env`.
- **Add Token Form**: Appends new FreeBuff auth tokens directly to `.env`.
- **OAuth Login Wizard**: One-click device-code browser login flow for minting fresh tokens without the CLI.

### 3. Quota Tracker
- **Per-Account Cards** (`Account #1…`): premium-pool bars and per-model session-quota tables with live upstream limits, recent usage, period reset countdowns (Pacific midnight; day granularity past 24h, e.g. `27d 10h`), and entitlement tiers. Restart-restored rows are labeled last-seen until the next request refreshes them. A daily-capped account still serves its own live session (reuse costs no quota); only fresh admissions are refused until reset.

### 4. Models
- Live catalog of served models with upstream agent bindings and session quotas; 1-click model ID copy actions.

### 5. Logs
- **Console** (default): live inference-traffic stream (`/v1` only) with structured per-request rows and status chips, auto-refreshing every 1s with pause, manual refresh, clear, and copy controls.
- **Table**: newest 200 ring entries with log-level select (`INFO`, `DEBUG`, `WARN`, `ERROR`), message search (`?msg=`), Hide-admin toggle, clear-filters, rows-per-page select, and Next/Prev pagination.
### 6. Metrics
- Tabular stat cards with SVG sparklines and a direct link to the raw `/metrics` Prometheus feed.

### 7. Traces
- Recent chat traces with per-phase latency breakdowns (token, model, status, duration, error class).

### 8. Settings
- Intent-driven cards whose values apply live on save (no container restart):
  - **Security**: admin password change with inline validation (minimum 6 characters, mismatch detection, show/hide toggles).
  - **General**: Anti-Ban Safe Mode, server log level, bridge-mode toggle.
  - **Pool**: per-IP rate limit, 429 auto-failover.
  - **Upstream**: model aliases, allowed-models filter, reasoning-in-content, model token locks.
- **Advanced: raw `.env` editor** (collapsible): direct editing with server-side validation; rejected writes roll back. Unsaved-changes banner with **Save Changes** / **Discard**.

### 9. Setup
- Mode-aware client setup: universal Base URL, client API key field with **Generate** / **Reset**, per-model 1-click copy buttons, and copy-paste snippets for major AI harnesses:
  - **OpenCode** (`opencode.json`)
  - **Claude Code CLI** (`ANTHROPIC_BASE_URL` & `ANTHROPIC_API_KEY`)
  - **Cursor / VS Code Continue / Cline** (OpenAI endpoint)
  - **Pi Coding Agent** (`models.json`)
  - **9router** (Provider setup)
  - **cURL** (Instant terminal verification)

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

---

## Hardening Recommendations

1. **Set `ADMIN_TOKEN`**: Always configure a strong password in production or multi-user environments.
2. **Bind to Loopback**: Keep `LISTEN_ADDR=127.0.0.1:3457` unless placed behind a reverse proxy (e.g., Caddy, Nginx) with HTTPS termination.
3. **Secret Redaction**: The dashboard strictly masks tokens and credentials in API responses and logs.

---

**Related Documentation**:
- [README](../README.md)
- [Getting Started](getting-started.md)
- [Client Integration](client-integration.md)
- [Design Specification](../DESIGN.md)
