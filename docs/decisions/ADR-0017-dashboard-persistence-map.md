# ADR-0017 — Dashboard persistence map (env vs db vs live vs memory)

Status: Accepted

Context: the dashboard must survive a crash with operators seeing continuity:
config intact, token quota showing last-known values marked stale, history
intact, and no surprise behavior change (e.g. a locked token serving again).
Four stores exist today with blurry ownership: `.env` (config), the
session-state JSON (per-session snapshots), `freebuff-history.db` (ADR-0016
logs/quota/maturity/requests), and process memory (locks, cooldowns,
quarantines, metrics samples, admin sessions).

Decision: fixed ownership, no new store.

- env (`.env`, atomic write + reload): operator config only. Never runtime
  state, never secrets beyond the credentials the operator pasted.
- sqlite (`freebuff-history.db`, ADR-0016): append-only history
  (log_entries 7d, quota_snapshots / maturity_events 30d, request_records).
  Never current state, never secrets.
- session-state JSON: last-known per-session snapshots (quota maps, promo,
  referral, countdowns) restored with the stale mark until live contact.
  Never locks, cooldowns, or quarantines.
- live (upstream, zero-cost GET probe): the only fresh truth for quota and
  account state. Probe-all refreshes snapshots without admission.
- memory (explicitly ephemeral): token locks, tokenhealth cooldowns,
  quarantine markers, metrics samples, admin HMAC key. A restart clears
  them by design: locks/cooldowns/quarantines re-derive from the next
  upstream contact instead of persisting a possibly-wrong yesterday
  (a ban that lifted overnight must not stay a lock forever).

Reasoning: persisting locks/quarantines would trade one failure mode
(a crash clears a lock) for a worse one (a stale lock/quarantine survives
a fix and silently sidelines a healthy token with no operator action to
clear it but the same UI that set it). The dashboard already surfaces
re-derivation honestly (stale marks, last-seen timestamps, probe-all).

Alternatives considered: persist locks/quarantines in sqlite (rejected:
stale-state risk above; revisit if an operator reports a crash-loop
re-admitting a banned token); persist metrics samples (rejected: rebuilt
in minutes from live traffic); server-side admin sessions (rejected:
stateless HMAC cookie means re-login, which is also the secure default).

Consequences: crash behavior is now a contract — config and history
survive, snapshots restore stale, locks/cooldowns/sessions re-derive.
Any change moving a memory item into sqlite needs an ADR superseding
this one with the staleness story addressed.

Invariants: secrets never land in sqlite or session JSON (redaction
applies); retention stays off the request path; corrupt/missing files
degrade to live-only, never a failed boot.

Affected packages: none (documents `internal/config`, `internal/session`,
`internal/store`, `internal/pool`, `internal/dashboard` as-is).

Related tests: existing restore/stale/probe tests pin the map
(TestHistorySinkConfigEvents, quota rollover, probe-all e2e).
