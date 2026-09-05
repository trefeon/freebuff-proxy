package server

// Shared stream-relay plumbing (issues #245/#246/#247/#248/#276): the XML
// tool-call feed/flush core used by all three relays, the end_turn
// pipeline, the common prologue/keepalive helpers and the shared
// non-streaming drain loop. Protocol-specific framing stays in each relay
// (openai_stream.go / anthropic_stream.go / responses_stream.go).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"freebuff-proxy/backend/internal/convert"
	"freebuff-proxy/backend/internal/phasetiming"
	"freebuff-proxy/backend/internal/reasoningcache"
)

// --- XML tool-call extraction: feed (map-level) ---

// feedXMLToolCalls feeds one unmarshalled chat chunk through the stream's
// XML tool-call extractor and rewrites it in place: withheld block text is
// removed from delta.content (the key is dropped when empty) and completed
// calls are appended as native tool_calls fragments with per-stream
// sequential indexes so they cannot collide with upstream indexes. Existing
// native tool_calls fragments are left untouched. Returns whether the chunk
// map was mutated (the caller decides whether to re-marshal) and whether
// any extracted fragment was appended (the OpenAI relay's xmlCallsSeen
// signal feeds its terminal finish_reason flip).
func feedXMLToolCalls(xmlExtractor *convert.XMLToolCallExtractor, chunk map[string]any, xmlCallIndex *int) (mutated, appended bool) {
	changed := false
	rawChoices, _ := chunk["choices"].([]any)
	for _, raw := range rawChoices {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		content, _ := delta["content"].(string)
		if content == "" {
			continue
		}
		text, calls := xmlExtractor.Feed(content)
		if text != content {
			if text == "" {
				delete(delta, "content")
			} else {
				delta["content"] = text
			}
			changed = true
		}
		if len(calls) == 0 {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		// Synthetic fragment indexes must never collide with the chunk's
		// native tool_calls indexes (parity across all three relays): raise
		// the per-stream counter past the max native index present.
		bumpXMLCallIndex(tcs, xmlCallIndex)
		grew := false
		for _, call := range calls {
			if call.Function.Name == "end_turn" {
				continue // strip-parity: never relay the proxy-injected pseudo-tool
			}
			tcs = append(tcs, convert.ToolCallDeltaFragment(*xmlCallIndex, call))
			*xmlCallIndex++
			grew = true
		}
		if grew {
			delta["tool_calls"] = tcs
			appended = true
		}
		changed = true
	}
	return changed, appended
}

// drainXMLToolCalls releases any still-open XML candidate block at stream
// end. It returns the trailing scrubbed text and the completed calls as
// native tool_calls fragments (end_turn stripped, sequential synthetic
// indexes continuing the stream's counter). No-op result (empty text, no
// fragments) when nothing was buffered. Callers frame the protocol chunk
// themselves (each relay accumulates through its own path).
func drainXMLToolCalls(xmlExtractor *convert.XMLToolCallExtractor, xmlCallIndex *int) (string, []any) {
	ft, fc := xmlExtractor.Flush()
	if ft == "" && len(fc) == 0 {
		return "", nil
	}
	frags := make([]any, 0, len(fc))
	for _, call := range fc {
		if call.Function.Name == "end_turn" {
			continue // strip-parity: never relay the proxy-injected pseudo-tool
		}
		frags = append(frags, convert.ToolCallDeltaFragment(*xmlCallIndex, call))
		*xmlCallIndex++
	}
	return ft, frags
}

// --- end_turn pseudo-tool pipeline (map-level) ---

// trackToolCallIndexesInChunk records per-stream tool-call state on an
// unmarshalled chunk BEFORE StripEndTurnToolCalls deletes the end_turn
// entries: end_turn indexes feed the downstream continuation-fragment drop
// (later argument fragments carry the same index but an empty name), and
// any real named call flips seenRealToolCalls so the terminal finish_reason
// rewrite never downgrades a genuine tool-call turn to stop. seenReal may
// be nil (the Responses relay gates on found instead).
func trackToolCallIndexesInChunk(chunk map[string]any, endTurnCallIndexes map[int]bool, seenRealToolCalls *bool, foundEndTurn *bool) {
	choices, _ := chunk["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, tc := range tcs {
			tcMap, _ := tc.(map[string]any)
			if tcMap == nil {
				continue
			}
			fn, _ := tcMap["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if name == "end_turn" {
				if i, ok := tcMap["index"].(float64); ok {
					endTurnCallIndexes[int(i)] = true
					if foundEndTurn != nil {
						*foundEndTurn = true
					}
				}
			} else if name != "" && seenRealToolCalls != nil {
				*seenRealToolCalls = true
			}
		}
	}
}

// processEndTurnCalls runs the end_turn pipeline on an unmarshalled chunk:
// track indexes before stripping, strip the pseudo-tool, drop continuation
// fragments for the stripped indexes. found reports whether an end_turn
// call was present; toolCallsRemaining mirrors StripEndTurnToolCalls after
// the continuation drop; emptied reports whether a choice's tool_calls list
// was emptied by the drop; mutated reports whether the chunk map was
// rewritten (strip or drop) so callers know to re-marshal. Both relays
// orbit this core so end_turn semantics change in one place (issue #246).
func processEndTurnCalls(chunk map[string]any, endTurnCallIndexes map[int]bool, seenRealToolCalls *bool, stripAlways bool) (found, toolCallsRemaining, emptied, mutated bool) {
	trackToolCallIndexesInChunk(chunk, endTurnCallIndexes, seenRealToolCalls, &found)
	// The Responses relay strips on every chunk (historical behavior — its
	// own foundEndTurn gate controls the extra flips); the OpenAI relay
	// only strips chunks that actually carried an end_turn (historically
	// gated on the "end_turn" substring), because StripEndTurnToolCalls
	// flips a tool_calls terminal to stop and a real-call stream must keep
	// its finish_reason. Apply the strip conditionally to preserve both.
	remaining := true
	if stripAlways || found {
		remaining, _ = convert.StripEndTurnToolCalls(chunk)
		mutated = true
	}
	dropped, emptied := dropEndTurnContinuationsInChunk(chunk, endTurnCallIndexes)
	if emptied {
		remaining = false
	}
	if dropped {
		mutated = true
	}
	return found, remaining, emptied, mutated
}

// --- shared stream-relay prologue (issue #247) ---

// newStreamRelay performs the prologue every streaming relay shares: SSE
// headers, the flusher check, WriteHeader(200), the ": connecting" grace
// flush, the keepalive ticker, the upstream line channel with its read
// loop, and the client-write clock. Only the keepalive frame text differs
// per protocol (maybeKeepalive carries it). ok is false when the response
// writer cannot flush; callers log the warning and return.
func newStreamRelay(ctx context.Context, w http.ResponseWriter, r io.Reader) (flusher http.Flusher, keepalive *time.Ticker, lines <-chan lineChunk, lastWrite *time.Time, ok bool) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Content-Type-Options", "nosniff")
	flusher, ok = w.(http.Flusher)
	if !ok {
		return nil, nil, nil, nil, false
	}
	w.WriteHeader(http.StatusOK)

	// The official CLI client treats a ": connecting" comment as the signal
	// that headers have flushed and the stream is live (grace flush): write
	// it before relaying anything so a client-side timeout can never fire
	// during a long upstream admission pause. Comment frames are ignored by
	// SSE parsers.
	_, _ = io.WriteString(w, ": connecting\n\n")
	flusher.Flush()

	keepalive = time.NewTicker(keepaliveInterval)
	// Buffered channel decouples upstream read-ahead from downstream
	// Write+Flush syscall latency: the scanner can keep filling while the
	// client socket drains, so a slow WAN client never stalls the upstream
	// producer and upstream burst never stalls on a single Flush. 64 is
	// enough for a full streaming window without blocking (freebuff bursts
	// ~70-120 deltas/s).
	ch := make(chan lineChunk, 64)
	go relayReadLoop(ctx, r, ch)
	now := time.Now()
	return flusher, keepalive, ch, &now, true
}

// streamErrorAttrs builds the structured attributes for a mid-stream death
// (upstream read failure after WriteHeader(200)): the downstream access
// line keeps status 200, so without req_id + relay progress (chunks/bytes/
// elapsed) the failure is uncorrelated noise. req_id rides the request
// context (chatCore stamps it); relays driven directly in tests carry none
// and omit the key.
func streamErrorAttrs(ctx context.Context, chatStart time.Time, stats *relayStats, err error) []any {
	attrs := []any{
		"err", err,
		"elapsed_ms", time.Since(chatStart).Milliseconds(),
		"chunks", stats.chunks,
		"bytes", stats.bytes,
	}
	if reqID := reqIDFrom(ctx); reqID != "" {
		attrs = append(attrs, "req_id", reqID)
	}
	return attrs
}

// maybeKeepalive writes the protocol's keepalive frame when the relay has
// been silent past keepaliveInterval, advancing the client-write clock. The
// three relays select on the same ticker; the frame text is the only
// difference ("event: ping" carries the SSE event name for the Anthropic /
// Responses surfaces, plain comments elsewhere). Returns true when written.
func maybeKeepalive(w http.ResponseWriter, flusher http.Flusher, lastWrite *time.Time, frame string) bool {
	if time.Since(*lastWrite) < keepaliveInterval {
		return false
	}
	_, _ = io.WriteString(w, frame)
	*lastWrite = time.Now()
	flusher.Flush()
	return true
}

// --- shared non-streaming drain (issue #248) ---

// errDrainUpstreamDecode marks an accumulator decode failure inside
// drainUpstream; relays build their own protocol envelope from it.
var errDrainUpstreamDecode = errors.New("upstream decode")

// errDrainUpstreamScan marks a scanner/stream error inside drainUpstream.
var errDrainUpstreamScan = errors.New("upstream stream")

// drainUpstream drains one upstream SSE stream into acc: scanner setup with
// the shared maxStreamLine buffer, the TTFB phase timing on the first line,
// acc.Add's per-chunk error surface, and the scanner.Err + ctx.Err guards.
// onLine, when given, runs on every raw line BEFORE it is fed to the
// accumulator (used by the OpenAI relay's native-tool-call probe). The
// caller only shapes the error response after a non-nil return.
func drainUpstream(ctx context.Context, r io.Reader, acc *convert.Accumulator, stats *relayStats, chatStart time.Time, onLine ...func([]byte)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLine)
	first := true
	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		if first {
			first = false
			phasetiming.FromContext(ctx).Since(phasetiming.UpstreamTTFBMS, chatStart)
		}
		line := scanner.Bytes()
		for _, hook := range onLine {
			hook(line)
		}
		if err := acc.Add(line); err != nil {
			return fmt.Errorf("%w: %v", errDrainUpstreamDecode, err)
		}
		stats.chunks++
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() == nil {
			return fmt.Errorf("%w: %v", errDrainUpstreamScan, err)
		}
		return nil
	}
	return nil
}

// --- canonical tool-call key builder (issue #276) ---

// buildCanonicalToolKey reduces per-index accumulated tool calls to the
// canonical identity key in upstream index order (the order the relayed
// fragments reconstruct the client's tool_calls array from). Calls whose id
// never arrived are skipped, matching the toolIDs list put alongside the
// key. extract returns the (id, name, args) triple for one accumulator
// entry; sanitize, when given, rewrites the id before keying (Anthropic
// relays sanitize upstream ids before emitting, so the key must match what
// clients echo).
func buildCanonicalToolKey[T any](calls map[int]T, extract func(T) (id, name, args string)) string {
	indexes := make([]int, 0, len(calls))
	for i := range calls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	triples := make([][3]string, 0, len(indexes))
	for _, i := range indexes {
		id, name, args := extract(calls[i])
		if id == "" {
			continue
		}
		triples = append(triples, [3]string{id, name, args})
	}
	return reasoningcache.CanonicalToolKey(triples)
}
