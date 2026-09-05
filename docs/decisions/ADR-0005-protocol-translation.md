# ADR-0005 — Protocol translation across three API surfaces

Status: Accepted

Context:
Clients speak OpenAI (`/v1/chat/completions`, `/v1/responses`) or Anthropic
(`/v1/messages`) shapes; upstream speaks its own CLI wire protocol. Every
request must be normalized to the internal representation, translated to the
upstream envelope, and every response translated back, in both streaming and
non-streaming modes, without corrupting tool calls or violating either
protocol's framing.

Decision:
- Translation is a pure pipeline in `internal/convert`: request normalization
  (parameter whitelist, `developer`→`system` roles, legacy `functions`→`tools`,
  tool-schema normalization via `schemacache.go`, `end_turn` injection),
  streaming XML tool-call extraction (`streamxml.go`), SSE encode/sanitize
  (`sse.go`), non-streaming accumulator (`accumulator.go`), reasoning effort
  (`effort.go`). No I/O, no environment reads.
- `internal/upstream` owns envelope injection (codebuff metadata, forced
  stream, `x-freebuff-*` headers) and the wire client.
- `internal/server` owns transport: three handlers, three relay families
  (stream + JSON per surface), shared helpers for lease lifecycle and error
  envelopes. Anthropic errors use the `{"type":"error","error":{...}}` envelope;
  OpenAI errors the standard `error` object; both flow through one writer that
  picks the envelope by request type.
- `end_turn` is injected upstream and MUST be stripped from every client-visible
  stream and response (both relays call `StripEndTurnToolCalls`).
- Anthropic streaming is strictly sequential per content block:
  `content_block_start` → `content_block_delta` → `content_block_stop`; blocks
  never interleave. Thinking blocks carry `"signature": ""` for strict schema
  compliance.
- OpenAI streaming emits `chat.completion.chunk` deltas with sequential
  synthetic tool-call indices; `[DONE]` terminates. `finish_reason`/usage
  handling follows the strict OpenAPI 3.1 shape (`refusal: null`, `logprobs: null`).
- Unsupported params are answered with explicit `400`s, not silent drops
  (documented-ignore exceptions: Anthropic `top_k`, Responses `include`/`truncation`).

Reasoning:
Keeping conversion pure and transport-free makes it unit-testable without
servers and keeps the wire protocol's quirks in one place. One error writer
with a request-type switch guarantees the two envelope formats cannot drift.
Strict sequential SSE block ordering is required by the Anthropic protocol;
relaxing it corrupts client state machines.

Alternatives considered:
- Per-surface conversion in the handlers: rejected; tripled logic, drift between surfaces.
- Streaming passthrough of upstream events: rejected; upstream dialect is not client-compatible.

Consequences:
- `convert` is the most test-heavy package; its import boundary is pinned by `layer_reviewfix_test.go`.
- New client parameters require whitelist + translation + 400-or-ignore decisions + tests per surface.
- Harness tool-name mapping (`toolmap.go`) is bidirectional and pinned against the harness corpus.

Invariants:
- `end_turn` never reaches a client; never duplicated upstream.
- Anthropic block lifecycle ordering never regresses.
- The three surfaces share the internal representation; translation bugs are parity bugs.

Affected packages:
`internal/convert`, `internal/upstream`, `internal/server` (chat.go, anthropic.go, responses.go, openai_stream.go, responses_stream.go, anthropic_errors.go, errors.go).

Related tests:
`convert_req_test.go`, `convert_schema_test.go`, `convert_stream_test.go`, `convert_xml_test.go`, `stream_completeness_test.go`, `server_api_test.go`, `replay_anthropic_test.go`, `conformance_pi_omp_test.go`, E2E suite in `backend/cmd/freebuff-proxy`.