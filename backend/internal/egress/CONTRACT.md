# Package Contract: `backend/internal/egress`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Leaf package: egress-path diagnostics. Probes the outbound path (trace parse) and caches the result with a TTL so `-doctor` and the dashboard can report which region/ASN the gateway exits from without probing on every request.

## Public API (stable surface)

- `Probe(ctx, dialer, timeout) Result` (parses the trace body; tolerates missing fields, error statuses, dial errors, timeouts).
- `DirectDialer(timeout)`, `Path`, `Cache`, `NewCache()` / `NewCacheWithTTL(ttl)` (`DefaultTTL`).

## Allowed dependencies

None. Zero internal imports — enforced by `archtest`.

## Forbidden dependencies

Everything internal, especially `upstream` (probe results feed diagnostics; the wire client never depends on them) and `config` (dialer and timeout are caller-supplied).

## Critical invariants

- Probe is read-only diagnostics: it must never claim sessions, mutate state, or affect routing — failures return a `Result`, never an error that callers must handle as fatal.
- Cache is TTL-bounded (`TestCache`); a stale egress fact is worse than none for region-gated tiers.
- Trace parsing tolerates missing fields (`TestProbeMissingTraceFields`) — upstream trace shape is not a contract.

## Tests that protect it

`TestProbeParsesTrace`, `TestProbeMissingTraceFields`, `TestProbeErrorStatus`, `TestProbeDialError`, `TestProbeTimeout`, `TestProbeAll`, `TestCache`, `TestProbeBodyAndCancelEdges`.

## Safe modification patterns

- New trace field: parse tolerantly (absent = zero value, never an error) + a `MissingTraceFields`-style case.
- TTL change: justify against probe cost vs region-change frequency; the cache exists to keep diagnostics off the hot path.
