# Package Contract: `backend/internal/logring`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Bounded in-memory `slog` ring backing `/admin/logs`: retains the newest entries first, forwards everything to the wrapped sink, and exposes counts for the dashboard. The gateway's log tap without disk or network.

## Public API (stable surface)

- `Entry`, `Ring`, `Handler`; `NewHandler(next slog.Handler, capacity int)`.
- Read/aggregate surface used by the dashboard handler (`Recent`, `Counts` and friends — see handler tests).

## Allowed dependencies

`telemetry` — exactly the set pinned by `archtest`.

## Forbidden dependencies

Everything else, especially `server` and `dashboard` (they consume the ring; the ring must not know its readers) and `config` (capacity is caller-supplied).

## Critical invariants

- Bounded newest-first eviction with capacity clamp (`TestRingRetainsNewestFirst`, `TestCapacityClamp`) — unbounded growth in a log tap is a memory leak by design.
- `WithAttrs`/`WithGroup` forward to the wrapped sink with group prefixes folded (`TestRingForwardsToNext`, `TestWithAttrsWithGroupFold`, `TestRingFlattenGroupKeys`) — the dashboard view and the real log stream must agree.
- Handler errors still retain (`TestHandleErrorStillRetains`); `Recent` with negative bounds is guarded (`TestRecentNegativeGuard`).
- Concurrency-safe (`TestCountsConcurrent`, `TestRingSubHandlersShareStore`) — writers span all request goroutines.

## Tests that protect it

`TestRingRetainsNewestFirst`, `TestRingSubHandlersShareStore`, `TestRingForwardsToNext`, `TestRingFlattenGroupKeys`, `TestWithAttrsWithGroupFold`, `TestEnabledGating`, `TestCapacityClamp`, `TestFormatAttrKinds`, `TestFormatAttrQuotesControlChars`, `TestRecentNegativeGuard`, `TestHandleErrorStillRetains`, `TestCounts`, `TestCountsConcurrent`, `TestRingEmptyGroupInlined`.

## Safe modification patterns

- Capacity change: keep the clamp; the dashboard polls this ring — bigger rings cost per-request memory on every log call.
- New attr formatting: extend `TestFormatAttrKinds`-style cases; control characters and quotes must stay escaped (log injection via crafted upstream bodies).
- Never block in `Handle`: slow sinks stall every request logger — forward first, retain second.
