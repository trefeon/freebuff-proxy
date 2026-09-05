# AI Code Review Checklist

Run this on every change before claiming it is done. Each section is
pass/fail; a single fail blocks the change.

## Correctness

- [ ] The change does the smallest thing that solves the stated problem.
- [ ] No behavior changed beyond the task scope (statuses, envelopes, headers,
      state transitions, defaults identical unless the task says otherwise).
- [ ] What assumption could I be wrong about? Named and checked against code,
      not memory.
- [ ] What existing behavior might this accidentally modify? Callers searched
      (`lsp` references), not guessed.

## Architecture

- [ ] The touched package still owns what its CONTRACT.md says it owns.
- [ ] No new import edge outside `backend/internal/archtest/arch_test.go`
      allowlist (run the archtest package explicitly after dependency changes).
- [ ] No upward edge (leaf/L1→anything, convert→server family, upstream→
      session, session→pool, pool→server/dashboard, dashboard→server).
- [ ] No new abstraction for fewer than three call sites; no moved
      responsibility without updating the CONTRACT.md and any affected ADR.

## Concurrency

- [ ] Shared state accessed only under the owning lock (bridge entries under
      `bridgeMu`; network calls outside `bridgeMu`).
- [ ] New goroutines/timers bound to a lifecycle (shutdown context, server
      lifetime); no leaks, no double-close, no forgotten cancellation.
- [ ] Race-sensitive paths verified with `-race` on Linux CI (note it locally
      when Windows-only: "race pending on CI").

## Security

- [ ] No secret, token, cookie, key, or full auth header in logs, dumps,
      errors, metrics labels, or new test fixtures (secrets render redacted).
- [ ] New admin endpoint has the correct `AdminRoutes` auth level + CSRF gate
      (state-changing POST) + loopback consideration.
- [ ] New error path maps to the right surface envelope (no upstream bodies or
      internal details leaked to clients).
- [ ] New config/env/CLI input validated; error strings and cookie flags
      follow existing patterns.

## Compatibility

- [ ] No protocol change without updating `docs/api/API-ENDPOINTS.md`, the
      README surface tables, and the conformance/replay suites.
- [ ] No upstream egress change (headers, UA, fingerprints, envelope) without
      ADR-0012 review and a wire test.
- [ ] `/v1/models` entries, modelcat facts, and registry maps moved together
      when a model changed (with upstream SHA recorded).
- [ ] Config knob added to `keycatalog.go` + `dotenvKeys` + templates + README
      + fixture, or explicitly out of scope for the knob.

## Tests

- [ ] What test proves the new behavior? Named; fails pre-fix, passes post-fix.
- [ ] Regression tests sit in the owning package, not a giant shared file.
- [ ] No test asserts implementation wording (log text), wiring, or incidental
      defaults; each test would fail on a plausible bug.
- [ ] New/changed package still passes its own suite + the hermetic suite;
      E2E added only at the invariant level.

## Documentation

- [ ] CONTRACT.md updated when responsibility/dependencies moved.
- [ ] ADR written or marked superseded when a decision changed.
- [ ] INVARIANTS.md updated when a contract-level behavior moved.
- [ ] README/docs touched only for user-visible changes; no duplicate facts
      created (see docs/maintenance/TECH-DEBT.md duplication notes).

## Diff hygiene

- [ ] Nothing unrelated touched (`git status` clean except the task's files).
- [ ] No dead code, commented-out blocks, stray debug dumps, or temp files.
- [ ] No dependency changes unless the task required them (then: why pinned,
      Dependabot coverage, checksum verification).
- [ ] Generated assets (`dashboard/dist`) rebuilt and committed atomically
      when frontend changed (CI will check anyway).
- [ ] Struct-literal edits verified with `git diff | grep '^-[^-]'`.