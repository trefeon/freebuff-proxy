# ADR-0023 — Burst load-balance (opt-in)

Date: 2026-09-07. Status: Accepted.

## Context

Drain is the safest default (ADR history, anti-ban freeze): one account is
exhausted before rotating, mimicking single-user behavior. But a model
hammered as a subagent engine hits one account's throttle brutally while
sibling accounts with the same model sit idle. The fixed strategy radio
offers no middle ground.

## Decision

An opt-in burst path, default OFF (unset keys = byte-identical selection):

- `BURST_BALANCE_ENABLED` bool default false, live-apply, GroupPool.
- `BURST_WINDOW` duration default 1m; `BURST_THRESHOLD` int default 20
  same-model admissions per window; `BURST_MAX_TOKENS` int default 2
  (validated >= 2).
- Pool keeps an in-memory per-model sliding window (pruned on the maintain
  tick). While a model's count exceeds threshold, selection for THAT model
  switches to least_used among healthy tokens — same-model admission on
  another account — capped at BURST_MAX_TOKENS distinct tokens per burst.
- Entry/exit WARN-logged per model. Non-burst models and disabled state
  follow the configured strategy untouched.

## Consequences

Operators with brutal subagent bursts can spread load without abandoning
drain for steady traffic. The default path is provably unchanged
(pre-existing acquire tests pass unmodified). Burst spreading is visibly
less single-user-like: the catalog marks it as an anti-ban tradeoff, and
the Tokens card carries the same caution.
