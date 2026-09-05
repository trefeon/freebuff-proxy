# Test Strategy

State of the suite on 2026-09-06 (from the test inventory, `TestInventory`
scout): backend **~207 test files, ~1,399 `func Test` entries** across 25
packages plus `cmd`; frontend **63 Playwright e2e tests** (6 specs), zero unit
tests; `scripts/` untested; zero real-network tests (all upstream goes through
in-process `testutil.NewMock`/httptest); concurrency exercised via goroutine
hammers under `-race`, never `t.Parallel`.

## Test commands (hermetic)

```bash
env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./backend/...          # plain suite
env -u AUTH_TOKENS -u ADMIN_TOKEN go test -race ./backend/...    # Linux/CI only; -race does not run on Windows (cgo)
task test        # plain suite      task test:race    # race suite
npm --prefix frontend run check && npm run lint && npm run format:check && npm run build && npm run test:e2e
```

Never set `AUTH_TOKENS`/`ADMIN_TOKEN` when testing: leaked ambient tokens flip
bridge-mode tests into pooled mode. Most tests use
`testutil.UnsetConfigEnv(t)` + `t.Chdir(t.TempDir())` to avoid `.env` bleed;
follow that pattern for new tests.

## Coverage map (strongest → watch)

| Area | Strength | Protected by |
|---|---|---|
| SSE/Anthropic/OpenAI/Responses protocol + 13-harness conformance | strongest | server relay/replay/conformance suites (407 tests) |
| Pool failover precedence (ban>country>rate>waiting), cooldowns, quota windows | strong | pool suite (230) |
| Wire classification + transient retries + header fidelity | strong | upstream suite (143) |
| Session admission/poll/readmit, persistence, store CAS | strong | session suite (107) |
| Config precedence, keycatalog parity, atomic writes | strong | config suite (102) |
| Run lifecycle, drain queues, FINISH-once, shutdown races | good | runs suite (47) |
| Admin APIs/auth/CSRF, dashboard pages | good | dashboard + server admin tests (≈100) |
| Model parity vs upstream pins | good | modelcat/registry tests (25) |
| Real-binary lifecycle (serve/drain/update/config/doctor) | thin but real | cmd E2E (15) |
| SPA flows (tokens, quota, settings, logs, login/CSRF, a11y) | e2e only | frontend Playwright (63) |
| SPA unit logic (stores, validation, state) | GAP | none — see tech debt |
| Shell/PS1 scripts (install, gen-token, start, sync) | GAP | none — see tech debt |

## Regression test policy

Every bug fix answers: what broke, why, what test would have caught it, did we
add it? Prefer the test closest to the subsystem that owns the behavior; never
one giant regression file. Pin the observable contract (status, envelope,
headers, state transition), never log-line wording or incidental internals.

## What a good test looks like here

- Deterministic: no real network, no ambient env, temp-dir cwd, poll-until-
  deadline instead of fixed `time.Sleep` where flakiness was observed
  (see findings below).
- Behavior-first: assert what the client observes (status, code, SSE frames,
  token state) not wiring (field copies, call counts, log text).
- Race-covered: concurrency changes must pass `-race` on Linux CI; bridge and
  ledger code mutate shared state only under the owning lock.
- Isolated: full-suite-safe (`go test ./backend/...` in one run), no port or
  file collisions, `t.Parallel` avoided (serial suite by choice; hammers +
  race do the concurrency checking).

## Known weaknesses (from inventory 2026-09-06, track in TECH-DEBT.md)

1. Frontend/src has zero unit tests (HIGH): stores/validation bugs invisible
   to the 63 route-mocked E2E specs; no vitest script in package.json.
2. scripts/ untested (MEDIUM): installer/gen-token/start/sync wrappers ship silently.
3. Fixed `time.Sleep` waits (~34 sites; runs 400ms, admin_restart 300ms,
   pool_quota_window 1s, update 500ms/2s) are latent flakes; prefer the
   poll-until-deadline pattern already used elsewhere.
4. ~20 conditional skips (Windows-only ctrl+break/update-swap/chmod, root
   bypass, missing gitignored fixture for keycatalog parity): each skip is
   justified, but the keycatalog fixture skip can silently disable parity on
   fresh checkouts — pin it in CI with `FP_REGEN_FIXTURE` handling.
5. White-box log-wording pins (~4 server tests) break on cosmetic copy edits;
   migrating them to field-level assertions reduces maintenance cost.
6. No fuzz targets; parser edge cases (convert/schema) rely on hand-built
   tables only. Characterization over coverage: add fuzz seeds for
   `XMLToolCallExtractor`, schema normalization, and SSE sanitization.
7. SOCKS5 dial path (PROXY_ROTATION) selects/rotates in tests but never dials
   through a real SOCKS proxy.
8. E2E cmd suite compiles a real binary per suite (slow); keep new E2E at the
   invariant level, not per-case.

## Adding tests: acceptance bar

A new test earns its place only where a plausible bug would fail it: behavior,
boundaries, invariants, transitions, precedence, real errors. Same-path
parameter rows, bare not-throw, and non-empty checks are padding. Match the
package's existing conventions (mock via `testutil.NewMock`, hermetic env,
temp dirs) and name the invariant in a comment when it pins one.