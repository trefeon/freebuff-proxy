# ADR-0028 — Freebucks-meter truth in user docs

Date: 2026-09-08. Status: Accepted (user-ordered).

## Status

Accepted (user-ordered). Companion to ADR-0027 (which drops the
session-count quota mirror in code/dashboard); this ADR records the docs
side of the same shift.

## Issue

User docs still described quota in session counts — 1 session/day, 5/5,
+1 session per referral, bonus/reward sessions, daily session slots,
premium-pool membership as quota — while the upstream reference
(`reference/freebuff @2e57674fc`) meters pooled users. Two meters
coexist upstream and the wire prices map is the sole cost source: the
picker derives everything from the session response
(`cli/src/utils/freebucks.ts`: "`prices` is a map of model id to price,
and the balances come with it"), with row intent typed per
`common/src/types/freebuff-session.ts`. Count language contradicts the
meter: sessions are 1-hour billable rows ("priced in Freebucks per hour
of session, charged once when the session starts. Your daily Freebucks
refill at midnight Pacific; the wallet keeps what you buy or earn."
— `FREEBUCKS_PICKER_NOTICE`), DELETE refunds, and there are "no more
weekly or monthly session caps."

## Decision

- Rewrite every session-count quota claim in user docs to meter truth:
  priced rows read 'N Freebucks/hr' off the wire prices map; unpriced
  rows are unmetered.
- Sessions remain the consumption unit (1-hour rows with the admission /
  poll / DELETE lifecycle); only the COUNT-as-quota language is retired.
- Referral GLM rows are gated: gated accounts earn Freebucks via the
  Earn page; no `+1 session per referral` promise on metered accounts.
- Streak rows count down to the 7-day milestone with a completion line;
  no bonus-session gift promise.
- Quoted upstream notices stay verbatim, each with one trailing
  annotation line marking the legacy wording; user quotes in
  `rotation-lab-plan.md` stay verbatim with only our own framing
  adjusted.
- `docs/decisions/ADR-0004*` and `ADR-0021*` stay untouched as history.

## Consequences

- Tier docs, model tables, and the Quota Tracker speak prices and pool
  refills only; future upstream pricing moves need no count-semantics
  port.
- Upstream refusals outside the mirrored meter surface as honest
  upstream errors, not local fallback.
- Follow-up slices must keep new docs on the meter; any reintroduced
  session-count quota language contradicts this ADR.
