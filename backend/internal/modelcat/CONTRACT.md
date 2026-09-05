# Package Contract: `backend/internal/modelcat`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Single source of truth for per-model facts: served/paused/premium, caps and cap pools, context windows, reasoning-effort ladders, display metadata. Consumers (`registry`, `convert`, UI) derive; nothing else owns a model table.

## Public API (stable surface)

- Facts: `ModelInfo`, `IsServed`, `IsPaused`, `PausedReplacement`, `WithdrawnModelMessage`, `IsPremium`, `SharedPremiumModels`, `PerModelCap(id) (limit, pool)`, `ContextWindow`, `Efforts`, `IsMediumlessLadderModel`, `IsStrictReasoningModel`, `IsLimitedTierAllowed`.
- Sets: `PausedMap`, `ServedMap`, `ServedIDs`, `ServedHelpText`.
- Display: `DisplayName`, `Tagline`, `Notice`, `Badges`.
- Defaults: `DefaultModelID` (z-ai/glm-5.3-flash), `FallbackModelID` (mimo/mimo-v2.5), `LimitedModelID`, `PremiumSessionLimit` (4), `GLMSessionLength` (1h).

## Allowed dependencies

None. Stdlib only — enforced by `archtest`.

## Forbidden dependencies

Everything internal, especially `upstream` (facts come from pinned snapshots + review, never live fetch) and `registry` (registry derives from modelcat, not the reverse).

## Critical invariants

- `Catalog` row order is pinned (`SUPPORTED` order); the parity test fails on reorder or drift against the pinned upstream snapshot.
- `catalog_test.go` pins the upstream snapshot: catalog changes require a synced `reference/freebuff` and usually a matching `registry` fallback-map update.
- Effort ladders are the canonical copy — `convert` delegates here, never keeps a local table.

## Tests that protect it

`TestCatalogParityWithPinnedUpstream`, `TestCatalogFactsPinned`, `TestIsMediumlessLadderModel`, `TestIsStrictReasoningModel`.

## Safe modification patterns

- Model fact change (pause/serve/cap/effort): update the row + run the parity test; if the test fails, sync upstream first (`scripts/sync-upstream.sh --check`) — the test is usually right.
- New model: add the row in `SUPPORTED`-order position, wire `registry` fallbacks separately; never invent availability — verify against the pinned snapshot.
