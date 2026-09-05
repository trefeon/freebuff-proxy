# Package Contract: `backend/internal/stealth`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Leaf package: TLS fingerprinting (uTLS) and HTTP header sanitization so upstream sees a browser-class client. Profiles model Chrome/Safari ClientHello shapes; `upstream` applies them and rotates on transient retry.

## Public API (stable surface)

- Profiles: `Profile`, `ProfileID`, `DefaultProfile()` (Chrome126), `Lookup(name)`, `GetProfileForConnection(p)`, `RandomUserAgent()`.
- Headers: `SanitizeHeaders(h)`, `ApplyProfileHeaders(h, p)`.
- Dialer: `Dialer(profile, baseDial, insecureSkipVerify, alpn)` — uTLS ClientHello, ALPN negotiation, Safari custom spec.
- `SetLogger(l)`.

## Allowed dependencies

None. Zero internal imports — enforced by `archtest`.

## Forbidden dependencies

Everything internal, especially `upstream` (upstream consumes profiles; stealth must not know the wire) and `config` (profiles are selected by callers, never read from env here).

## Critical invariants

- The dialer must never mutate the shared spec (`TestDialerDoesNotMutateSharedSpec`) — concurrent requests share profiles.
- Profile swap must change the ClientHello (`TestDialerProfileSwapChangesClientHello`); ALPN negotiation must hold (`TestDialerALPNNegotiation`).
- Sanitization is a fixed 25-header policy (`TestSanitizeHeaders`); client-hint randomness must stay plausible (`TestProfileRandomClientHints`).

## Tests that protect it

`TestLookup`, `TestSanitizeHeaders`, `TestApplyProfileHeaders`, `TestDialerTLS`, `TestDefaultProfile`, `TestGetProfileForConnection`, `TestProfileRandomClientHints`, `TestDialerSafariCustomSpec`, `TestDialerDoesNotMutateSharedSpec`, `TestSetALPN`, `TestDialerInvalidAddr`, `TestProfileResolutionEdgeCases`, `TestDialerProfileSwapChangesClientHello`, `TestDialerALPNNegotiation`, `TestProfileSelectionLogs`.

## Safe modification patterns

- New fingerprint: add a `Profile` + `Lookup` case + dialer test asserting the ClientHello bytes changed; never weaken the shared-spec immutability test.
- Header policy change: update `TestSanitizeHeaders` in the same commit — a dropped header is wire-visible upstream.
