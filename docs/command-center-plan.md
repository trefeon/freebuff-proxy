# Command Center: Install, Update, Rollback — Full Plan (no code yet)

Status: planning only. Execution order is by value unlocked; each phase
ships separately. Target: post-v1.8.8.

## 0. Non-negotiable boundaries

- The dashboard NEVER gets the Docker socket (root-equivalent inside a
  network-facing proxy — refused outright). No Watchtower-style blind
  auto-pull either.
- Honest naming: the dashboard restarts the proxy *process*, never the
  container object. Under `restart: unless-stopped` the two are
  observably identical (new PID, same container) — say exactly that.
- Check-ON by default, execute always explicit (one click). No silent
  updates. A global kill-switch gates every executor.
- Every version move is pinned explicitly (`vX.Y.Z`), never floating
  `latest`. Rollback = return to the previous pin.

## 1. Artifact strategy (unlocks everything else)

- Keep GoReleaser binaries (linux/darwin/windows × amd64/arm64) as-is.
- ADD GHCR images on every tag (buildx amd64+arm64, tags `vX.Y.Z` +
  `latest`, SLSA attestations like the binaries). Kills the two
  biggest deployment pains: flaky `go mod download` TLS on constrained
  hosts and ~100s local builds on small VPS boxes. Update becomes
  pull, rollback becomes re-tag.
- Compose goes image-first:
  `image: ghcr.io/trefeon/freebuff-proxy:${VERSION:-latest}` by
  default, local `build:` kept as fallback. The existing `VERSION=`
  pin convention stays; auto-update moves the pin, nothing floats.

## 2. Official installers (binaries + docker)

- `install.sh` / `install.ps1` (curl-pipeable, idempotent):
  detect Docker → write compose pin + `.env` from template with
  GENERATED secrets (`ADMIN_TOKEN` random, never factory default) →
  `up -d` (or install binary + register service) → print dashboard
  URL. Re-running upgrades safely: `.env` is never overwritten,
  state file and tokens survive.
- Binary mode registers a supervisor with restart-always semantics
  (systemd `Restart=always`, Task Scheduler equivalent, launchd
  `KeepAlive`) — this is what makes dashboard-initiated restarts and
  self-updates safe. Without a supervisor the dashboard disables
  restart/update execution with an explicit warning (no suicide
  buttons).
- Non-goals: no docker.sock mount, no privileged installer steps
  beyond service registration, no telemetry.

## 3. Dashboard Update Center

Extends the existing version badge (#50b: running vs latest GitHub
release, 6h cache):

- **Check**: latest tag + changelog summary + per-mode applicability
  (binary asset for the host arch vs GHCR tag).
- **Trigger by mode**:
  - Binary: in-process self-update — download versioned asset →
    checksum verify → stage → swap → health gate → rollback.
  - Docker: orchestrate + verify, host executes. The dashboard shows
    the exact host command (`VERSION=x.y.z docker compose pull &&
    docker compose up -d`) with copy button, then watches health
    and reports. It never rebuilds or recreates containers itself.
- **Failsafe protocol (both modes):**
  snapshot (version + config hash) → stage new → health gate
  (N consecutive `/healthz` 200 AND reported version match, bounded
  timeout) → success (notify) / failure (rollback to snapshot →
  re-verify → notify with reason).
- **Notify:** persistent dashboard banner + existing `WEBHOOK_URL` +
  log line. Every outcome (success, rollback, refused) is explicit;
  silence is never the signal.
- **Restart proxy button** (ships with the Center):
  `POST /admin/restart` (admin-only + CSRF + confirm dialog naming
  the cost: in-flight streams drop) → validate config (fail = abort,
  never die stupid) → drain (FINISH runs, persist sessions; the 30s
  grace already exists) → exit → supervisor restarts. Cheap because
  sessions + quota resume on boot (v1.8.8 resume work). Gated on a
  safe supervisor; otherwise warning + disabled.

## 4. What is explicitly out

- Watchtower / sidecar auto-pull (update without verification or
  intelligent rollback).
- Docker-socket mounts or in-container `docker` CLIs.
- Default-ON execution, floating tags, silent failures.
- Limited-tier / unrelated feature work — this plan is install +
  update + rollback only.

## 5. Execution order

1. GHCR publish on tag (+ compose image-first).
2. Official installers + supervisor registration.
3. Dashboard Update Center + Restart proxy (+ failsafe + notify).
4. Docs (install matrix per OS/mode, update/rollback runbook).

Each phase merges and deploys independently; later phases never
block earlier value. Implementation starts after plan review.
