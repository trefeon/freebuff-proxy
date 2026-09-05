# Technical Debt Register

Priorities: P0 breaks safety/correctness/release; P1 significant
reliability/security/maintainability; P2 meaningful improvement; P3
nice-to-have. Only items with engineering impact; no aesthetic complaints.

## P1

### TD-P1-1 — Factory-default admin bootstrap allows remote takeover
Description: with public default `ADMIN_TOKEN=123456`, remote login +
`change-password` (BOOTSTRAP EXEMPTION) owns the instance in two requests.
Reason: deliberate bootstrap UX (remote first-run) + public default.
Risk: takeover of careless exposed deployments.
Impact: security. Effort: small code, needs owner decision.
Suggested approach: (a) loopback-gate change-password while factory default
is active (breaks remote first-run; update README + the pinned remote test),
or (b) boot-generated one-time secret in startup logs. Do not silently change.
Blocked by: owner choice. Evidence: SECURITY-MODEL gap 1, baseline H-1.

### TD-P1-2 — Frontend has zero unit tests
Description: SPA logic (stores, validation, state) covered only by 63
route-mocked Playwright specs; no vitest script.
Reason: e2e-first history.
Risk: logic bugs in non-rendered paths invisible; slow feedback.
Impact: frontend reliability. Effort: medium (harness + stores/validation first).
Suggested approach: add vitest, unit-test `frontend/src/lib/stores/*` and
form validation first; keep e2e for flows.
Blocked by: nothing. Evidence: test inventory §3.

## P2

### TD-P2-1 — scripts/ wrappers untested
install/gen-token/start/sync shell+PS1 ship silently (Go sides tested).
Approach: shellcheck in CI + smoke tests for gen-token/start arg parsing;
sync-upstream already exercised by drift CI. Effort: small-medium.

### TD-P2-2 — Fixed time.Sleep waits are latent flakes
~34 sites; sensitive: runs 400ms, admin_restart 300ms, pool_quota_window 1s,
update 500ms/2s, leader-election 20ms.
Approach: migrate to poll-until-deadline (pattern exists in
pool_mismatch_test.go:49). Effort: small, mechanical.

### TD-P2-3 — keycatalog fixture parity can skip silently
When the generated fixture is absent (fresh checkout), parity goes unchecked.
Approach: regenerate in CI or fail-closed when the fixture path is missing.
Effort: small.

### TD-P2-4 — No fuzz targets for parsers
XML tool-call extractor, schema normalization, SSE sanitization rely on
hand-built tables.
Approach: add `Fuzz*` seeds in convert; run briefly in CI or nightly.
Effort: small-medium.

### TD-P2-5 — X-Forwarded-Proto trusted unconditionally for Secure cookies
Header injection on plain-HTTP deployments can force Secure cookies browsers
refuse (self-DoS); document/require `ADMIN_FORCE_SECURE_COOKIES` behind
proxies, optionally add a trusted-proxy allowlist. Effort: small.

### TD-P2-6 — Redaction coverage coupled to `cb_`/`Bearer` shapes
Adequate today; a future non-cb_ token format would flow to clients/logs.
Approach: extend `telemetry.RedactSecrets` with new formats at introduction;
consider hint-only client messages. Effort: small, per-format.

## P3

### TD-P3-1 — SOCKS5 dial path untested
Rotation selection tested; no test dials through a real SOCKS5 proxy.
Approach: in-process SOCKS5 stub in a dial test. Effort: small.

### TD-P3-2 — Log-wording pins in ~4 server tests
Break on cosmetic copy edits. Approach: migrate to field-level assertions.
Effort: trivial.

### TD-P3-3 — E2E cmd suite is slow
Compiles a real binary per suite; prewarms ~13 agent runs.
Approach: keep new E2E at invariant level; consider shared build cache.
Effort: medium, low value — only if suite time hurts.

### TD-P3-4 — Helper-only test files report 0% by function
`pool_test.go`, `teststack_test.go`, `session_test.go`, `convert_test.go`
carry harness code. Approach: none required; informational.

## Closed in this program (were findings, now fixed)

- H-2 docs-subdir gitignore → `.gitignore` negations added.
- M-1 rationale in gitignored devdocs → `docs/architecture/BOUNDARIES.md`
  committed; archtest header repointed.
- L-1 `.dockerignore` lag → secret-class entries mirrored.
- L-2 release job permissions → test/frontend scoped to `contents:read`.
- L-3 silent open `/v1` → startup warning added in `cli.Serve`.
- PR template → scope/behavior/protocol/security rows added.