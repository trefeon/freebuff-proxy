# Operations

Deploy, run, observe, and release freebuff-proxy. End-user setup: README +
docs/getting-started.md. Release policy: ADR-0010.

## Deployment paths

| Path | Command | Notes |
|---|---|---|
| Installer (Linux/macOS) | `curl -sSL <raw>/scripts/install-freebuff-proxy.sh \| bash` | prompts Easy/Manual/Docker/Bridge; writes `.env` to platform dir |
| Installer (Windows) | `irm <raw>/scripts/install-freebuff-proxy.ps1 \| iex` | same; `-Dir`/`-EnvFile` overrides |
| Release binary | unzip release asset, `./start-proxy.sh` (Win: `start-proxy.cmd`) | resolves `.env` from platform config dir |
| Docker | `docker compose up -d --build` | unprivileged user, `/healthz` healthcheck, `LISTEN_ADDR=:3457` in container |
| Service | `-install-service` / `-uninstall-service` / `-service-status` | Task Scheduler (Win, per-user), systemd `--user` (Linux), launchd (macOS) |

Platform config dirs: Linux `~/.config/freebuff-proxy/.env`, macOS
`~/Library/Application Support/freebuff-proxy/.env`, Windows
`%APPDATA%\freebuff-proxy\.env`. A `./.env` in the working directory still
wins when present. Never commit `.env`.

## Day-to-day operations

```bash
./freebuff-proxy -doctor        # config, port, DNS/TLS, registry, zero-cost token probes
./freebuff-proxy -test-token    # first-token probe, exit 0/1 (installers/scripts)
./freebuff-proxy -validate-tokens  # every token, exit 0/1/2
curl http://127.0.0.1:3457/healthz
curl http://127.0.0.1:3457/metrics
curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" http://127.0.0.1:3457/admin/reload
```

- Rotate `ADMIN_TOKEN` before exposing the port (factory default is public
  knowledge; see SECURITY-MODEL gap 1). Pooled mode without `API_KEYS` is an
  open `/v1` surface; set keys or bind loopback.
- Logs: `LOG_LEVEL`/`LOG_FORMAT`/`LOG_FILE`; per-request access lines;
  `/healthz`+`/metrics`+OPTIONS poll lines are rate-limited.
- Quota is normal: `429` resets at Pacific midnight; the proxy locks locally
  and answers in <1ms. Only `403 banned`/`country_blocked` is terminal.

## Development commands

```bash
task build            # frontend build, then Go binary (keeps the embed fresh)
task dev              # run the proxy locally
task frontend:dev     # Vite dev server (127.0.0.1:5173, proxies /admin/* to 127.0.0.1:3457)
task test             # hermetic Go suite
task test:race        # race suite (Linux/CI; not on Windows)
task lint             # go vet + golangci-lint
task verify           # canonical gate: format, vet, lint, build, tests, frontend checks
task verify:full      # verify + race + e2e (slow; CI-equivalent)
```

`make` mirrors the Taskfile for muscle memory only (see Makefile header);
Taskfile.yml is canonical.

## Release checklist

Pre-tag (all must be green):

- [ ] `task verify` green on a clean tree (`git status` clean).
- [ ] Frontend dist fresh: `task frontend:build`, then
      `git diff --exit-code -- backend/internal/dashboard/dist`.
- [ ] `env -u AUTH_TOKENS -u ADMIN_TOKEN go test -race ./backend/...` green
      (Linux/CI; `-timeout 10m` in CI).
- [ ] `golangci-lint run ./backend/...` green.
- [ ] Registry pins current: `bash scripts/check-upstream.sh` clean (or the
      drift PR merged first).
- [ ] CHANGELOG/release notes drafted; no secrets in notes or artifacts.
- [ ] `go mod verify` clean; no unpinned action changes.

Tag + publish: commit → push main → tag `vX.Y.Z` → push tag (release.yml
gates + GoReleaser + SLSA attest) → `gh release view` verifies assets
(3 OS × 2 arch, one format per OS, checksums.txt).

Post-release smoke (no live credentials needed):

```bash
./freebuff-proxy -doctor          # diagnostics
./freebuff-proxy &                # boot
curl localhost:3457/healthz      # liveness + snapshot
curl localhost:3457/v1/models    # catalog
curl localhost:3457/admin -o /dev/null -w '%{http_code}\n'   # dashboard serves
kill %1                           # graceful drain
```