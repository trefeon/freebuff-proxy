# freebuff-proxy

[![CI](https://img.shields.io/github/actions/workflow/status/trefeon/freebuff-proxy/ci.yml)](https://github.com/trefeon/freebuff-proxy/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/trefeon/freebuff-proxy)](https://github.com/trefeon/freebuff-proxy/releases)
[![License](https://img.shields.io/github/license/trefeon/freebuff-proxy)](https://github.com/trefeon/freebuff-proxy/blob/main/LICENSE)

An OpenAI-compatible proxy bridge for the FreeBuff free tier. Point any OpenAI client at it and it talks to FreeBuff for you, with token pooling and session management built in.

FreeBuff (Codebuff's free coding agent) exposes its models only through the official CLI. The backend fingerprints CLI traffic and rejects direct API calls with `403 free_mode_cli_required`. freebuff-proxy replicates the CLI request envelope, manages the free-session and agent-run lifecycle upstream, and pools multiple tokens. Clients see a plain OpenAI-compatible API.

> **Current upstream status (read before installing).** Since roughly 2026-08-03, upstream
> rejects free-tier `POST /api/v1/chat/completions` with `403 free_mode_cli_required` even
> for valid, non-banned tokens. Sessions and agent runs still succeed, so `/healthz` and
> `/v1/models` work normally and your install will look healthy while chat fails. The check is
> server-side and bound to the account, and it is **not** bypassable from this project: see
> [the FAQ entry](#faq) for the full list of what was tested against a live token. Install
> anyway to be ready for an upstream change, but do not expect working chat today.

What this is not: an official FreeBuff or Codebuff product. It is a community bridge for an unofficial service. See the FAQ and Terms of use at the bottom.

> ## WARNING: your account can get suspended or banned
>
> This project works by making FreeBuff believe it is talking to the official CLI. The
> upstream service detects this and **does suspend and ban accounts**.
>
> - Suspended/banned accounts fail with `403 account_banned` / `{"status":"banned"}`, and
>   the web dashboard shows **"suspended"**. Your FreeBuff/Codebuff account, tokens, and
>   free-tier access are on the line.
> - The ban is **per account and effectively terminal**. The official source code flags
>   the account as banned ("terminal", returned from every endpoint). Unbanning is an
>   internal admin operation; there is no self-service path. Community proxies have seen
>   `resumes_at` timestamps in ban responses, which would mean temporary bans, but this is
>   **not confirmed in any official source** and may just be the account being gone.
> - Bans are scored by a public abuse-detection pipeline: heavy continuous usage (hundreds
>   of messages a day, many distinct active hours, long unattended sessions), automation
>   patterns, fresh GitHub accounts (under a few weeks old), throwaway email addresses,
>   and clusters of new accounts created close together all raise the score.
> - Codebuff's terms allow one account per person and explicitly prohibit wrappers,
>   proxies, and non-human sessions. Using this proxy already conflicts with them.
>
> **Use at your own risk, and assume a ban is permanent.**
>
> - Use one modest account; do not run 24/7, do not leave sessions running unattended,
>   stop when you see `429 rate_limited`.
> - If you are banned: the token is dead. Wait and re-probe once (cheap to try), then get
>   a **new account with an established GitHub login (months old, not fresh) and a clean
>   IP, without a VPN**. That is the only realistic recovery.
> - Appeals go to support@codebuff.com and realistically only succeed for false positives,
>   not for proxy use. The maintainers have had accounts suspended while building and
>   testing this project. This is not a toy warning.

## What it does

- Serves `/v1/chat/completions`, `/v1/models`, and `/healthz` on `127.0.0.1:3457` by default.
- Pools tokens: `AUTH_TOKENS` accepts comma-separated values, round-robins across them, and cools a token down for 30 minutes after a 401.
- Keeps free sessions alive: single-flight session create/poll/end, runs prewarmed at boot, rotated every `ROTATION_INTERVAL` (default 6h).
- Refreshes the model catalog every 6h from the Codebuff sources (15 models at boot, served by `/v1/models`).
- Sends outbound traffic through `HTTP_PROXY` or `SOCKS5_PROXY`, or impersonates a browser TLS fingerprint with `TLS_FINGERPRINT`.

## Requirements

- Zero or more FreeBuff auth tokens. With none, the proxy runs in **bridge mode** — each client sends their own token (see [Bridge mode](#bridge-mode)).
- Release binaries run standalone. Building from source needs Go 1.26+ (see `go.mod`).

## Install

Four ways to install. If you are new, pick Option 1.

| Option | Pick it when | Needs |
|---|---|---|
| 1. One-command installer | You just want it running (interactive menu picks the rest for you) | `curl`, a terminal |
| 2. Manual download | You want to see every step, or the installer is blocked | `curl`, `tar`/`unzip` |
| 3. Docker Compose | You run containers, or want it always-on with a healthcheck | Docker + Compose v2 |
| 4. Build from source | You want to audit or modify the code | Go 1.26+ |

All four end the same way: a proxy listening on `127.0.0.1:3457` (or `:3457` in a container)
plus a `.env`. Then run the [Quick start](#quick-start) smoke test and compare against the
results table there before wiring any client.

### Option 1: one-command installer (recommended)

Downloads the **latest** release binary for your platform, verifies its checksum, sets up
`.env`, asks for your token, and prints the next steps. No version to look up, no manual
downloads. Running it in a terminal shows an interactive menu: easy install, manual binary,
Docker Compose, or **bridge mode** (no proxy token — clients send their own). For scripted
runs add `--no-prompt` (safe defaults) or pick a method with `--method=binary|docker|bridge`.

**Windows (PowerShell):**

```powershell
irm https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.ps1 | iex
```

**Linux / macOS (bash):**

```bash
curl -sSL https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.sh | bash
```

Both scripts install into the current directory (`--dir <path>` to change it), create
`AUTH_TOKENS` in `.env` from your freebuff CLI login, or prompt you to paste a login URL
(`https://freebuff.com/login?auth_code=...`), and print the run and smoke-test commands.

### Option 2: manual download (no version lookup)

Download the archive for your platform from the [latest release](https://github.com/trefeon/freebuff-proxy/releases/latest). The commands below resolve the version automatically — no manual `<version>` replacement needed. Every release ships `checksums.txt`.

| Platform | Asset name |
|---|---|
| linux / amd64 | `freebuff-proxy_<version>_linux_amd64.tar.gz` |
| linux / arm64 | `freebuff-proxy_<version>_linux_arm64.tar.gz` |
| macOS / amd64 | `freebuff-proxy_<version>_darwin_amd64.tar.gz` |
| macOS / arm64 | `freebuff-proxy_<version>_darwin_arm64.tar.gz` |
| windows / amd64 | `freebuff-proxy_<version>_windows_amd64.zip` |
| windows / arm64 | `freebuff-proxy_<version>_windows_arm64.zip` |

**Linux / macOS** (one-liner, adjust the asset suffix for your platform):

```bash
VERSION="$(curl -fsSL https://api.github.com/repos/trefeon/freebuff-proxy/releases/latest | grep -oP '"tag_name":\s*"\K[^"]+' )"
curl -fsSL -o freebuff-proxy.tar.gz "https://github.com/trefeon/freebuff-proxy/releases/latest/download/freebuff-proxy_${VERSION}_linux_amd64.tar.gz"
curl -fsSL -o checksums.txt "https://github.com/trefeon/freebuff-proxy/releases/latest/download/checksums.txt"
tar xzf freebuff-proxy.tar.gz
sha256sum -c checksums.txt --ignore-missing 2>/dev/null || echo "checksum mismatch — verify manually"
./freebuff-proxy
```

**Windows (PowerShell):**

```powershell
$v = (Invoke-RestMethod https://api.github.com/repos/trefeon/freebuff-proxy/releases/latest).tag_name
Invoke-WebRequest -OutFile freebuff-proxy.zip "https://github.com/trefeon/freebuff-proxy/releases/latest/download/freebuff-proxy_${v}_windows_amd64.zip"
Expand-Archive freebuff-proxy.zip -DestinationPath . -Force
.\freebuff-proxy.exe
```

For the fully automatic path (download + checksum + `.env` + token + next steps), just use the one-command installer in Option 1.

### Option 3: Docker

Copy `.env.example` to `.env` and set `AUTH_TOKENS` first (or leave it empty for bridge
mode), then:

```bash
docker compose up -d --build
docker compose ps          # wait for "healthy" before smoke testing
```

The compose file publishes port 3457, sets `LISTEN_ADDR=:3457`, and runs a healthcheck
against `/healthz`. For a one-shot setup on Linux, `scripts/setup-proxy-docker.sh` clones the
repo, grabs the token, starts the container, and prints the 9router config with the right
Docker gateway IP.

**If you run the image without Compose, you must set `LISTEN_ADDR` yourself.** The default
(`127.0.0.1:3457`) binds loopback *inside* the container, so a published port leads nowhere
and `curl` fails with "connection refused" while the container looks fine:

```bash
docker build -t freebuff-proxy .
docker run -d -p 3457:3457 --env-file .env -e LISTEN_ADDR=:3457 freebuff-proxy
```

Leaving out `-e LISTEN_ADDR=:3457` is the single most common "my setup does not work"
report. Compose sets it for you; plain `docker run` does not.

### Option 4: build from source

```bash
go build -o freebuff-proxy ./cmd/freebuff-proxy
```

Windows builds: `go build -o freebuff-proxy.exe ./cmd/freebuff-proxy`.

## Getting a token

Two ways, plus scripts that automate the CLI path:

- **Web (no install):** log in at [freebuff.llm.pm](https://freebuff.llm.pm). Under
  **Freebuff Auth** click **Generate login URL**, then **copy** the URL it shows
  (`https://freebuff.com/login?auth_code=...`). The token is the `auth_code` value from
  that link, e.g. from `...?auth_code=4v2G-8dmPXNjgZvbCvhIcA` the token is
  `4v2G-8dmPXNjgZvbCvhIcA`. Pasting the whole URL also works in the scripts below.
- **Official CLI:** `npm i -g freebuff`, run `freebuff` once to log in, then read `authToken` from `~/.config/manicode/credentials.json` (Windows: `C:\Users\<you>\.config\manicode\credentials.json`).
- **Scripts:** `scripts/get-freebuff-token.sh` (bash) or `scripts/get-freebuff-token.ps1` (PowerShell) install the CLI, log in, and write `AUTH_TOKENS` into `.env` for you.

Use the token without any `Bearer ` prefix; the proxy adds it upstream itself. For higher throughput, log in with several accounts and comma-separate the tokens: `AUTH_TOKENS=tok1,tok2`.

## Quick start

1. Copy the example config:

   ```bash
   cp .env.example .env
   ```

   (Windows PowerShell: `Copy-Item .env.example .env`)

2. Edit `.env` and set `AUTH_TOKENS`. Optional — empty starts the proxy in bridge mode (see [Bridge mode](#bridge-mode)).

3. Run the proxy:

   ```bash
   ./freebuff-proxy
   ```

4. Smoke test:

   ```bash
   curl http://localhost:3457/healthz
   curl http://localhost:3457/v1/models
   curl -N http://localhost:3457/v1/chat/completions \
     -H "Content-Type: application/json" \
     -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"Say hello in one short sentence."}],"stream":true}'
   ```

### What the smoke test should return

Read this table before concluding your setup is broken. **`/healthz` and `/v1/models`
returning 200 means your setup is correct** — those two prove the binary, config, port, and
model registry all work. The chat call additionally depends on your account and on upstream,
so a failure there is usually *not* a setup problem.

| Call | Result | Meaning |
|---|---|---|
| `/healthz` | `200` + JSON (`uptime_seconds`, `models`, `tokens`) | Proxy is up. Setup OK. |
| `/healthz` | connection refused | Not running, or in a container without `LISTEN_ADDR=:3457` (see [Option 3](#option-3-docker)). |
| `/v1/models` | `200` + ~12 model ids | Registry loaded. Setup OK. |
| `/v1/models` | `401 invalid_api_key` | You set `API_KEYS`; send `Authorization: Bearer <your-api-key>`. |
| chat | SSE stream (`data: {...}`) | Everything works end to end. |
| chat | `502` wrapping `403 free_mode_cli_required` | **Upstream's CLI-only gate, not your setup.** See the FAQ entry below. |
| chat | `502` wrapping `401`/`404 Invalid API key or user not found` | The token in `.env` is invalid, expired, or the account is gone. Get a fresh token. |
| chat | `403 account_banned` | Account suspended upstream. Token is dead, see the WARNING at the top. |
| chat | `429 rate_limited` | Daily session quota used up (6/day on limited tier). Wait for the Pacific-midnight reset or add a token. |
| chat | `503 waiting_room_queued` | Normal. Retry after `Retry-After`; 9router and opencode do this automatically. |
| chat | `401 missing_bearer_token` | Bridge mode with no client token. Send `Authorization: Bearer <your-freebuff-token>`. |
| chat | `400 invalid_json` | Shell quoting mangled the `-d` payload. On Windows use `--data @file.json` instead of inline quotes. |

If `/healthz` and `/v1/models` are 200 and only chat fails, your installation is fine: the
problem is the token or upstream, and no config change in this project will fix it.

## Configuration

Every key is read from the environment and overrides the JSON config file passed with `-config` (see `config.example.json`; keys mirror the env names). `-v` enables verbose logging.

| Key | Default | Description |
|---|---|---|
| `AUTH_TOKENS` | empty | FreeBuff token(s), comma-separated. Round-robin + failover across tokens. Empty = bridge mode: clients supply their own token per request. |
| `LISTEN_ADDR` | `127.0.0.1:3457` | Listen address. Loopback only by default; use `:3457` in containers or behind a firewall. |
| `UPSTREAM_BASE_URL` | `https://codebuff.com` | Upstream base URL (host normalized to `www.codebuff.com`). |
| `ROTATION_INTERVAL` | `6h` | How long an agent run lives upstream before rotation (FINISH + restart). |
| `REQUEST_TIMEOUT` | `15m` | Timeout for one chat-completions request, stream included. |
| `SESSION_CALL_TIMEOUT` | `30s` | Timeout for individual session/run API calls. |
| `REGISTRY_REFRESH` | `6h` | How often the model registry re-fetches the Codebuff sources. |
| `API_KEYS` | empty | Optional client auth. Comma-separated keys clients must present. Empty means no client auth. |
| `HTTP_PROXY` | empty | Outbound HTTP/HTTPS proxy (CONNECT tunneling). |
| `SOCKS5_PROXY` | empty | Outbound SOCKS5 proxy, e.g. `socks5://127.0.0.1:1080`. |
| `COST_MODE` | `free` | Mode sent upstream with chat requests. Must be `free`: the upstream 402 balance check runs only when `cost_mode != "free"`, so omitting it makes fresh free-tier accounts fail with `402 "Out of credits. Please add credits at codebuff.com/usage"`. |
| `DEBUG_DUMP` | `false` | Dump raw upstream traffic into `./dump` (sensitive headers redacted). |
| `LOG_FILE` | empty | Append logs to a file in addition to stderr. |
| `LOG_LEVEL` | info | `debug`, `info`, `warn`, or `error`. `-v` implies debug; `LOG_LEVEL` wins. |
| `TLS_FINGERPRINT` | empty | Outbound JA3 fingerprint: `chrome120`, `safari17`, `firefox120`, or `random`. |
| `MAX_MESSAGES_PER_DAY` | `0` | Per-token rolling 24h message cap. At the cap the proxy answers `429 rate_limited` with `Retry-After` instead of hitting upstream, keeping the account far under FreeBuff's abuse thresholds (~500 msgs/24h). `0` = unlimited. |
| `IDLE_ROTATION_TIMEOUT` | `0` | Pause background work after this long without traffic (e.g. `30m`): runs are FINISHed and maintenance stops until the next request, so the account is not kept artificially active 24/7. `0` = always maintain. |

## Bridge mode

Leave `AUTH_TOKENS=` **empty** and the proxy boots in bridge mode: it holds no token of its own and is a pure relay. Every client sends their own FreeBuff token with each request:

```bash
curl -N http://localhost:3457/v1/chat/completions \
  -H "Authorization: Bearer <your-freebuff-token>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

- The token from `Authorization: Bearer <token>` (or `x-api-key: <token>`) is used **verbatim upstream** — it is never written to `.env` or logged (only counts/hints are logged).
- `/healthz` and `/v1/models` need no header. `API_KEYS` is **ignored** in bridge mode (the Authorization header is the upstream credential, not a proxy key).
- Sessions and runs are created **lazily per token** on first use and **reused** across that client's later requests (least quota burn). The cache is bounded at **32 client tokens** with LRU eviction; entries idle for ~2h are finished and dropped.
- All existing error mapping still applies **per account**: `403 account_banned`, `429 rate_limited` + `Retry-After`, `503 waiting_room_queued`, etc. — each client's token is cooled down / banned independently.
- `MAX_MESSAGES_PER_DAY` applies **per client token** (each cached entry has its own rolling 24h counter).

Readiness check: `/healthz` includes `bridge_tokens` — the number of cached client-token entries (0 until the first chat).

## 9router integration

Add freebuff-proxy as an OpenAI-compatible custom provider in 9router. The step-by-step guide covers the dashboard form, model catalog, verification, and troubleshooting: [docs/guides/9router-integration.md](docs/guides/9router-integration.md).

Quick version: Dashboard, Providers, Add OpenAI Compatible. Base URL `http://localhost:3457/v1`, API Type Chat Completions, any non-empty API key, and the model ids come from `/v1/models`. Model combos become `freebuff/<model-id>`.

Alternatively, **bridge mode**: leave `AUTH_TOKENS` empty on the proxy and use **your FreeBuff token as the 9router API key** — the proxy then relays with your token (one token = one session; quota/ban status is yours).

## Docs

- [9router integration guide](docs/guides/9router-integration.md): full wiring, model catalog, troubleshooting.
- The other project docs (PRD, research notes, delivery tasks, security notes) are local-only dev docs, gitignored on purpose. They do not ship with the repo.

## FAQ

**The smoke test chat returns `403 free_mode_cli_required` (often wrapped in a `502`).**

The full message is *"Free mode is only available through the freebuff CLI. Install it with
`npm i -g freebuff`, then run `freebuff`. Calling the API directly is not supported and may
get your account banned."*

**This is not a broken setup and not a bad token.** Upstream added a CLI-only gate on the
free tier (first reported around 2026-08-03, see
[Quorinex/Freebuff2API#18](https://github.com/Quorinex/Freebuff2API/issues/18)). If
`/healthz` and `/v1/models` return 200, your install is correct.

What was tested against a live, non-banned token, all still returning `403`:

- the CLI/AI-SDK user agent (`ai-sdk/openai-compatible/<ver>/codebuff`)
- a stable `client_id` reused across session, run, and chat
- `x-freebuff-model` on session creation, `x-freebuff-instance-id` on chat
- `COST_MODE=free` (verified in the startup log) and a valid 13-char base36 `client_id`
- `TLS_FINGERPRINT=chrome120` (browser JA3 impersonation)
- a hand-built request sent straight to upstream with no proxy involved

Session creation and agent-run START both succeed (`200`); only
`POST /api/v1/chat/completions` is rejected, and only once the run actually exists — so the
check is server-side and bound to the account/run, not to anything in the request. No setting
in this project bypasses it. Your options are to use the official CLI directly, or wait and
re-test after an upstream change. Open an issue if you see it start working again.

**I get `402` / "Out of credits. Please add credits at codebuff.com/usage".**

The request went down the paid path. Upstream runs its balance check only when `cost_mode != "free"`, so a fresh free account (balance 0) always gets 402 unless `COST_MODE=free` is sent. Check your `.env`: `COST_MODE` must be `free` (the default and the value in `.env.example`). If it is empty, the proxy bills the request as paid. Old configs copied before v0.2.0 that set `COST_MODE=` empty need the value restored.

**I get `429` with `rate_limited` in the body.**

The token's daily session quota is exhausted (6 sessions per day on the limited tier, resets at Pacific midnight). The proxy returns `429` with the upstream `resetAt` so clients back off. Add another `AUTH_TOKENS` or wait for the reset.

**I get `403` with `account_banned` / `{"status":"banned"}`.**

Your FreeBuff account was banned or suspended upstream. See the **WARNING** at the top of
this file: the ban is per account and effectively permanent, the token is dead, and no
setting will revive it. The proxy stops using the token during the ban window (upstream
`resumes-at`, or 24h if none) and then re-probes once, which is cheap to try; if it still
fails, get a new account with an established GitHub login and a clean IP (no VPN). Appeals
go to support@codebuff.com but realistically only succeed for false positives.

**I get `503` with `waiting_room_queued`.**

Normal. The free session is queued in the waiting room. The `Retry-After` header tells the client when to retry; 9router and opencode retry automatically.

**Windows Defender or Kaspersky flags the binary or test executables.**

This is a heuristic false positive, not malware. The trigger is the optional TLS-fingerprint
module (`internal/stealth`): it links `refraction-networking/utls`, a library whose purpose
is impersonating a browser's TLS fingerprint (JA3). Malware uses the same technique to evade
network detection, so AV vendors heuristically flag any executable containing uTLS (that is
a static pattern match, not a behavior detection). The proxy is a plain HTTP server: no
persistence, no injection, no extra network traffic beyond the documented upstream relay,
and all token values are redacted from logs and dumps.

Verify it yourself in under a minute: build from source and compare with the release
checksums:

```bash
go build -o freebuff-proxy.exe ./cmd/freebuff-proxy
sha256sum freebuff-proxy.exe         # must match the value in the release's checksums.txt
```

If the hashes match, the flagged binary is exactly the public source. You can also submit
the binary for re-analysis at [opentip.kaspersky.com](https://opentip.kaspersky.com) with the
build-from-source repro. Practical workarounds:

- Add the binary and the Go build cache (`go-build*` paths) to AV exclusions so `go test`
  and normal use are not interrupted.
- `TLS_FINGERPRINT` is **empty by default**, so the uTLS path is only compiled in, not
  active, unless you set it. You lose nothing by leaving it unset.

Open an issue if you see a detection name (we have never seen a real signature match — only
heuristics).

**Is this against FreeBuff's terms?**

FreeBuff is intended to be used through the official CLI only. This proxy uses undocumented endpoints and replicates CLI fingerprints, which conflicts with the letter of the service terms. Account bans are possible. Use it for personal and educational experimentation, keep usage modest, at your own risk.

**How do I keep my account from getting banned?**

Use less, use it like a human, and let the proxy do the same. Set
`MAX_MESSAGES_PER_DAY` (well under the ~500 msgs/24h threshold, e.g. `150`) and
`IDLE_ROTATION_TIMEOUT` (e.g. `30m`) so the proxy stops background work when you are not
using it; do not run it 24/7, stop when you see `429 rate_limited`, and never share a
token between the proxy, the official CLI, and the web dashboard at the same time. See
the WARNING at the top of this file and the Terms of use.

**I don't want to put my token in `.env` — can I send it per request?**

Yes — run the proxy in **bridge mode**: leave `AUTH_TOKENS=` empty and send your FreeBuff
token as `Authorization: Bearer <token>` (or `x-api-key: <token>`) on every chat request.
The proxy relays with your token, never stores it in `.env`, and reuses one lazy session
per token. See [Bridge mode](#bridge-mode).

**Still stuck?** Open an issue with the proxy version, your client, and `LOG_LEVEL=debug` output (redact tokens).

## Development

```bash
go build ./...
go vet ./...
go test ./...          # runs against the mock upstream, no token needed
golangci-lint run ./...  # lint config in .golangci.yml
```

CI runs `go test -race ./...` and `go mod verify` on Linux. Windows note: some AVs quarantine freshly linked test binaries out of the go-build cache (`fork/exec ... Access is denied`); that is the false positive above, use `go test -c -o out\convert.test.exe ./internal/convert` and run it directly as a workaround.

See [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request.

## Terms of use

This project is not affiliated with or endorsed by Codebuff. FreeBuff free tier is an unofficial, moving target: quota, models, and endpoints change without notice, and the proxy may break at any time. Use at your own risk.

## License

[MIT](LICENSE).
