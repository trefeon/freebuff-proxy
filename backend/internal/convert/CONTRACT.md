# Package Contract: `backend/internal/convert`

Task-local contract for agents modifying this package. Load before editing any file here.

## Purpose

Pure OpenAI request/response normalization for the bridge. Performs NO I/O: every function is a pure transformation over JSON decoded from a request body or SSE frame. Covers request sanitization (parameter whitelist, developer→system role rewrite, tool-schema normalization, `end_turn` injection), SSE frame encoding + per-chunk stream sanitization, the non-streaming response accumulator with XML tool-call extraction, reasoning-effort extraction, and tool-name mapping.

## Public API (stable surface)

- Request normalization: `NormalizeRequest(body, modelOverride)`, `NormalizeRequestOpts`, `NormalizeRequestMapped(Opts)` → `([]byte, ToolMapper, error)`.
- Tools: `NewToolMapper(body) ToolMapper` (+ `MsgCount`/`ToolCount`, request rename / response restore); `ToolCallDeltaFragment(index, tc)`.
- SSE: `EncodeSSE(data)`, `DONE`, `ErrorChunk(msg, code)`, `SanitizeChunk(line) ([]byte, bool)` + `SanitizeChunkOpts`, `StripEndTurnToolCalls(chunk) (remaining bool, finishReason string)`.
- Accumulator: `NewAccumulator()`, `NewAccumulatorOpts(Options)`, `Accumulator.Add/…/Finish()`; `XMLToolCallExtractor` (Feed/Flush).
- Effort: `ExtractReasoningEffort(payload)`; `InjectCacheControl(messages)`.
- Options: `Options`, `DefaultOptions()`.

## Allowed dependencies

`modelcat`, `reasoningcache` (via `Options.ReasoningLookup`), `tokenestimate` — exactly the set pinned by `layer_reviewfix_test.go` (`TestConvertDoesNotImportPackageBoundary` scans non-test files).

## Forbidden dependencies

EVERYTHING else — `config`, `pool`, `upstream`, `server`, `registry`, `dashboard`, `session`, `runs`, `notify`, `stealth`, `telemetry`, `logring`, `cmd/*`. The import-boundary test fails the build on any new internal import. Envelope injection (`codebuff_metadata`, forced stream, `x-freebuff-*` headers) deliberately lives in `backend/internal/upstream`.

## Critical invariants

- Pure: the `*Opts` variants never read the process environment, touch the filesystem, or make network calls; the only external inputs are passed in explicitly (`Options`, a `ReasoningLookupFn`, an Accumulator's per-call config). `DefaultOptions` is a deprecated compatibility shim that still reads `COMPRESS_PROMPT` / `CACHE_CONTROL_INJECTION` / `REASONING_IN_CONTENT` from the environment until callers pass config-derived options (issue #277).
- `end_turn` injection: the proxy injects the `end_turn` tool definition to pass Codebuff's `foreign_toolset` validation (schemacache.go). Downstream parity: it MUST be stripped before emitting to clients (`StripEndTurnToolCalls`); both the OpenAI and Anthropic relays call it. An existing `end_turn` must never be duplicated.
- Schema normalization: `$ref` inlining with cycle guard, depth cap (12 levels), per-request node budget, enum cross-type dedupe; the schema cache returns maps that callers must not mutate (mutation-safety pinned by test).
- Stream sanitization: whitelist keys, drop empty-choice chunks, drop enrichment keys (`service_tier`, `obfuscation`, `moderation`), correct `finish_reason`/`[DONE]` handling; canonical chunks take a fast path.
- XML extractor: pooled buffers, bounded partial-hold (repeated `<` cannot re-hold forever), blocks close on `</tool_call>`; extraction of `end_turn`-shaped blocks must never surface to clients.
- Effort ladders delegate to `modelcat` (never a local copy).

## Tests that protect it

`layer_reviewfix_test.go` (import boundary — do not break), `convert_req_test.go` (whitelist, roles, effort clamp, compression, think-tag extraction), `convert_schema_test.go` (end_turn injection, schema cache, refs, budgets), `convert_stream_test.go` (SSE encode/sanitize, accumulator), `convert_xml_test.go` + `stream_completeness_test.go` (XML dialects, partial openers), `schemacache_foreign_test.go` (signature-tool pin), `effort_modelcat_test.go` (parity with modelcat), `reasoning_restore_test.go`, `toolmap_test.go` + `toolmap_coverage_test.go` (bidirectional harness mapping), `accumulator_bench_test.go` (allocation floors, pool race-freedom).

## Safe modification patterns

- New knobs: extend `Options` and wire from `config` — do NOT add new env reads on hot paths (`DefaultOptions` is on the deprecation path).
- New internal import → update `layer_reviewfix_test.go` allowlist deliberately (a review event, not a routine edit).
- Tool-map changes: keep the bidirectional mapping (client name ↔ official name); the coverage test pins the harness signature corpus — regenerate/extend it with any new harness dialect.
- Performance-sensitive edits: run `accumulator_bench_test.go` benchmarks; the sanitize fast path must stay allocation-light.