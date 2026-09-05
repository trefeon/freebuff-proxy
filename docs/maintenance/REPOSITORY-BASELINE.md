# Repository Baseline (2026-09-06)

Health report for `trefeon/freebuff-proxy` at the start of the hardening
program. Method: full tree + CI + test inventory (two delivered audit reports
plus targeted source reads for the remaining areas), secret scan of all
tracked files, and workflow review.
Severity reflects risk to correctness, security, release safety, or
AI-maintainability.

## Headline state

| Area | Verdict |
|---|---|
| Architecture | GOOD: layered, matrix-guarded (archtest allowlist), contracts exist for convert/pool, invariants documented in code comments |
| Testing | STRONG backend (~207 files, ~1,399 tests, zero real network, race-covered), THIN top (15 E2E), frontend e2e-only (63 Playwright, 0 units), scripts untested |
| CI/CD | STRICT: gofmt, build, race tests, frontend gates + dist-freshness, CodeQL, dependency-review, drift CI, gated releases + SLSA |
| Security | STRONG posture, ONE HIGH finding (factory-default remote bootstrap), hygiene verified clean |
| Dependencies | PINNED: go.sum, package-lock, SHA-pinned actions, Dependabot groups |
| Documentation | RICH but FLAT: README/AGENTS/DESIGN + 14 flat docs/*.md; guard rationale lived outside the committed tree; docs/ subdirs were gitignored |
| Release | GATED: tag-push re-runs the hermetic suite + lint + frontend before GoReleaser |
| AI-readiness | GOOD foundations, knowledge too centralized (AGENTS.md + memory) |

## Findings

### CRITICAL

None. No real secrets in tracked files (all credential-like literals are
test/fixture placeholders), no remote unauthenticated config write, no broken
release gate.

### HIGH

**H-1. Factory-default admin credential allows remote full-admin takeover
via the documented bootstrap flow.**
Why it matters: `ADMIN_TOKEN=123456` is public knowledge (`.env.example`);
`/admin/login` has no loopback gate, and `/admin/api/change-password` is the
deliberate BOOTSTRAP EXEMPTION from the loopback restriction (pinned by
`admin_password_remote_test.go:19`). Any exposed deployment that forgot to
rotate is owned in two requests, zero brute force.
Affected: `backend/internal/server/admin_auth.go`, operators.
Evidence: 2026-09-06 security audit §1/§3; README "change-password works from
anywhere".
Recommended action: owner decision between (a) loopback-gate the bootstrap
while the factory default is active (same rule as all other sensitive
routes), or (b) boot-generated one-time secret printed to startup logs. Do
NOT silently change; the exemption is documented behavior with a pinned test.
Risk if ignored: silent takeover of careless deployments.
Recorded as P1 in TECH-DEBT.md (needs behavior decision).

**H-2. New `docs/` subdirectories were gitignored.**
Why it matters: `.gitignore` has `docs/*` + `!docs/*.md`, so the whole
`docs/architecture|decisions|development|testing|security|operations|
upstream|api` tree this program adds would be invisible to git.
Affected: `.gitignore`, all new docs.
Action: add explicit `!docs/<dir>/` negations (done in this program).
Risk if ignored: the documentation deliverable silently never commits.

### MEDIUM

**M-1. Architecture rationale lived outside the committed tree.**
`backend/internal/archtest/arch_test.go` (matrix guard) cited layer rules that
were not part of the committed tree: a fresh clone got the guard without the
why. Fixed in this program: `docs/architecture/BOUNDARIES.md` is the committed
source of truth and the archtest header now points at it.

**M-2. Frontend has zero unit tests.**
63 Playwright e2e specs (route-mocked) cover flows, but Svelte store/
validation/state logic has no unit layer and `package.json` has no
unit-test script. Recorded P1 in TECH-DEBT.md.

**M-3. `scripts/` (installers, gen-token, start-proxy, sync-upstream) are
untested.** The Go sides (setup/update/doctor) are tested; the shell/PS1
wrappers ship silently. Recorded P2.

**M-4. Latent timing flakes in the Go suite.** ~34 fixed `time.Sleep` waits;
several are timing-sensitive (runs 400ms, admin_restart 300ms,
pool_quota_window 1s, update 500ms/2s, leader-election 20ms). Recorded P2;
prefer the poll-until-deadline pattern already used elsewhere.

**M-5. Conditional skips can silently disable parity.** ~20 skip sites are
justified (Windows-only, root, chmod), but the keycatalog fixture-parity skip
fires when the gitignored fixture is absent — on a fresh checkout the parity
can go unchecked. Recorded P2.

### LOW

**L-1. `.dockerignore` lagged `.gitignore`.** `API_Keys*`,
session-state hashes, `*.tmp`, agent scratch rode the build context
(`COPY . .`); never entered the final image. Fixed in this program (mirrored
secret-class entries).

**L-2. Release job permissions were workflow-level.** `release.yml`
test/frontend jobs inherited `contents:write`. Fixed in this program
(scoped to `contents:read`).

**L-3. Open `/v1` in pooled mode was silent.** `AUTH_TOKENS` set + empty
`API_KEYS` exposes the pool to the network with no startup warning.
Fixed in this program (warning mirroring the open-admin one).

**L-4. SOCKS5 dial path untested** (rotation selection tested, actual dial
not). Recorded P3.

**L-5. White-box log-wording pins** (~4 server tests) break on cosmetic copy
edits. Recorded P3.

### INFORMATIONAL

- No `t.Parallel` anywhere (serial suite by choice; hammers + `-race` do the
  concurrency checking). No fuzz targets (parser tables are hand-built).
- Registry live-refresh fetch path asserts URL construction, not fetch.
- Windows behavior is heavily tested but Windows-only tests skip elsewhere.
- Upstream error bodies surface verbatim to clients (redacted for `cb_`/
  `Bearer` shapes; coverage coupled to those regexes).
- Model tables in README embed vendor-snapshot facts (date + SHA stamped);
  they rot on upstream drift — the registry parity test catches code drift,
  README prose needs a sync-time glance.

## Recommended priorities (executed order)

1. H-2, M-1, L-1, L-2, L-3: done in this program (guardrails + docs).
2. H-1: owner decision (P1, documented options).
3. M-2: frontend unit harness (P1, scoped to stores/validation first).
4. M-3/M-4/M-5, L-4/L-5: P2/P3 tech-debt queue.

## AI-agent readiness (before → after)

- Before: AGENTS.md carried the architecture; the layer rationale lived outside
  the committed tree; contracts existed only for convert/pool.
- After: `docs/architecture/` (overview/boundaries/dependencies/invariants/
  glossary), 12 ADRs, CONTRACT.md for 13 packages, testing/security/upstream/
  api/operations docs, AI review checklist, refactoring policy, canonical
  `task verify`, scoped CI permissions, baseline + tech-debt registers.