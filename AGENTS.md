# AGENTS.md — freebuff-proxy operating guide

Machine-readable rules for agents working in this repo. Human overview lives in
`README.md`; visual grammar in `DESIGN.md`; multi-agent workflow in
`docs/AGENTIC-WORKFLOW.md`.

## 1. Identity

- Go 1.26 (`go.mod`) FreeBuff wire gateway. OpenAI-compatible surfaces
  (`/v1/chat/completions`, `/v1/models` — see `backend/cmd/freebuff-proxy/e2e_test.go`,
  `backend/internal/cli/cli_serve.go`) plus an Anthropic translation layer
  (`backend/internal/server/anthropic*.go`).
- Svelte 5 dashboard (`frontend/`, `freebuff-proxy-dashboard`) embedded via
  `go:embed` (`backend/internal/dashboard/assets_embed.go`) and served at `/admin`.
  Health probe: `GET /healthz` → 200.
- Modes (`backend/internal/config/config.go:HybridBridgeMode/EffectiveMode`):
  pooled (`AUTH_TOKENS` set + `BRIDGE_ENABLED=0`), bridge (`AUTH_TOKENS`
  empty, per-request client token), hybrid (default when `AUTH_TOKENS` set:
  `API_KEYS` credential uses the pool, any other credential relays as bridge).
- Freebucks meter: the wire `prices` map is the sole cost source; charge-once at
  session start; 1h sessions; `DELETE` refund; Pacific-midnight refill.
  `deepseek/deepseek-v4-flash` is an unpriced row (verified cost-0 live 2026-09-08).

## 2. Topology

- `backend/` — Go gateway (`cmd/`, `internal/`). `internal/` packages include
  `server`, `pool`, `upstream`, `session`, `store`, `config`, `dashboard`,
  `modelcat`, `registry`, `wirefacts`.
- `frontend/` — Svelte 5 SPA. Committed bundle
  `backend/internal/dashboard/dist` is what the binary serves.
- `upstream/freebuff` — gitignored live vendor clone of `CodebuffAI/freebuff`, never commit. Source of truth for all wire/registry/model work. Keep freshly fetched to `origin/main` before starting; pins live in `backend/internal/wirefacts/testdata/wire/snapshots.json` (`upstream_sha`) + `scripts/vendor-version.txt`, verified by `scripts/check-upstream.sh`.
- `scripts/` — `sync-upstream.sh`, `check-upstream.sh` (canonical parity check),
  `review-wire-drift.sh`.
- `.github/workflows/` — `ci.yml` (jobs `test`, `frontend`), `lint.yml` (job
  `golangci`), `codeql.yml` (job `analyze`), `dependency-review.yml`,
  `upstream-drift.yml`, `release.yml`.

## 3. Commands

```sh
# Hermetic backend tests (CI equivalent: go test -race -timeout 10m ./backend/...)
env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./backend/...

# Build / vet / lint
go build ./backend/...
go vet ./backend/...
golangci-lint run ./backend/...

# Frontend (run inside frontend/)
npm run check && npm run lint && npm run format:check
npm run test:e2e          # Playwright SPA suite (needs built dist)
npm run build             # vite build → refresh backend/internal/dashboard/dist
```

Knob chain: any `.env` knob must propagate
dotenv → static → live → SSE hash → store refresh.

## 4. Workflow (protected main)

1. Feature branch off `origin/main` → PR → CI gates
   (`test`, `frontend`, `golangci`, `analyze`/CodeQL, `dependency-review`) green →
   squash merge. `gh pr update-branch` takes NO `--merge` flag on this host.
2. Conventional Commits (`feat|fix|chore|docs|…(scope): subject`).
3. Never stage/commit unless asked. Never commit secrets, `reference/`, or devdocs.
4. No local docker. Preview on acerblue from a `/tmp` worktree (never the shared
   checkout — it carries uncommitted user work):
   `docker build --network=host` + compose up, then `GET /healthz` → 200.
   Prod is VPS SG.
5. Frontend `dist` is rebuilt and committed LAST (dist-freshness CI diffs the
   bundle; any `src` touch after `vite build` fails it).
6. Upstream syncs: classify wire drift BEFORE refreshing the baseline, else
   `review-wire-drift.sh` reports all-SAME against the new anchors and hides
   FUNCTIONAL rows. LF-normalize `snapshots.json` comparisons (CRLF checkouts
   fake drift). Merge drift PRs serially wire → registry → dashboard.
7. Upstream-first: start any wire/registry/model work by updating `upstream/freebuff` to latest `origin/main` (`git -C upstream/freebuff fetch origin main`, checkout `origin/main`). Nothing gates or pre-approves this update. If it moved past the recorded pins, classify with `check-upstream.sh` + `review-wire-drift.sh` and carry any port/re-pin through the drift PR flow.

## 5. Budgets and freezes (as observed)

- Autonomy under ~10-step rails; fan out via isolated lanes (see
  `docs/AGENTIC-WORKFLOW.md` §1).
- Request limits default 30/min, 1500/day Pacific; `SAFE_MODE=true` is the
  anti-ban preset (`.env.example`); `COST_MODE=free`.
- Test flake policy: single FAIL with greens before/after (e.g. wall-clock
  quota-boot probe before ~09:05 PDT) is note-and-move-on after 2 reruns;
  reproduce on pristine `main` before blaming the branch.
- `archtest.test.exe` "Access is denied" on Windows is the AV block; hand-verify
  via import grep, CI Linux is the real proof.
- Public repo: zero secrets in code, transcripts, or comments. Rotate on
  suspicion; test live with user-provided keys only.
