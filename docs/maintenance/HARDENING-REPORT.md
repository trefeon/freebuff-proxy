# Repository Hardening Report (2026-09-06)

## Executive Summary

Waves 1–3 of the hardening program are complete; Waves 4–5 were deliberately
scoped down per the program's own sequencing rule (document first, change
little, defer big test work to the prioritized queue). No rewrites, no
behavior changes, no dependency moves. One HIGH security finding needs an
owner decision (documented, not silently fixed). Full hermetic Go suite is
green, including one transient failure re-verified green. **Final status:
PASS WITH FOLLOW-UP.**

## Current Architecture

Layered Go gateway (`backend/internal`): leaves (config, modelcat, stealth,
…) → convert/upstream → session/runs → pool/dashboard → server/cli, enforced
by the `archtest` dependency-matrix test. Three API surfaces share one
translation core; token lifecycle (cooldown/quarantine/quota-lock) is the
highest-churn logic; the Svelte 5 dashboard is go:embed'd. Full map:
`docs/architecture/OVERVIEW.md`, `DEPENDENCIES.md`, `BOUNDARIES.md`.

## Changes Made

Docs (new, committed-tree): `docs/architecture/` (5 files),
`docs/decisions/` (index + ADR-0001…ADR-0012), `docs/development/`
(REFACTORING.md, AI-CODE-REVIEW.md), `docs/testing/TEST-STRATEGY.md`,
`docs/security/` (SECURITY-MODEL.md, THREAT-MODEL.md), `docs/upstream/`,
`docs/api/`, `docs/operations/`, `docs/maintenance/` (baseline, tech-debt,
this report). Package contracts: server, session, upstream, registry,
modelcat, config, runs, dashboard, reasoningcache, ratelimit, telemetry, cli
(remaining leaf packages covered by their own CONTRACTs).

Code (minimal, additive): `openAPIWarning` + startup call + `TestOpenAPIWarning`
(`backend/internal/cli/`); `task verify` / `task verify:full` (Taskfile) +
`make` mirrors; release.yml test/frontend jobs scoped to `contents:read`;
`.gitignore` docs-subdir negations; `.dockerignore` secret-class mirror;
PR template rows (architecture/security/generated/upstream).

Doc-only touch: archtest header now cites committed `BOUNDARIES.md` instead
of its previous uncommitted location.

## Files Added / Modified / Removed

- Added: 30 docs + 12 contracts + 1 test function. Removed: none.
- Modified (mine): `.dockerignore`, `.gitignore`, `Makefile`, `Taskfile.yml`,
  `.github/pull_request_template.md`, `.github/workflows/release.yml`,
  `backend/internal/archtest/arch_test.go` (comment only),
  `backend/internal/cli/cli.go`, `backend/internal/cli/cli_test.go`.
- Out of scope for this program (left untouched; stage only the paths listed
  above): AGENTS.md, config/*, convert/*, dashboard/*, server/*, frontend/*,
  leaf CONTRACTs, archtest body, conformance/toolmap tests.

## Architecture / AI-Agent / Testing / Security / CI / Documentation Improvements

- Boundaries are now committed prose + machine guard (archtest green).
- 12 ADRs capture the WHYs agents kept rediscovering (modes, lifecycle,
  translation, sync, config, admin, reasoning, release, catalog, anti-ban).
- Test strategy + regression policy + review checklist + refactoring policy
  set the contribution bar; inventory (207 files/1,399 tests) is on record.
- Security: secret-free repo verified, trust boundaries + threat model
  written, two LOW/INFO gaps fixed (dockerignore, release perms, open-/v1
  warning), one HIGH recorded with options (TD-P1-1).
- CI: strict and unchanged in behavior; release jobs least-privilege; PR
  template asks the right questions.

## Technical Debt Remaining

P1: TD-P1-1 (factory-default bootstrap decision), TD-P1-2 (frontend unit
harness). P2: scripts tests, sleep-flakes, fixture-parity skip, fuzz seeds,
XFP trust, redaction coupling. P3: SOCKS dial test, log-wording pins, E2E
speed. Full register: `docs/maintenance/TECH-DEBT.md`.

## Known Risks / Deferred Work

- H-1 (TD-P1-1) is a real remote-takeover path on careless deployments;
  mitigation today is docs + banner + rotation guidance.
- One transient full-suite red (convert test file, mid-program) re-verified
  green with no fix needed. Commit reviewed slices before the next program
  wave so verification runs against a stable tree.
- Frontend unit tests and scripts tests are queued, not done.
- `-race` and Playwright e2e were not run locally (Windows cgo / browsers);
  they run on Linux CI.

## Verification Results

- `go build ./backend/...`: PASS.
- Hermetic suite `env -u AUTH_TOKENS -u ADMIN_TOKEN go test -count=1
  ./backend/...`: PASS, zero FAIL lines (all 25 packages + cmd E2E; one
  transient convert failure re-verified green in isolation and the final pass).
- `go vet` on edited packages: PASS. `gofmt -l` on edited files: clean
  (`archtest.go` carries pre-existing format drift outside this program's
  one-line comment change; left untouched, the CI gofmt gate flags it on commit).
- `archtest` matrix + new `TestOpenAPIWarning`: PASS.
- release.yml YAML parse + per-job permissions: verified.
- Taskfile YAML parse + `verify`/`verify:full` present; Makefile mirrors present.
- Frontend checks / e2e / `-race`: NOT run locally (unchanged code; CI-owned).

## Final Repository Status

**PASS WITH FOLLOW-UP** — follow-ups: owner decision on TD-P1-1, frontend
unit harness (TD-P1-2), P2 test-hardening queue, review + staging per the file lists above.