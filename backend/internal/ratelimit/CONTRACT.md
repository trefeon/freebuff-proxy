# Package Contract: `backend/internal/ratelimit`

Task-local contract for agents modifying this package. Load before editing any file here.

## Purpose

Per-IP token-bucket rate limiter for the client-facing surface
(`RATE_LIMIT_PER_IP`, `RATE_LIMIT_BURST`; `0` disables; burst defaults to
2x the rate). Protects the gateway from a single abusive client; it is NOT
## Public API (stable surface)

- `Limiter`, `New(rate, burst, maxEntries)`, `Allow(key) (allowed,
  retryAfter)`, `Rate()`/`SetRate` for live knob updates.

## Allowed dependencies

None (stdlib only; archtest leaf). Wired by `server`.

## Forbidden dependencies

Everything internal. The limiter knows IPs and budgets, never tokens or models.

## Critical invariants

- IP isolation: one client's bucket never affects another; eviction of idle
  entries is bounded (never an unbounded map keyed by attacker input).
- `0` disables cleanly; partial configuration (rate without burst) falls back
  to documented defaults, never to deny-all.
- Limiter decisions are local and fast (in-memory); exceeding the limit fails
  the request without touching the pool.

## Tests that protect it

`limiter_test.go` (bucket, IP isolation, eviction, concurrency under `-race`).

## Safe modification patterns

- New limiting dimension (e.g. per-key): keep it in this package with the same
  bounded-table discipline; wire the knob through config, never ad hoc flags.