# Getting Started with freebuff-proxy (5-Minute Guide)

This guide takes you from zero to a working OpenAI-compatible proxy connected to FreeBuff.

---

## What is freebuff-proxy?

`freebuff-proxy` is a local bridge server. It sits between your favorite coding tools (like Continue, Cursor, aider, or opencode) and FreeBuff's free AI models. Your tools talk standard OpenAI API to `localhost:3457`, and the proxy manages sessions and tokens behind the scenes.

```
+-------------------+      OpenAI API      +-------------------+      FreeBuff      +-------------------+
| Your AI Client    | -------------------> | freebuff-proxy    | -----------------> | codebuff.com      |
| (Continue/Cursor) | <------------------- | (localhost:3457)  | <----------------- | (Free Models)     |
+-------------------+      SSE Streams     +-------------------+     CLI Envelope   +-------------------+
```

---

## Important Safety Warning

Using this proxy conflicts with Codebuff's terms of service. Upstream abuse detection scans for automation patterns and suspends accounts.
- **Use `SAFE_MODE=true`** (enabled by default in `.env.example`).
- Do **not** run 24/7 on heavy unattended automated tasks.
- Keep one modest account; do not create spam clusters of accounts.

---

## Step 1: Install & Start the Proxy

### Option A: One-Command Installer (Recommended)

**Linux / macOS Terminal:**
```bash
curl -sSL https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.sh | bash
```

**Windows PowerShell:**
```powershell
irm https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.ps1 | iex
```

Follow the prompts to pick your token or enable bridge mode.

---

### Option B: Docker Compose

```bash
cp .env.example .env
# Edit .env and set AUTH_TOKENS=your_token
docker compose up -d --build
```

---

## Step 2: Verify It Works

Run the diagnostic tool or curl:

```bash
# Diagnostic doctor check:
./freebuff-proxy -doctor

# Quick health check:
curl http://localhost:3457/healthz

# List available models:
curl http://localhost:3457/v1/models
```

`/healthz` returning status `200` means your proxy setup is **100% correct**.

---

## Step 3: Connect Your Favorite AI Client

Point your AI tool to:
- **Base URL:** `http://localhost:3457/v1`
- **API Key:** `not-needed` (or your token in bridge mode)
- **Model:** `deepseek/deepseek-v4-flash` (or `z-ai/glm-5.2`)

See the [Client Integration Guide](client-integration.md) for copy-paste config for Continue, Cursor, aider, opencode, and more.

---

## Troubleshooting

Run `./freebuff-proxy -doctor` to diagnose problems automatically.

| Error / Symptom | Cause & Fix |
|---|---|
| `502` + `403 free_mode_cli_required` | Upstream CLI-only enforcement on free tier. Setup is fine; see FAQ. |
| `502` + `401 Invalid API key` | Token in `.env` is expired or invalid. Get a fresh token at https://freebuff.llm.pm. |
| Connection refused | Proxy is not running, or in Docker without `LISTEN_ADDR=:3457`. |
| `403 account_banned` | Account suspended upstream. Token is dead; use a new established account. |
