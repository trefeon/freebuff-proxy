# Agentic workflow

How multi-agent work runs in this repo. Distilled from 2026-09-08 session
verdicts and lessons; each practice names the pain it answers.

## 1. One slice = one lane + explicit file ownership

Pain: parallel subagents on one tree clobbered each other (uncommitted
dashboard edits lost).
Practice: every fan-out slice gets its own `git worktree` (or OMP/orca lane)
plus a declared file set; message siblings via `hub` before touching shared
files (`server`, contract surfaces); the coordinator integrates serially.
Never build a deploy from a dirty checkout — use the acerblue `/tmp` worktree
pattern (`AGENTS.md` §4).

## 2. Classify drift BEFORE refreshing baselines

Pain: refreshing the baseline first makes `review-wire-drift.sh` report
all-SAME against the new anchors and hides FUNCTIONAL rows; CRLF checkouts
fake drift in LF-normalized `snapshots.json`.
Order: `sync-upstream.sh --check` (read-only) → `review-wire-drift.sh`
classify → refresh pins/baseline → `check-upstream.sh` (canonical parity proof)
→ full tests + dist rebuild. LF-normalize before comparing hashes. Merge drift
PRs serially wire → registry → dashboard with green CI between each. Vendor pin:
2e57674fc; fresh-vendor check 2026-09-08 found zero functional drift.

## 3. Visual gate on every dashboard slice

Pain: e2e-green agents shipped truncation, overflow, and unequal boxes; a
mock-harness review (52 screenshots) found 9 gaps e2e missed.
Gate: mock-harness screenshots at 1600 / 1280 / 390 viewport widths, reviewed
with own eyes before merge; per-page locator discipline (never blanket-replace
locators across specs); `vite build` before screenshots (e2e serves committed
`dist` via serve-static, so `src` edits are invisible until built); rebuild +
commit `dist` LAST (dist-freshness CI check).

## 4. Secret hygiene as a hard gate

Pain: a pooled test key and the dashboard password once leaked into a chat
transcript (rotation required).
Gate: never take passwords/keys into subagent transcripts; test live with
user-provided keys only, then rotate; never commit secrets/`reference/`/devdocs;
pre-push scan per `public-repo-publish-hygiene` (gitleaks + trufflehog); GitHub
push protection stays on. Agents must NOT self-scan by reading credential files.

## 5. Merge / CI resilience

Pain: transient `gh merge` 502s, BEHIND blocks, wall-clock flakes, AV blocks.
Protocol: 502 on merge usually started a background merge — poll state before
retrying; BEHIND `mergeStateStatus` blocks squash — `gh pr update-branch`
(no `--merge` flag here) + re-green, merges run serially; a lone FAIL with
greens around it is note-and-move-on after 2 reruns, and any pool failure is
first reproduced on pristine `main`; `archtest.test.exe` access-denied is the
Windows AV block — hand-verify via import grep, CI Linux is proof.
Triage CI blocker-first (build > lint > types > tests) and never claim green
without a fresh run.

## Skill table

Have locally — use, do not install:

| Need | Skill | Location |
|---|---|---|
| Lanes | using-git-worktrees / worktrees / orca-workspace-protocol | `~/.agents/skills/` / opencode / managed |
| Fan-out | dispatching-parallel-agents / subagent-driven-development / executing-plans | `~/.agents/skills/` |
| Evidence | verification-before-completion / pr-status-triage | `~/.agents/skills/` |
| Invariants/TDD | tdd / test-driven-development / golang-testing / golang-security | `~/.agents/skills/` |
| Debug | systematic-debugging / diagnosing-bugs | `~/.agents/skills/` |
| E2E/visual | webapp-testing / web-design-reviewer / agent-browser | `~/.agents/skills/` |
| Frontend | frontend-design / svelte5-best-practices / svelte-code-writer | `~/.agents/skills/` |
| Review | code-review / quality-code-review / receiving-code-review | `~/.agents/skills/` |
| Secrets | public-repo-publish-hygiene (managed) / security-review / git-guardrails-claude-code | managed / opencode / `~/.agents/skills/` |
| Merge | resolving-merge-conflicts / finishing-a-development-branch / git-workflow | `~/.agents/skills/` |
| Docs | update-docs / find-docs / adr-skill / write-guide | `~/.agents/skills/` |

Install (into `~/.agents/skills/`, never the repo; audit SKILL.md first):

| Need | Skill | Install |
|---|---|---|
| Runner-side visual regression (`toHaveScreenshot`, masking, baselines) | testdino-hq/playwright-skill | `npx skills add testdino-hq/playwright-skill` |
| Secret scanning CLI | GitGuardian/agent-skills (ggshield) | `npx skills add GitGuardian/agent-skills` |
| Contract tests (`can-i-deploy`) | contract-test-validator | `npx skills add jeremylongshore/claude-code-plugins-plus-skills --skill contract-test-validator --agent claude-code` |

Deliberately not installed: upstream-drift (project scripts ARE the skill);
docs-as-code (covered by update-docs/find-docs/adr-skill above).
