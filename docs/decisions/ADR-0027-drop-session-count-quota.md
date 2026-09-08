# ADR-0027 — Drop session-count quota mirroring (incl. premium pool)

Date: 2026-09-08. Status: Accepted (user-ordered).

## Context

Upstream moved to a pure Freebucks meter: every model priced per hour of
session, charged once when the session starts. Session-count quotas
(weekly/monthly caps) are retired upstream. The proxy still mirrors a
session-count world: quota counters, the shared 5/day premium session
pool (`PremiumSessionLimit`), and count copy across docs/dashboard.
Keeping the mirror doubles dev friction: every upstream pricing move
needs a parallel count-semantics port.

## Decision

- Remove session-COUNT quota: counters, caps, the premium session pool
  gate, and count copy in docs/dashboard/frontend.
- Keep the session ENVELOPE: admission, relay, runs, persistence, and
  the wire session types stay; they are the billing unit upstream
  charges per hour.
- Upstream still enforces its own pools server-side; unmirrored
  refusals surface as honest upstream errors, not local fallback.
- Parity tests pinning the pool are updated to the new behavior in
  the same slices (deliberate divergence, recorded here).

## Consequences

- Premium rows (Luna, 1.2) admit like unmetered rows until upstream
  refuses; no local 5/day gate.
- Quota Tracker shows Freebucks windows only, no session counts.
- Future upstream pool changes need no proxy port.
