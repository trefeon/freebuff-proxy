# ADR-0022 — Quota auto-probe scheduler

Date: 2026-09-07. Status: Accepted.

## Context

The Tokens header Probe-all button was removed (ADR-0021): quota data must
stay fresh without a manual bulk probe. Per-token quota `reset_at` is known
from cached probe results.

## Decision

A scheduler on the existing pool maintain tick (30s, no new goroutine)
probes each pooled token once per Pacific day at a deterministic jittered
time inside the 2h window before its known reset:

- Slot = hash(token-index + Pacific date) spread over [reset-2h, reset).
- In-memory lastProbeDay per token; a restart may double-probe once, and
  the jitter spreads restarts so the herd never moves together.
- Probe path only: session-less `ProbeToken`, warn-only failures, results
  flow through the existing `UpdateQuotaFromProbe` snapshots.
- Tokens with unknown reset (never probed) are skipped — the first probe
  stays manual (per-token Probe button, Quota Tracker probe-all).
- Kill-switch: `QUOTA_AUTO_PROBE` (bool, default true, live-apply,
  GroupPool). False restores exact pre-scheduler behavior.

## Consequences

One session-less upstream GET per token per day, staggered — negligible
ban surface versus the bulk button it replaces. No DB persistence for the
day map (accepted imprecision, documented here).
