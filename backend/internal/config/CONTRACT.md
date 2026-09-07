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

- Precedence, lowest to highest: built-in defaults < JSON `-config` < `.env` < DB settings overlay < environment (ADR-0019). The overlay arrives via `LoadOptions.Overlay` (canonical KEY→VALUE, read from the store by the caller — config imports nothing internal); `applyMappedValues` is the single key list shared by the file and overlay tiers. `SettingsBlockedKeys` (secrets + `UPSTREAM_BASE_URL` + `DB_PATH` + env-only `AUTO_DISCOVER_TOKEN`) never apply: POST rejects them and Load filters them. `OverlayFromRows` additionally drops values that fail `ValidateSettingValue`, so a tampered row can never poison a load. `SettingSources` is value-aware on the file tier (empty `.env` values and empty JSON values — null or blank strings — report `default`) and attributes the legacy `USER_ID` alias to `ACTING_USER_ID` on every tier that resolves it. Empty `AUTH_TOKENS` clears JSON/dotenv values (bridge mode), never merges.
- Every new key lands in the `dotenvKeys` catalog (byte-ascending order) or it silently does nothing; `TestDotenvFullKeySet` pins the full set.
- `Validate` must accept zero-values: tests construct `Config` literals directly, bypassing `Load` defaults (0 target = unset; the pool normalizes to its own default).
## Tests that protect it

`TestDotenv*` (precedence, BOM, CRLF, quoting/comments, duplicate-last-wins, JSON-wins, env-wins, missing-is-fine, empty-auth-clears), `TestEnv*` (bridge-mode detection), `TestDotenvFullKeySet(+EnvWins)`, `TestCORSAllowedOrigin`, `TestSessionPersist*`, `TestModelsHideUnavailableEnv`, `keycatalog_test.go`, `env_example_test.go`, `envfile_test.go`, `TestParseLevelGrammar`, `TestSettingsOverlay*` + `TestOverlayCoversCatalog` + `TestValidateSettingValue` + `TestSettingSources*` (ADR-0019 overlay precedence, blocked-key filtering, POST gate, source tiers).
`quota_autoprobe_test.go` (`QUOTA_AUTO_PROBE` default-true plus env/`.env`/DB-overlay tiers and the `ValidateSettingValue` bool gate).

## Safe modification patterns

- New knobs: shape-only `Validate` checks here (e.g. provider/model slash shape); semantic checks (served/unmetered) live where `modelcat` is visible (pool fire path + admin endpoint).
- New default-true live-apply bools (e.g. `QUOTA_AUTO_PROBE`, ADR-0022): default in `defaultRawConfig`, `overrideBool` (env tier) + `overrideBoolFrom` in `applyMappedValues` (shared `.env`/overlay tier — overlay + `ValidateSettingValue` need no extra code), `Config` + `rawConfig` fields, GroupPool catalog entry (byte-ascending slot) + `renderKey` case + `dotenvKeys` entry, then regen the `config-meta.json` fixture with `FP_REGEN_FIXTURE=1`. No `Validate` case (bool zero = off; production default ON comes from `Load`).
- New default-false live-apply knob groups with ranged values (e.g. burst balance, ADR-0023): same wiring as above, plus `Validate` range checks that accept zero-values (0 = unset; the consumer normalizes to its default) and `ValidateSettingValue` semantic gates for what the kind gate cannot express (BURST_MAX_TOKENS >= 2, BURST_WINDOW duration parse — the RATE_LIMIT_PER_IP precedent). Regen the `config-meta.json` fixture the same way.
- Never import an internal package for validation logic — move the logic down into config instead.
- Duplicate-key files: last wins; quoting/BOM/CRLF handling must stay total (fuzz-adjacent edge tests pin it).
