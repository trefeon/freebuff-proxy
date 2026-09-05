# Upstream Compatibility

The proxy reimplements the official FreeBuff CLI's wire protocol. Upstream
churns; this file separates what is pinned from what is policy, and documents
the sync workflow. (Decision rationale: ADR-0006. Procedure detail lives in
`scripts/README.md`.)

## Two kinds of truth

| Upstream-derived facts (sync owns) | Repository-owned behavior (code review owns) |
|---|---|
| Pinned constants in `backend/internal/registry/testdata/upstream/` | `fallbackAgents`/`fallbackRootByModel` mapping policy |
| Session/chat envelope shapes | Translation behavior (`convert`) |
| Error wire codes (`wirecodes.go`) | Classification → token-state transitions (pool) |
| Agent maps, availability windows, session caps | Aliases, allowlists, fallback chains, effort ladders |
| CLI UA strings / fingerprint inputs | Which profile is default (SAFE_MODE presets) |

Never "update a constant" without knowing which column it sits in. Facts
change via sync; policy changes via reviewed commits with tests.

## Sync workflow

```bash
bash scripts/sync-upstream.sh --test-all   # refresh pins + hash parity + full suite
bash scripts/sync-upstream.sh --check      # drift-only check (no writes)
# Windows: .\scripts\sync-upstream.cmd  (-TestAll / -CheckOnly)
bash scripts/check-upstream.sh             # read-only hash parity
bash scripts/review-wire-drift.sh          # COMMENT-ONLY vs FUNCTIONAL classification
```

- Local reference clone: gitignored `reference/freebuff`. Always sync it
  before reference-driven work; never trust an unsynced tree.
- If pins change: update `fallbackAgents`/`fallbackRootByModel` in
  `backend/internal/registry/registry.go` to match (parity test fails on
  drift), re-run drift analysis, and record the upstream SHA in the commit.
- CI (`upstream-drift.yml`, every 6h): registry drift → auto-opened sync PR
  (never auto-merged; GITHUB_TOKEN PRs do not trigger CI — run checks
  manually); wire drift → "needs port" issue with classification;
  drift JSON published into the dashboard embed every run.

## Drift classification

`review-wire-drift.sh` labels wire-shape drift as:

- **COMMENT-ONLY**: refresh the wire baseline
  (`check-upstream.sh --update-wire-baseline`); no Go change.
- **FUNCTIONAL**: port the Go side (e.g. `injectEnvelope`, `classifyError`,
  `parseSessionResponse`) **plus a test**. Needs human judgment; queued as an
  issue, never auto-merged.

## What a port commit contains

1. The upstream SHA in the message.
2. Pin/testdata refresh if constants moved.
3. The Go port (envelope/classify/parse + mapping).
4. Tests at the layer that owns the behavior (wire test + pool/server mapping
   test where client-visible).
5. Updated parity fixtures; updated docs where the wire contract is quoted
   (INVARIANTS.md, API-ENDPOINTS.md, ADR-0012 when egress changes).