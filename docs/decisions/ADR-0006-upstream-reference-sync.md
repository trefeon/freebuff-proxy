# ADR-0006 — Upstream reference synchronization

Status: Accepted

Context:
The proxy reimplements the official FreeBuff CLI's wire protocol and session
lifecycle. Upstream (CodebuffAI/freebuff) churns constants frequently:
availability windows, session caps, agent maps, wire shapes. Any drift between
the proxy's assumptions and upstream reality produces wrong wire behavior or
bans. The reference is a first-class dependency, but the repo must never
blindly "update constants" without knowing whether a value is an upstream fact,
local policy, compatibility shim, or fallback.

Decision:
- A gitignored local clone (`reference/freebuff`, synced by
  `scripts/sync-upstream.sh` / `.cmd` / `.ps1`) is the source of upstream truth.
- Five upstream constant files are pinned into committed testdata
  (`backend/internal/registry/testdata/upstream/`); `sync-upstream.sh` refreshes
  pins, verifies hash parity, and runs the test suite (`--test-all`).
- `check-upstream.sh` is a read-only drift check; `review-wire-drift.sh`
  classifies wire-shape drift as COMMENT-ONLY vs FUNCTIONAL.
- CI (`upstream-drift.yml`, every 6h) detects drift, auto-opens a sync PR for
  registry pin drift, opens a "needs port" issue for functional wire drift, and
  publishes drift JSON into the dashboard embed. Sync PRs never auto-merge.
- Registry parity test (`catalog_test.go`) pins the upstream snapshot; the
  `pinnedFallbackAgents`/`pinnedFallbackRootByModel` maps in `registry.go`
  (plus `retiredRootOverrides` in `parse.go` for retired roots) must be
  updated to match when pins change.
- The upstream SHA used for any decision that encodes reference facts into code
  is recorded in the commit message.

Reasoning:
Upstream is a moving target with real account-level consequences for getting it
wrong. Pinning + parity tests + classified drift detection gives the repo an
explicit, reviewable upgrade path instead of silent staleness. The
"facts vs policy" split is what keeps AI agents from "fixing" local policy
(e.g. fallback maps, effort ladders) when upstream facts change, and vice versa.

Alternatives considered:
- Live-only registry refresh: rejected; offline fallback must exist for cold start and test determinism.
- Auto-merge sync PRs: rejected; porting wire drift needs human judgment.

Consequences:
- Stale reference data produces wrong wire behavior; never trust an unsynced tree.
- New harness dialects or upstream tools may require extending `toolmap.go` and its coverage corpus.
- Drift classification is a human-reviewed queue, not an automatic merge.

Invariants:
- Committed pins == upstream snapshot at recorded SHA (parity test fails on drift).
- Local policy (fallbacks, aliases, effort ladders, allowlists) lives in code, clearly separated from pinned upstream facts.
- Wire-drift porting requires a test, not just a constant swap.

Affected packages:
`scripts/`, `backend/internal/registry` (+ testdata), `backend/internal/modelcat` (facts consumers), `backend/internal/convert` (toolmap).

Related tests:
`registry_test.go`, `catalog_test.go` (parity), `toolmap_coverage_test.go`, `retired_root_test.go`; CI `upstream-drift.yml`.