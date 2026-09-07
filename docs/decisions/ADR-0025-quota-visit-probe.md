# ADR-0025 — Quota auto-probe on page visit (hourly)

Date: 2026-09-07. Status: Accepted. Amends ADR-0022.

## Context

Quota probes are session-less upstream GETs: no session admission, no
chat relay, the same read the CLI performs on its own. The ADR-0022
daily/pre-reset gate was conservative design, not an upstream
requirement — and it left the page empty whenever the cache was cold.
Manual Probe-all has no throttle at all, so a bounded automatic probe
is strictly gentler than what the UI already allows.

## Decision

- Quota Tracker calls `POST /admin/tokens/test-all?auto=1` on page
  mount (after the first tokens load), then reloads the tokens store.
  Silent unless it fails: no success banner, the numbers just appear.
- Server keeps one pool-scoped last-bulk-probe timestamp (in-memory).
  `auto=1` probes only when the timestamp is older than 1h (or never);
  otherwise it returns the current view untouched. The manual button
  (no param) always forces and refreshes the timestamp.
- The timestamp covers all clients/tabs: ten open tabs still cause one
  upstream pass per hour, not ten.
- ADR-0022 daily scheduler and ADR-0024 boot seed stay as backstops.

## Consequences

Worst case upstream load: one session-less GET per pooled token per
hour while a human looks at the page. Cold page shows numbers within
seconds of opening, no button press.
