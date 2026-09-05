# Package Contract: `backend/internal/config`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Bottom-layer typed configuration: loads `.env` + JSON `-config` + process environment with fixed precedence, validates shape, and hot-reloads via `atomic.Pointer`. Every other internal package reads config; config reads nothing internal.

## Public API (stable surface)

- Loading: `Load(configPath) (Config, error)`, `LoadOpts(configPath, LoadOptions)`, `EnvFileCandidates()`, `ResolveEnvFile()`.
- Modes: `Config.BridgeMode()` (true when `AuthTokens` is empty), `HybridBridgeMode()`, `EffectiveMode()`, `RequireLogin()`, `IsDefaultAdminToken()`.
- Keys: `dotenvKeys` catalog in keycatalog.go; `modelsAllowList` / `quotaFallbackModelsList` JSON unmarshalers.
- Levels: `ParseLevel(s)` + `LevelTrace` in loglevel.go (telemetry forwards to this; do not duplicate).

## Allowed dependencies

None. Zero internal imports — enforced by `layer_imports_test.go` (`TestConfigDoesNotImportTelemetry` scans non-test files) and `archtest`.

## Forbidden dependencies

Everything internal, including `telemetry` (the 2026-08-31 P3 inversion: LOG_LEVEL validation used to call `telemetry.ParseLevel` from the bottom layer).

## Critical invariants

- Precedence, lowest to highest: built-in defaults < JSON `-config` < `.env` < environment. Empty `AUTH_TOKENS` clears JSON/dotenv values (bridge mode), never merges.
- Every new key lands in the `dotenvKeys` catalog (byte-ascending order) or it silently does nothing; `TestDotenvFullKeySet` pins the full set.
- `Validate` must accept zero-values: tests construct `Config` literals directly, bypassing `Load` defaults (0 target = unset; the pool normalizes to its own default).
- New env knobs with frontend surface need `FP_REGEN_FIXTURE=1` regen of `frontend/e2e/fixtures/config-meta.json`.

## Tests that protect it

`TestDotenv*` (precedence, BOM, CRLF, quoting/comments, duplicate-last-wins, JSON-wins, env-wins, missing-is-fine, empty-auth-clears), `TestEnv*` (bridge-mode detection), `TestDotenvFullKeySet(+EnvWins)`, `TestCORSAllowedOrigin`, `TestSessionPersist*`, `TestModelsHideUnavailableEnv`, `keycatalog_test.go`, `env_example_test.go`, `envfile_test.go`, `TestParseLevelGrammar`.

## Safe modification patterns

- New knobs: shape-only `Validate` checks here (e.g. provider/model slash shape); semantic checks (served/unmetered) live where `modelcat` is visible (pool fire path + admin endpoint).
- Never import an internal package for validation logic — move the logic down into config instead.
- Duplicate-key files: last wins; quoting/BOM/CRLF handling must stay total (fuzz-adjacent edge tests pin it).
