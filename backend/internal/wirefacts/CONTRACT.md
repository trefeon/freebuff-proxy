# Package Contract: `backend/internal/wirefacts`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Verbatim upstream wire-shape snapshots plus the fail-explicit generator that
turns them into Go: `testdata/wire/` holds the snapshots and `snapshots.json`
(manifest: upstream SHA, npm wrapper version, per-file sha256). The snapshots
are never transformed, so `scripts/review-wire-drift.sh` keeps reading the
same bytes. `cmd/wiregen` (Go only — see Toolchain below) emits five
generated files from them; every other package consumes the output and never
parses TypeScript itself.

## Public API (stable surface)

- Facts table: `Run(upstreamSHA, wireDir, registryDir string, out io.Writer) error`
  → `wirefacts_gen.go` (`UpstreamSHA`, `VendorVersion`, `WireFiles`, `RegistryPins`).
- Catalog: `EmitCatalog(upstreamSHA, registryDir string, out io.Writer) error`
  → `backend/internal/modelcat/catalog_gen.go`.
- Wire vocabulary + notice copy: `EmitWire(upstreamSHA, wireDir string, wireOut, noticesOut io.Writer) error`
  → `backend/internal/upstream/wirecodes_gen.go` + `notices_gen.go`.
- Tool names: `EmitTools(upstreamSHA, wireDir, registryDir string, out io.Writer) error`
  → `backend/internal/convert/toolnames_gen.go`.
- Lexical gate: `ScanTS(name string, src []byte, commit string) error` rejects
  any top-level TypeScript construct outside `knownOpeners`, naming
  file:line, the construct, and the upstream commit.
- Regenerate: `go generate ./backend/internal/wirefacts/` (the `go:generate`
  line pins the current `-upstream` SHA; keep it equal to the manifest SHA).

## Allowed dependencies

None. Stdlib only — enforced by `archtest` (leaf package).

## Forbidden dependencies

Everything internal. The emitters read snapshot/registry-pin bytes off disk;
no package may import `wirefacts` for runtime facts — consumers use their own
generated `_gen.go` file (this keeps `dashboard`/`convert` out of the import
graph; see `archtest`).

## Toolchain

The generator is `go run ./backend/cmd/wiregen` — Go only. Bun is at most a
generate-time toolchain exception, never a runtime dependency; the current
design uses no Bun at any stage (no `bun` invocation in the scripts, the
workflow, or the generator).

## Critical invariants

- Never hand-edit any `_gen.go` file: `TestGeneratedUpToDate`,
  `TestCatalogGenUpToDate`, `TestEmitWireUpToDate`, and
  `TestEmitToolsUpToDate` regenerate in memory and require byte equality.
  The CI `codegen-parity` job (`upstream-drift.yml`) enforces the same.
- `-upstream` must equal the manifest `upstream_sha`; anything else is a hard
  failure before any snapshot is read (`TestUpstreamFlagMismatch`).
- Failing runs emit nothing: every emitter buffers first and leaves the
  previous generated file untouched; unknown constructs fail with file:line
  + commit (`TestUnknownConstructFailsExplicit`).
- Snapshots stay verbatim: `TestGeneratedPinsManifest` requires every table
  entry to resolve to a real file with a matching hash.
- `scripts/check-upstream.sh` with a full-SHA ref refuses to run when the
  manifest SHA diverges from the checked SHA (re-pin + regenerate first);
  branch refs stay in floating drift-detection mode.

## Tests that protect it

`TestGeneratedUpToDate`, `TestGeneratedPinsManifest`,
`TestUnknownConstructFailsExplicit`, `TestUpstreamFlagMismatch`,
`TestCatalogGenUpToDate` (`emit_catalog_test.go`),
`TestEmitWireUpToDate` (`emit_wire_test.go`),
`TestEmitToolsUpToDate` (`emit_tools_test.go`).

## Safe modification patterns

- Re-pin (TYPE C work, never mixed with features): sync reference first,
  update snapshots + manifest, bump the `go:generate` SHA, regenerate all
  five files, commit them together — the parity tests fail until all agree.
- New upstream construct: teach the emitter one construct at a time and
  extend `knownOpeners`/verification alongside it; a silent skip is a bug —
  reject explicitly.
- New generated file: add the emitter + `cmd/wiregen` flag + parity test +
  CI `git diff` path in the same change, or the new file rots on the first
  re-pin.
