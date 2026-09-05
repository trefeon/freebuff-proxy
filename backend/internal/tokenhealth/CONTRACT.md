# Package Contract: `backend/internal/tokenhealth`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Leaf package: token/account health reporting. Maps session statuses to health states, classifies login-email risk (disposable/shared domains), and formats masked health reports for the dashboard and `-doctor`. Reporting only — it changes no token state.

## Public API (stable surface)

- `TokenHealth`, `TokenHealthState`, `EmailRiskKind`.
- `SessionStateFromStatus(status) (TokenHealthState, string)`.
- `ClassifyEmailDomain(email) EmailRiskKind`, `MaskEmail(email)`, `FlagSharedMailboxes(rows)`, `FormatHealthReport(rows)`, `RiskHint(k)`, `JoinHint(parts...)`.

## Allowed dependencies

None. Zero internal imports — enforced by `archtest`.

## Forbidden dependencies

Everything internal, especially `pool` and `upstream` (health is computed from data handed in, never fetched; fetching would couple reporting to the wire and to locks).

## Critical invariants

- Emails are always masked in output (`TestMaskEmail`) — full addresses never reach logs, dashboard payloads, or reports.
- Disposable-domain list keeps parity with its source (`TestDisposableEmailDomainsParity`) — update the list, not the test, when the source changes.
- `SessionStateFromStatus` is total over known statuses; unknown statuses map to a neutral state, never to healthy.

## Tests that protect it

`TestDisposableEmailDomainsParity`, `TestClassifyEmailDomain`, `TestMaskEmail`, `TestFlagSharedMailboxes`, `TestFormatHealthReport`.

## Safe modification patterns

- New risk signal: add an `EmailRiskKind` + `RiskHint` text + classify case; keep `FormatHealthReport` output stable for dashboard parsers.
- Status mapping change: mirror the upstream session-status vocabulary (see `upstream` classify); unknown → neutral, never healthy.
