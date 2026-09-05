# Package Contract: `backend/internal/clicreds`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Leaf package: discovers a locally stored CLI credential (Manicode/Freebuff/Codebuff token files) for `-test-token`, `-doctor`, and first-run flows. Filesystem reads only, no network, no config.

## Public API (stable surface)

- `DiscoverToken() (token, email, path string, ok bool)` — precedence: Manicode profile > Freebuff profile > Codebuff fallback > session-token fallback; strips BOM.

## Allowed dependencies

None. Zero internal imports — enforced by `archtest`.

## Forbidden dependencies

Everything internal. Credential discovery must not depend on `config` (it feeds config, not the reverse) or `upstream` (no validation here — discovery only).

## Critical invariants

- Read-only: never writes, migrates, or deletes credential files.
- Never logs the token value; return it only to the caller.
- Precedence order is pinned by tests — reordering changes which account `-test-token` probes.

## Tests that protect it

`TestDiscoverTokenManicode`, `TestDiscoverTokenCodebuffFallback`, `TestDiscoverTokenPrefersManicode`, `TestDiscoverTokenPrefersFreebuff`, `TestDiscoverTokenFreebuffProfilePrecedence`, `TestDiscoverTokenSessionTokenFallback`, `TestDiscoverTokenEmpty`, `TestDiscoverTokenStripsBOM`.

## Safe modification patterns

- New credential location: add a probe in precedence order + a `TestDiscoverToken*` case; keep BOM/empty-file tolerance.
- Never add network validation here — that belongs to `upstream` (probe) or `pool` (`ProbeNewToken`).
