# Package Contract: `backend/internal/testutil`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Shared test scaffolding: hermetic environment isolation, a mock upstream codebuff server (`MockUpstream`), SSE framing helpers, polling assertions, and temp-file hygiene. Imported by package test suites; ships no production code paths.

## Public API (stable surface)

- Env isolation: `UnsetConfigEnv(t)`, `UnsetConfigEnvForTestMain()`.
- Mock upstream: `MockUpstream`, `NewMock()`, `StartRequest`, `RecordedStep`, `FinishedRun`.
- Helpers: `SSEEvent(data)`, `WaitFor(t, timeout, cond, ...)`, `DrainStrayTempFiles(t, dir)`.

## Allowed dependencies

`config` — exactly the set pinned by `archtest` (env-key knowledge for isolation).

## Forbidden dependencies

Everything else, especially `pool`, `server`, and `upstream` (the mock must not import the real client — wire fidelity is maintained by hand against the envelope contract, and importing the client would test the mock against itself).

## Critical invariants

- Hermetic by default: suites using `UnsetConfigEnv` must not see `AUTH_TOKENS`/`ADMIN_TOKEN` from the developer machine — a passing suite on a dirty env is a false pass.
- Mock envelope fidelity: `MockUpstream` request/response shapes must track the real `codebuff_metadata` envelope (run_id, client_id, cost_mode, instance id) — when `upstream` changes the envelope, update the mock in the same commit or downstream suites assert against a lie.
- No stray temp files (`DrainStrayTempFiles`) — leaked fixtures break hermetic re-runs.

## Tests that protect it

None of its own (`[no test files]`); its contract is enforced by every consumer suite. Breakage surfaces as mass suite failure, not a local red test.

## Safe modification patterns

- New helper: keep it hermetic (no network, no real env reads); document which suites consume it.
- Mock shape change: grep consumers for the old shape first — the mock is a shared fixture, not a private stub.
- Never add production imports here; this package must stay importable from any test binary without pulling the world.
