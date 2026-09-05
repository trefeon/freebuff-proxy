# ADR-0013 — REQUEST_TIMEOUT guards TTFB only, never the streamed body

Status: Accepted

Context: `REQUEST_TIMEOUT` (default 15m) was applied as a deadline on the chat request context. The deadline fired mid-stream and killed healthy long streams — a turn-heavy agent session died at exactly the bound with the connection still feeding data (observed live 2026-09-06).

Decision: the knob now bounds only the wait for response headers, via the transport's `ResponseHeaderTimeout` (`upstream/client.go`). No deadline is attached to the chat request context; the streamed body runs until upstream EOF or the caller's context cancels (client disconnect).

Reasoning: a single timeout cannot serve two masters. Header arrival is a liveness signal (upstream accepted the request); body duration is workload (reasoning + tool loops). Conflating them turns every long task into a timeout bug. Control calls keep their own tight ctx deadlines (`SessionCallTimeout`), which stay tighter than the header bound.

Alternatives considered: raising the default (moves the cliff, keeps the cliff); per-request header timer with body re-wrap (more moving parts than a transport knob for identical semantics).

Consequences: `REQUEST_TIMEOUT` semantics changed — long streams are never cut by it. A stalled-header upstream still aborts at the bound. A headers-arrived-but-stalled-forever upstream hangs until client disconnect (accepted trade-off; the client always controls termination).

Invariants: body lifetime belongs to the caller context; TTFB belongs to the transport; control-call deadlines unchanged.

Affected packages: `internal/upstream` (client.go, chat.go), `internal/config` (keycatalog description).

Related tests: `TestChatStreamSurvivesPastRequestTimeout`, `TestChatTTFBTimeoutStillAborts`.
