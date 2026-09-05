# Safe Refactoring Policy

Refactors earn their place the same way tests do: by making a specific future
change safer. "Cleaner" is not a reason.

## Before

1. Identify the responsibility (package CONTRACT.md) and the relevant ADRs.
2. Search all callers/usages (`lsp` references for exported symbols; never
   guess).
3. Read the tests that protect the area; if none exist for the behavior you
   touch, write a characterization test FIRST (existing behavior → test →
   refactor → test → behavior preserved).
4. Name the invariant you must preserve (docs/architecture/INVARIANTS.md).
5. State why the refactor is needed: repeated logic, measurable coupling,
   dangerous change path, testing difficulty. If none applies, leave it alone.

## During

- Preserve external behavior exactly: same statuses, envelopes, headers,
  state transitions, defaults.
- One logical change per commit/PR. Never bundle a refactor with a feature,
  fix, or upstream sync.
- Keep the diff reviewable: renames and moves without behavior edits; edits
  without renames. Never both in one hunk.
- No "while we're here" refactors unless they are the task.
- Respect the dependency matrix (archtest is a build gate); a refactor that
  needs a new edge is an architecture decision, not cleanup.
- Struct-literal edits: `git diff | grep '^-[^-]'` — confirm every removed
  line has an intended re-add (dropped fields compile as zero values).

## After

- Targeted tests for the touched package, then the full hermetic suite
  (`task test`), then `task test:race` where concurrency is involved
  (Linux/CI; race does not run on Windows).
- `gofmt -l backend` empty; `go vet ./backend/...` clean.
- Diff review with the AI review checklist
  (docs/development/AI-CODE-REVIEW.md).
- Regenerate affected fixtures where catalogs changed
  (`FP_REGEN_FIXTURE=1` for the e2e config-meta fixture).
- Update the package CONTRACT.md and any affected ADR if responsibility moved.

## Areas that need coverage before refactoring

`server`, `pool`, `session`, `upstream`, `convert`, admin, config,
streaming: covered today (see docs/testing/TEST-STRATEGY.md). New uncovered
ground must get characterization tests first; the inventory's listed gaps
(frontend units, scripts, SOCKS dial path, fuzz seeds) are recorded in
docs/maintenance/TECH-DEBT.md with priorities.

## Prohibited patterns

- Mass renames, god-file splits without a coupling problem, new abstraction
  layers over two call sites, framework migrations, mass dependency upgrades,
  protocol-behavior changes smuggled inside cleanup, combining an upstream
  port with a refactor and a feature.