# Package Contract: `backend/internal/updatecheck`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Leaf package: self-update version checks against the GitHub repo. Fetches the latest release, compares semver, caches the answer with a TTL, and fails open — an unreachable update endpoint must never disturb a running gateway.

## Public API (stable surface)

- `Checker`, `New(repo, client)`; `UpdateAvailable(current, latest) bool`, `CompareVersions(a, b) int`; `Latest` fetch with cache.

## Allowed dependencies

None. Zero internal imports — enforced by `archtest`.

## Forbidden dependencies

Everything internal, especially `config` (repo/client are caller-supplied) and `notify` (callers decide what to do with the answer).

## Critical invariants

- Fail-open: fetch failure returns the previous answer (`TestLatestFetchFailureReturnsprev`); first-fetch failure backs off for the TTL (`TestLatestFirstFetchFailureBacksOffForTTL`) — no hot-loop hammering of the release endpoint.
- Cached (`TestLatestFetchesAndCaches`); decisions are logged (`TestLatestLogsDecision`).
- Version comparison is total and correct (`TestCompareVersions`, `TestUpdateAvailable`) — a wrong "update available" either spams operators or hides security fixes.

## Tests that protect it

`TestCompareVersions`, `TestUpdateAvailable`, `TestLatestFetchesAndCaches`, `TestLatestFetchFailureReturnsprev`, `TestLatestFirstFetchFailureBacksOffForTTL`, `TestLatestLogsDecision`.

## Safe modification patterns

- Endpoint change: keep the TTL cache + backoff; a check that runs per-request is a self-inflicted rate limit.
- Compare-rule change: extend `TestCompareVersions` first (pre-release/build-metadata edges), implementation second.
