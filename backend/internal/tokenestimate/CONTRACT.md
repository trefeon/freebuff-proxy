# Package Contract: `backend/internal/tokenestimate`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Leaf package: local deterministic token estimation with the `o200k_base` BPE tokenizer plus the FreeBuff fudge factors. Serves Anthropic `count_tokens` and streaming `input_tokens` estimates without any upstream call.

## Public API (stable surface)

- `New() (*Estimator, error)`, `CountText(text) int`, `CountJSON(v any) int`, `Decode(ids []uint) (string, error)`, `CountAnthropicRequest(raw map[string]any) (int, error)` (per-message overhead, image-flat costs, tool key order).

## Allowed dependencies

None. Stdlib only — enforced by `archtest`.

## Forbidden dependencies

Everything internal. The estimator must stay a pure function of its inputs so `count_tokens` is deterministic and offline-safe.

## Critical invariants

- Goldens cross-validate against an independent Python tiktoken implementation (dev-only reference, not committed). A Go/Python mismatch means the Go estimator diverged: update the Go code, never the goldens, unless estimator constants deliberately changed (then regenerate both sides together).
- Deterministic and concurrency-safe (`TestDeterminism`, `TestConcurrentUse`) — the estimator runs on hot streaming paths.
- Tool key order and per-message overhead are part of the estimate; Anthropic shape changes need new golden cases.

## Tests that protect it

`TestCountTextGolden`, `TestCountTextSpecialMarkers`, `TestCountJSONGolden`, `TestCountAnthropicRequestGolden`, `TestCountAnthropicRequestPerMessageOverhead`, `TestCountAnthropicRequestImageFlat`, `TestCountAnthropicRequestSystemImageFlat`, `TestCountAnthropicRequestToolsGolden`, `TestCountAnthropicRequestToolKeyOrder`, `TestCountAnthropicRequestErrors`, `TestCountAnthropicRequestContentShapes`, `TestCountAnthropicRequestSystemShapes`, `TestCountAnthropicRequestNonObjectMessage`, `TestDeterminism`, `TestConcurrentUse`, `TestDecodeRoundTrip`.

## Safe modification patterns

- Fudge-factor change: justify against the Python reference first, update goldens in the same commit, keep `Decode` round-trip exact.
- New Anthropic content shape: add a `SystemShapes`/`ContentShapes` case before wiring callers — unhandled shapes must error, not silently estimate zero.
