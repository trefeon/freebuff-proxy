# Package Contract: `backend/internal/registry`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Model→agent mapping and alias resolution: which upstream agent serves a model (`ResolveModel`, `AgentForModel`), per-model root/agent fallbacks (`fallbackAgents`, `fallbackRootByModel`), and the served-model list derived from `modelcat`. Refreshes from remote sources with failure-safe state.

## Public API (stable surface)

- Construction: `New(cfg, client)`, `SetLogger`, `SetConfig`, `SetSources`, `LoadFallback`.
- Resolution: `ResolveModel(model)`, `AgentForModel(model)`, `Models()`, `ModelCount()`, `AgentIDs()`.
- Refresh: `Refresh(ctx)`, `LastAttemptedSources()`.

## Allowed dependencies

`config`, plus `modelcat` in tests only (`registry_test.go`; non-test code resolves via its own pinned maps + plain HTTP refresh) — exactly the set pinned by `archtest`.

## Forbidden dependencies

Everything else, especially `server` (registry resolves; it is not an orchestration point), `pool`, and `upstream` (refresh uses a plain `http.Client`, never the wire client).

## Critical invariants

- `fallbackAgents` / `fallbackRootByModel` must match the pinned upstream snapshot; the parity test fails on drift — update them together with the snapshot, never from memory.
- Refresh is failure-safe: non-200, empty, over-limit, canceled, or partial sources keep the previous state (`Refresh*KeepsState` tests). `RefreshNoModelsResolvedGuard` prevents wiping the map.
- Aliases resolve one hop (`TestModelAliasesOneHop`); concurrent access is safe (`TestConcurrentAccess`).

## Tests that protect it

`TestFallbackMap`, `TestFallbackParityWithPinnedUpstream`, `TestFallbackPrunesRetiredModels`, `TestResolver`, `TestRefreshFromFixture`, `TestRefresh*KeepsState` (failure/empty/over-limit/non-200/canceled/partial), `TestModelAliases(+OneHop)`, `TestConcurrentAccess`, `TestRefreshNoModelsResolvedGuard`, `TestMirrorForURLConstruction`.

## Safe modification patterns

- Upstream model churn: sync reference first, update fallback maps + `modelcat` rows in the same change (TYPE C work — never mixed with features).
- New alias: one-hop only, add a `TestModelAliases` case; object-property names with regex metacharacters need escaping (`TestResolveObjectPropertyRegexMetachar`).
- Refresh sources: bounded fetch (2 MiB cap); never block serving on refresh — state updates are atomic swaps.
