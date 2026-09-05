# Package Contract: `backend/internal/archtest`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Executable architecture: pins the backend package dependency matrix. Scans every non-test Go file under `backend/` and fails on any internal import not in the `allowed` table; a second test pins the advisor-flagged forbidden edges with readable reasons. Test-only package — no runtime code.

## Public API (stable surface)

None exported for production. Tests: `TestBackendDependencyMatrix`, `TestKnownForbiddenEdges`.

## Allowed dependencies

Test-only stdlib (`go/parser`, `go/token`, `os`, `path/filepath`, `strings`, `testing`). Zero internal imports by construction (it only parses source text, never imports the packages it guards).

## Forbidden dependencies

Everything internal. Importing a guarded package would couple the guard to the guarded and could mask the very edge it pins.

## Critical invariants

- Matrix == reality: `allowed` must list every legitimate internal edge. A red matrix means either a layering violation (fix the code) or a deliberate new edge (extend the matrix with a reason — an architecture decision, not a routine edit).
- Unknown packages fail: any new `internal/*` dir without a matrix entry is red. `cmd/*` entrypoints are exempt (thin dispatch; `server`/`cli` already bound the stack below them); `testdata` dirs are skipped.
- `TestKnownForbiddenEdges` mirrors the matrix for the dangerous edges (`convert→server`, `pool→dashboard`, `modelcat→upstream`, `registry→server`, `upstream→session`, `session→pool`, `dashboard→server`) — keep both in sync.
- Source of truth for `docs/architecture/DEPENDENCIES.md`, which documents the same matrix in prose.

## Tests that protect it

Self-guarding: the two tests ARE the contract. Breakage signal is binary (green/red), diagnosis is the failing edge named in the error.

## Safe modification patterns

- New internal import anywhere in `backend/`: add the edge to `allowed` with a comment why, confirm no cycle is introduced, and check the matching per-package `CONTRACT.md` still describes reality.
- New package: add a matrix entry (leaves get `{}`); update `docs/architecture/DEPENDENCIES.md` in the same change.
- Never exempt a package from the scan to make it green — that defeats the guard.
