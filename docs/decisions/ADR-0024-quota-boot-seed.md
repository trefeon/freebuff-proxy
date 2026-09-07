# ADR-0024 — Quota view boot seeding + first-probe recovery

Date: 2026-09-07. Status: Accepted.

## Context

Quota Tracker renders live pool memory only. Two restart holes (found live
2026-09-07, acerblue: 132 persisted `quota_snapshots` rows, page empty,
zero probe log lines in 90m):

1. Nothing seeds the live quota view from the DB on boot. Snapshots feed
   sparklines/history only, so persisted data never reaches the cards.
2. ADR-0022 skips unknown-reset tokens until a manual probe. After a
   restart EVERY token has unknown reset (memory wiped), so the scheduler
   waits for exactly the manual probe it was built to replace.

## Decision

- Boot seeding: CLI loads the latest snapshot per (token_idx, model)
  from the store and pushes it into the pool through a new
  pool-side setter (CLI orchestrates; pool never imports store).
  Seeded rows carry their probed timestamp, so the view shows
  last-known values immediately and the scheduler learns reset_at
  from the seed.
- First-probe recovery: a token that is unknown-reset AND has no DB
  snapshot (truly never probed) gets one staggered boot probe —
  slot spread by token hash over the first 5m after boot, session-less
  ProbeToken path, warn-only. One GET per virgin token per boot.
- No new knob: this repairs the restart half of ADR-0022, it does not
  add a feature. `QUOTA_AUTO_PROBE=false` disables the boot probe too
  (seed display still applies: showing persisted data is never probing).

## Consequences

Restart shows last-known quotas instantly; virgin tokens self-heal
within minutes without Probe-all. Worst case added upstream load:
one session-less GET per unprobed token per boot.
