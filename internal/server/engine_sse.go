package server

// Plain SSE plumbing every relay shares: relayReadLoop (line pump), lineChunk
// (frame or terminal send), keepaliveInterval (liveness cadence), plus the
// relayStats/usageTotalTokens accounting used by all wire relays.

import (
	"bufio"
	"context"
	"io"
	"time"

	"freebuff-proxy/internal/convert"
)

// relayStats accumulates per-response relay counters for logging.
type relayStats struct {
	chunks int
	bytes  int
	// usageTokens is the upstream usage total of the completed chat (the
	// final usage block), fed to the pool spend ledger once per successful
	// completion (#122). 0 when the stream carried no usage.
	usageTokens int64
	// servedModel is the model this lease's session/run is actually bound to
	// (lease.Model, resolved fallbacks included). Set by chatCore before the
	// relay runs so wire relays can stamp it onto the response body's model
	// field and message_start events (issue #164). Empty when a relay is
	// driven directly by a test with no lease.
	servedModel string
	// toolMap restores client tool names on the response paths (issue #140
	// P2a): chatCore renames mapped client tools to official signature names
	// on the request, so every relay must rename them BACK before writing.
	// Zero value = identity.
	toolMap convert.ToolMapper
}

// usageTotalTokens extracts the token total from a chat usage object
// (total_tokens, falling back to prompt+completion). Returns 0 when absent.
// Feeds the per-token spend ledger (#122).
func usageTotalTokens(usage any) int64 {
	u, ok := usage.(map[string]any)
	if !ok || u == nil {
		return 0
	}
	if total, ok := intOf(u["total_tokens"]); ok && total > 0 {
		return total
	}
	prompt, _ := intOf(u["prompt_tokens"])
	completion, _ := intOf(u["completion_tokens"])
	return prompt + completion
}

// keepaliveInterval is how long the relay may go without writing a frame
// to the client before it emits an SSE keepalive frame (comment on the
// OpenAI-compatible wires, event: ping on the Anthropic wire) to hold the
// connection open. Long upstream reasoning pauses produce no chunks, and
// proxies/clients may treat silence as a dead connection. The timer keys
// on client writes only: dropped upstream comment/junk lines never count
// as liveness (#161). A var (not const) so tests can shrink it.
var keepaliveInterval = 15 * time.Second

// lineChunk is one upstream SSE line or the terminal send. done is set only
// on the clean-EOF send (a real empty line also arrives as line==nil, so the
// terminal state must be explicit, not inferred from a nil slice).
type lineChunk struct {
	line []byte
	err  error
	done bool
}

// relayReadLoop drains r line by line onto ch, stopping when the stream
// ends or ctx is canceled. The final send carries done (clean EOF) or the
// terminal read error; on cancellation the goroutine exits without sending
// (the request context cancellation closes the upstream body read, so Scan
// returns promptly).
func relayReadLoop(ctx context.Context, r io.Reader, ch chan<- lineChunk) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLine)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		select {
		case ch <- lineChunk{line: line}:
		case <-ctx.Done():
			return
		}
	}
	var terminal lineChunk
	if err := scanner.Err(); err != nil {
		terminal = lineChunk{err: err}
	} else {
		terminal = lineChunk{done: true}
	}
	select {
	case ch <- terminal:
	case <-ctx.Done():
	}
}
