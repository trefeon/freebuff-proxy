# Dashboard Guide

The embedded admin web UI gives you live relay state, per-token quotas, an in-browser `.env` Configuration Studio, model catalog inspection, live logs, and quick client setup snippets in a single binary.

Built with **Svelte 5 + Tailwind CSS v4** and self-hosted **IBM Plex Sans & IBM Plex Mono** typography, it follows an **"instrument panel"** design system: a dense, dark-mode-only operational console with hairline borders, tabular-numeric metrics, and live LED status indicators. It is embedded directly into the Go binary at build time (`//go:embed`) with zero runtime Node.js or external CDN dependencies.

---

## Access

Open `http://127.0.0.1:3457/admin` (or your configured `LISTEN_ADDR`). You land on the login page unless `ADMIN_TOKEN` is unset.

| Setting | Behavior |
|---|---|
| `ADMIN_TOKEN` set | **Password required**: `ADMIN_TOKEN` is both the bearer token for `/admin/reload` and the login password. Enter it on the login page; a signed `HttpOnly` + `SameSite=Strict` cookie (`fb_admin`) unlocks the dashboard for 24h (`Secure` is always set — modern browsers still accept it on `http://localhost`/`127.0.0.1`, so local dev works, while a remote plain-HTTP admin login is refused by the browser; use HTTPS or a TLS-terminating reverse proxy. The password is capped at 256 characters). Failed logins are rate-limited per IP (5 failed attempts $\to$ 1-minute lockout) plus a process-wide budget (20 failures per minute, then a global lockout that doubles with each breach, capped at 5 minutes). |
| `ADMIN_TOKEN` unset | **Open mode (local-only safe default)**: When running locally without `ADMIN_TOKEN`, the dashboard is open. When accessed from a non-loopback IP without `ADMIN_TOKEN`, sensitive routes (`/admin/config`, `/admin/logs`) return `403 Forbidden` to prevent remote secret disclosure. |

The session cookie is stateless (HMAC-signed expiry with a per-process random key): restarting the proxy automatically signs out active sessions.

---

## Pages & Navigation (6 Curated Sections)

The dashboard provides 6 focused operational sections:

### 1. Overview
- **System Status Line**: Live badge displaying active mode (`Pooled` or `Bridge`), proxy version, process uptime, and request count.
- **Key Performance Indicators (KPIs)**: 6 tabular-mono counters:
  - Total Pool Tokens
  - Active / Busy Tokens
  - Tokens in Cooldown
  - Banned Tokens
  - Requests Served Today
  - Served Models Count
- **Token Risk Cards**: Surfaces tokens with active cooldowns, elevated ban risk scores, or rate limits.

### 2. Tokens & Quotas
- **Token Table**: Lists all managed tokens with masked short IDs (`cb_...`), status badges, instance IDs, active runs, cooldown countdown timers, and per-token actions:
  - **Test Token**: Zero-cost upstream validity probe.
  - **Unlock**: Clears cooldown locks and re-admits the token.
  - **Remove**: Deletes token from the pool.
- **Add Token Form**: Input form to append new FreeBuff auth tokens (`cb_...`) directly to `.env`.
- **OAuth Login Wizard**: One-click device-code browser login flow for minting fresh tokens without the CLI.
- **Per-Model Quota Breakdown**: Expandable rows showing real-time upstream quota limits, recent usage counts, period reset countdowns (Pacific midnight), and entitlement tiers.

### 3. Models Registry
- Live catalog of all models available on FreeBuff.
- Upstream agent routing targets (`freebuff/deepseek/deepseek-v4-flash`, `mimo/mimo-v2.5`, `openai/gpt-5.6-luna`, `minimax/minimax-m3`, etc.).
- Access tier annotations (`Full Tier` vs `Limited Tier`).
- 1-click model ID copy actions for quick client configuration.

### 4. Configuration Studio
- **Visual `.env` Editor**: In-browser monospace editor for live configuration changes.
- **Validation & Safe Save**:
  - **Validate**: Checks syntax, port availability, and token formats before applying.
  - **Save & Reload**: Writes `.env` atomically via temp-file rename (mode `0600`) and triggers hot reload (`POST /admin/reload`) without dropping active connections.
  - **Rollback**: Automatically reverts to the previous valid configuration if validation fails.
  - **Restart-only keys**: A save reports which keys only take effect after a restart (`TLS_FINGERPRINT`, `TRANSIENT_RETRIES`, `UPSTREAM_BASE_URL`, and the other construction-time knobs) instead of implying a fully live update.
- **Effective Configuration Table**: Read-only breakdown of active in-memory settings with automatic secret redaction (`AUTH_TOKENS`, `ADMIN_TOKEN`, `API_KEYS` masked).

### 5. In-Memory Logs
- Live circular log ring buffer (last 500 entries) capturing structured slog output.
- Log level filtering (`ALL`, `INFO`, `DEBUG`, `WARN`, `ERROR`).
- Real-time substring search and automatic live polling.
- Horizontal scroll with mono formatting and level indicator dots.

### 6. Setup & Client Integration
- Mode-aware quick-start cards with one-click copy snippets for major AI harnesses:
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
