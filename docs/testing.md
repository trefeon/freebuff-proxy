# Testing freebuff-proxy by Hand (Linux and Windows)

A runbook for humans. It walks the same checks the CI suite automates, one
command at a time, and tells you what a healthy proxy looks like at each
step. Use it after installing, after upgrading, or when something feels
off and you want to isolate whether the proxy or the upstream is the
problem.

Two ground rules before you start.

- `127.0.0.1:3457` is the default listen address, loopback only. If your
  `.env` sets `LISTEN_ADDR=:3457` (containers do), substitute your machine's
  address where the commands say `127.0.0.1`.
- Every command below is safe. Nothing here consumes a paid session or
  modifies `.env`. The only exceptions are the two places that say so:
  the real chat test (which uses your daily quota) and the service
  install/uninstall section.

## 0. The binary, the version, the config

Start by confirming you have a real build and it can see its config.

```
./freebuff-proxy -version
./freebuff-proxy -v -doctor
```

What you want to see:

- `-version` prints a version string. `dev` means a build without release
  metadata, which is normal for `go run` or a hand-built binary. Release
  archives are built with the version baked in, so a downloaded release
  always prints its tag (e.g. `v1.0.0`); if a release binary says `dev`,
  something is off with the build.
- `-doctor` prints a line per check, `[ok]` or `[FAIL]`, then exits. It
  probes every configured token with a zero-cost GET request (no session
  slot consumed) and reports each as `[ok]` or `[FAIL]`. A clean
  environment shows `[ok]` everywhere; see section 4 for what the token
  lines mean.

On Linux, if you run `docker compose up -d --build` instead of the bare
binary, the container listens on `:3457` inside the network. Check it with
`docker compose ps` (should show `Up` and `healthy`) and `docker logs
freebuff-proxy --tail 20` (should show the startup banner with
`version=`, `listen_addr=`, and `auth_tokens=` counts, and nothing at
`level=ERROR`).

## 1. Is the HTTP server actually serving?

The proxy has one endpoint that requires no auth at all.

```
curl -s http://127.0.0.1:3457/healthz
```

Linux without curl? `wget -qO- http://127.0.0.1:3457/healthz`. PowerShell:

```powershell
(Invoke-RestMethod http://127.0.0.1:3457/healthz) | ConvertTo-Json -Depth 4
```

A healthy response is JSON with a `tokens` array. Each entry carries the
per-token state. The fields you care about:

| Field | Healthy value | What it means |
|---|---|---|
| `SessionStatus` | `active` (pooled) | Session up, ready to run |
| `ActiveRuns` | 0 or a small number | Runs currently executing |
| `Messages24h` | below `DailyLimit` | Rolling 24h usage |
| `UsagePct` | under 100 | Fraction of the daily budget spent |
| `RiskLevel` | `low` or `medium` | Account trust standing |
| `tier`, `country` | any | Account tier and region |

In bridge mode (`AUTH_TOKENS=` empty) the response instead shows a
`bridge_tokens` count and `mode: "bridge"`. Both are healthy; the shape
just differs.

Anything else here, like a connection refused, means the process is not
listening. Check three things in order: the process is running, the
`LISTEN_ADDR` in `.env` matches the address you curled, and nothing else
already owns the port (`netstat -ano | findstr :3457` on Windows,
`ss -ltnp | grep 3457` on Linux).

## 2. The models endpoint

```
curl -s http://127.0.0.1:3457/v1/models
```

The `requireAuth` gate only bites when you configured `API_KEYS` (and are
not in bridge mode). With the default loopback bind and no `API_KEYS`,
every `/v1/*` endpoint answers unauthenticated — the loopback binding is
the security boundary, so anything exposed this way is only reachable
from the machine itself. If you set `API_KEYS`, the same curl needs a
header:

```
curl -s -H "x-api-key: <one of your API_KEYS>" http://127.0.0.1:3457/v1/models
```

You expect a `data` array of objects each with an `id` and a model name,
plus `available` / `status` / `current_access_tier` per model so clients
can see quota and lock signals without probing. That list is the registry
the proxy routes on, refreshed on `REGISTRY_REFRESH` (default 6h). If it
is empty, the registry fetch failed; check the logs for a registry error
before blaming anything else.

## 3. The real test: one chat completion, then a stream

This is the check that proves the whole chain: your token, the session
manager, the upstream, and the SSE path. It uses your daily quota, so run
it once, not in a loop.

```
curl -s http://127.0.0.1:3457/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"say hi"}],"stream":false}'
```

In bridge mode this request must carry your FreeBuff token, because the
proxy has none of its own: add `-H "Authorization: Bearer cb_..."`. In
pooled mode (tokens in `AUTH_TOKENS`) the header is optional unless you
set `API_KEYS`. The proxy answers `missing_bearer_token` (401) exactly
when a bridge-mode request arrives without one, so a 401 here means you
forgot the header, not that the proxy is broken.

Healthy: HTTP 200 and a JSON body with `choices[0].message.content` that
contains an actual reply, plus a `usage` object with `total_tokens` over
zero. If the model id in the request is not in `/v1/models`, the proxy
answers `model_not_found`; pick one from the list.

Now the stream, because streaming is the default for every coding agent
and it fails in different ways than non-streaming does.

```
curl -N -s http://127.0.0.1:3457/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"count to 5"}],"stream":true}'
```

Same header rule as above: add `Authorization: Bearer cb_...` in bridge
mode.

Healthy: lines of `data: {"id":"chatcmpl-...","choices":[{"delta":{...}}]}` arriving over time, ending with `data: [DONE]`. If you get one big blob
at the end, streaming is broken. If the upstream stream dies mid-generation,
the proxy emits an error chunk and still terminates with `data: [DONE]`;
an error *without* the terminator means the proxy itself failed mid-relay.

### Responses API and Anthropic messages

Same idea, different shapes, all on the same server.

```
curl -s http://127.0.0.1:3457/v1/responses \
  -H "Authorization: Bearer cb_..." -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","input":"say hi"}'
```

```
curl -s http://127.0.0.1:3457/v1/messages \
  -H "Authorization: Bearer cb_..." -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"deepseek/deepseek-v4-flash","max_tokens":64,"messages":[{"role":"user","content":"say hi"}]}'
```

Healthy: HTTP 200, a content block array. A 404 here means you are
running an older binary; these routes landed in the v0.9.8 cycle.

## 4. Token and account checks

### The zero-cost probe

`-test-token` verifies the first configured token without consuming a
session slot. It is the fastest health signal for "is my account even
alive".

```
./freebuff-proxy -test-token
```

Exit 0 with `token OK` means the token authenticates. Any failure (bad
token, quota 429, banned 403) exits 1 with a message, so a quota-spent
account reports as failure — the exit code is about *can this token work
right now*, not about blame. A `429` is quota exhaustion, expected late in
a day, not a ban; it resets at Pacific midnight. A `403` with
`status: banned` may carry a resume time (the ban is temporary when the
upstream provides one); `403 country_blocked` means the egress region is
refused. Neither is something you can fix by restarting the proxy.

### The full diagnostics

`-doctor` checks local state (config validity, environment, port
bindings, DNS + TLS reachability of the upstream) and then probes every
configured token with a zero-cost GET request. Those probes do not claim
a session and do not consume a daily slot, which is why they always run:
the old session-handshake probes cost a slot per run, so they were gated
behind a flag; the GET probe removed that cost entirely. The only case
that skips probing is bridge mode, where the proxy holds no tokens to
check.

```
./freebuff-proxy -doctor
```

Watch for `[FAIL]` lines and read them; they are self-explanatory. A
`[!!]` line is a warning, not an error. A `Token #N validity probe
failed` means the token itself is expired or revoked; re-run the upstream
CLI to refresh it.

## 5. The dashboard

Open the browser UI at `http://127.0.0.1:3457/admin`. Set `ADMIN_TOKEN`
in `.env` if you want the login page; without it, the read-only pages
(overview, tokens, models, traces) stay open and the secret-bearing pages
(config, logs) require a loopback client.

What a healthy dashboard shows:

- **Overview**: uptime, 6 KPI counters, client-integration base URL. (Account
  risk cards live on the Tokens page, not here.)
- **Tokens**: `Account #1, #2, …` rows (1-based pool order) with live
  cooldown countdowns, per-account Lock/Remove/Move actions, rotation
  radios, and the at-risk account cards. `low` risk is healthy; `high`
  is worth a look at what changed (new egress, new device).
- **Quota Tracker**: per-account premium-pool bars and session-quota tables.
- **Logs**: console (live `/v1` traffic, 1s auto-refresh) plus the table
  view of the newest 200 ring entries, no `level=ERROR` spam.
- **Metrics**: live SVG sparklines of requests, runs, and usage.
- **Setup**: mode card, client API key field, and per-model copy buttons.

Polling is per-page (Overview 15s, Tokens live store, Logs 1s). If one
page's numbers freeze while the proxy still answers curl, reload that
page; a proxy restart clears everything.

## 6. Service mode (run forever)

If you want the proxy to survive logout and reboot, install it as a
service. This is the one section that modifies your system, and it needs
the binary at a stable path first (a temp download location will break
the service on next boot).

Windows:

```
.\freebuff-proxy.exe -install-service
.\freebuff-proxy.exe -service-status
```

The status command exits 0 when the service is *registered* (running or
not), 1 when not registered at all — it is a registration check, not a
liveness check. Registration uses
Task Scheduler at logon with limited privileges; you see the task under
`schtasks /query /tn "freebuff-proxy"` if you want to inspect it.
`-uninstall-service` reverses it.

Linux:

```
./freebuff-proxy -install-service
./freebuff-proxy -service-status
```

This registers a systemd `--user` unit in `~/.config/systemd/user`.
Status comes from `systemctl --user is-active freebuff-proxy`, logs from
`journalctl --user -u freebuff-proxy -n 50 -e`. The unit sets its working
directory to the binary's location, so `.env` must live next to the
binary, not in your shell's home.

## 7. The restart and the upgrade

Two behaviors worth knowing before they surprise you.

A restart ends in-flight streams. That is by design: the container or
process re-creates its sessions, and a streamed response mid-generation
is cut. Clients retry. With `SESSION_PERSIST=true`, active sessions are
resumed on restart instead of recreated, which avoids burning a fresh
session slot; that is the knob to test if you run long sessions.

The config page in the dashboard and `POST /admin/reload` reload `.env`
without restarting. What a reload applies: registry aliases and the
runtime knobs (session-create caps, re-admit lead, probe-cache TTL). A
reload also reconciles the pool with `AUTH_TOKENS`: a slot whose token
changed is rebuilt live — the old account's runs are FINISHed and its
admitted session ended, and a fresh entry is built for the new token (some
stale state, like a terminal quarantine for a dead account, never carries
over to a different account). A token appended to `AUTH_TOKENS` is picked
up the same way. Removing pooled tokens is still not something a reload
does: use the dashboard's Tokens page (Remove last) or a restart, and the
Tokens page remains the live path for add/remove and mode switch — those
take effect immediately and persist to `AUTH_TOKENS` in `.env` (surviving
a restart), no restart needed.

Self-update is `./freebuff-proxy -update`, which checks GitHub and
replaces the binary with the latest release. It skips the swap if the
checksum does not match. On Windows the swap defers until the current
process exits, so do not be surprised if the command exits before the
file changes.

## 8. Reading failures: the error taxonomy

When a test fails, the error body tells you which layer is at fault. The
proxy classifies upstream responses deliberately.

| Status | Code | Meaning | Your move |
|---|---|---|---|
| 401 | `missing_bearer_token` | Bridge mode: you sent no token | Add `Authorization: Bearer cb_...` |
| 402 | `out of credits` | Upstream billing state (paid routing) | A wrong `COST_MODE` in `.env` is a **startup config error**, not a runtime 402: the proxy refuses to start with anything but `free` or unset. A live 402 comes from the upstream account itself |
| 403 | `account_banned` | Account banned upstream (temporary when a resume time is provided) | Stop using that account; check egress for VPN/proxy signals; wait for the resume time if set |
| 403 | `country_blocked` | Egress region refused by upstream | Route through an allowed region (the proxy has no proxy support; egress is direct) |
| 429 | quota / rate limit | Daily quota spent, resets Pacific midnight | Wait, or add a token |
| 429 | `ip_capped` | Too many distinct users on one egress IP | Rotate the account or the IP |
| 5xx | upstream error | FreeBuff side, transient | Retry later; check `TRANSIENT_RETRIES` behavior in logs |

The distinction that matters most: **429 is a quota, not a ban**. A
banned account is 403 and may resume later (the upstream supplies a
resume time when the ban is temporary). Panicking over a 429 and rotating
egress aggressively is how accounts get flagged; the proxy's job is to
sit still on quota days.

## 9. The "is it running perfectly" checklist

Run through this in order when you are unsure. Each line is one command
or one glance.

1. `./freebuff-proxy -doctor` → no `[FAIL]` (token probe lines all `[ok]`).
2. `curl http://127.0.0.1:3457/healthz` → JSON, `SessionStatus: active`,
   `UsagePct` under 100.
3. `curl /v1/models` → non-empty model list.
4. One non-streaming chat → 200, real content, `usage.total_tokens` > 0.
5. One streaming chat → `data:` lines, ends `[DONE]`, no mid-stream error.
6. Dashboard Tokens page → `Account #1, #2, …` rows listed, at-risk cards
   render when present, no ERROR spam in logs.
7. Logs show startup banner with `auth_tokens=N` matching your `.env`.
8. `-test-token` exits 0 with `token OK`. (A quota 429 exits 1 — that is
   a quota day, not a proxy fault; the checklist then reads as "healthy
   but quota-spent".)

If all eight pass, the proxy is doing its job. Remaining weirdness is
upstream or account-side: a quota day, a regional model pick that got
silently coerced to `deepseek/deepseek-v4-flash`, or a client misconfig.

## 10. Platform-specific gotchas

Windows:

- PowerShell 5.1 reads UTF-8 `.env` files as ANSI by default. If the
  config page shows mojibake in comments or a token with non-ASCII
  neighbors breaks, re-save `.env` as UTF-8 with BOM or edit with a
  tool that writes UTF-8 explicitly.
- Loopback-only binding means other machines on your LAN cannot reach
  the proxy. That is intentional. To serve your LAN, set
  `LISTEN_ADDR=:3457` and, if you set `ADMIN_TOKEN`, the dashboard cookie
  always carries the `Secure` flag (unconditional — loopback dev still
  works because browsers accept Secure cookies on localhost/127.0.0.1).
- Windows Defender SmartScreen may flag the unsigned binary. Allow it
  once; the release carries SLSA provenance if you want to verify it
  instead of trusting the popup.

Linux:

- The binary needs execute permission after download
  (`chmod +x freebuff-proxy`). If you get `Permission denied` on a fresh
  download, that is the cause.
- If you run the systemd user service, `.env` goes next to the binary,
  not in `$HOME`. The unit sets `WorkingDirectory` to the binary path.
- The container build needs network access to the Go module proxy. On
  constrained hosts, build with `docker build --network=host`.

Both platforms:

- Never commit or paste `.env` contents, including into this runbook.
  Token check commands above take the token in the header; use a shell
  variable so it does not land in your shell history if that matters to
  you: `TOK=$(grep AUTH_TOKENS .env | cut -d= -f2)` then
  `-H "Authorization: Bearer $TOK"`.

## 11. When to file a bug versus when it is you

File an issue when: a `[FAIL]` in doctor contradicts a working setup,
`/v1/models` is empty while logs show no registry error, a stream ends
without `[DONE]` and without an `upstream_error` event, or the dashboard
freezes while curl works.

It is you, not the proxy, when: the token is wrong or quota-spent (429),
the model id is not in `/v1/models`, `LISTEN_ADDR` mismatches the address
you curl, or the client sends a shape the endpoint does not accept.

---

This guide covers the whole surface in the current release. If a command
here 404s or errors, you are likely on an older binary; upgrade and
re-run, because the routes and flags listed are the ones current releases ship.
