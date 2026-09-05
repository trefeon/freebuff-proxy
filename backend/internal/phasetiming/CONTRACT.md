# Package Contract: `backend/internal/phasetiming`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Leaf package: request-lifecycle phase timings carried on `context.Context` (admit → session → run → chat → relay phases). Powers the phase-timing breakdown in logs and the dashboard without polluting function signatures.

## Public API (stable surface)

- `Phases`, `New()`; `WithContext(ctx) (ctx, *Phases)`, `FromContext(ctx) *Phases`; `Since` markers and `All()` snapshot.

## Allowed dependencies

None. Zero internal imports — enforced by `archtest`.

## Forbidden dependencies

Everything internal. Timing must be dependency-free so any layer (including leaves) can record phases.

## Critical invariants

- Nil-safe everywhere (`TestNilSafety`) — phases are optional context baggage; absent context must never panic a hot path.
- `All()` returns a copy (`TestAllReturnsCopy`) — callers must not alias the live map across goroutines.
- Context carrying round-trips (`TestContextCarrying`); `Since`/`All` agree (`TestSinceAndAll`).

## Tests that protect it

`TestSinceAndAll`, `TestContextCarrying`, `TestNilSafety`, `TestAllReturnsCopy`.

## Safe modification patterns

- New phase: record via the existing marker API; keep `All()` a copy; keep everything nil-safe — this package is imported on request paths that must never panic.
