package server

// Anthropic streaming translation: relayAnthropicStream converts the upstream
// chat SSE stream into Anthropic message events (message_start,
// content_block_start/delta, message_delta, message_stop) through the
// sequential content_block lifecycle state machine (thinking/text/tool_use
// blocks), plus the shared JSON/usage helpers the Anthropic relays use.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"freebuff-proxy/internal/convert"
	"freebuff-proxy/internal/phasetiming"
)

// --- streaming translation ---

// anthropicToolState is one tool_use block being assembled from upstream
// tool-call fragments.
type anthropicToolState struct {
	index       int
	id          string
	name        string
	started     bool
	blockClosed bool
}

type anthropicStreamState struct {
	messageID          string
	model              string
	inputTokens        int
	thinkingStarted    bool
	thinkingIndex      int
	thinkingClosed     bool
	textStarted        bool
	textIndex          int
	textClosed         bool
	nextBlockIdx       int
	toolCalls          map[int]*anthropicToolState
	endTurnCallIndexes map[int]bool
	finishReason       string
	sawToolCall        bool
	usage              map[string]any
	// toolMap restores client tool names (#140 P2a): the request renamed
	// mapped tools to official signature names upstream, so tool_use blocks
	// must open with the CLIENT's dispatch name.
	toolMap convert.ToolMapper

	thinkingParts []string
	textParts     []string
	toolIDs       []string
	toolIDsSeen   map[string]bool
}

// relayAnthropicStream translates the upstream chat SSE stream into
// Anthropic message events: message_start, content_block_start (thinking/
// text/tool_use), thinking_delta, text_delta, input_json_delta,
// signature_delta, content_block_stop, message_delta, message_stop.
func (s *Server) relayAnthropicStream(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time, requestedModel string, inputTokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.logger.Warn("response writer does not support flushing")
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connecting\n\n")
	flusher.Flush()

	// Issue #164: the message_start event names the proxy's served model
	// (lease.Model, fallbacks included), not the raw requested id â€” clients
	// must see what actually served the request. Falls back to the requested
	// model when the relay is driven without a lease (direct unit tests).
	servedModel := stats.servedModel
	if servedModel == "" {
		servedModel = requestedModel
	}
	st := &anthropicStreamState{
		messageID:          "msg_" + randHexString(10),
		model:              servedModel,
		inputTokens:        inputTokens,
		toolCalls:          make(map[int]*anthropicToolState),
		endTurnCallIndexes: make(map[int]bool),
		finishReason:       "end_turn",
		toolMap:            stats.toolMap,
	}
	// Streaming XML tool-call extraction: MiMo/Hermes/Qwen/CodeBuff models
	// emit <tool_call>/<codebuff_tool_call>/<function_call> blocks inline in
	// delta.content; the extractor converts them to native tool-call
	// fragments the existing accumulateAnthropicChunk translates into
	// tool_use blocks. One instance per stream; xmlCallIndex keeps the
	// synthetic fragment indexes sequential so they cannot collide with
	// upstream indexes.
	xmlExtractor := &convert.XMLToolCallExtractor{}
	xmlCallIndex := 0
	send := func(ev map[string]any) {
		b, _ := json.Marshal(ev)
		_, _ = io.WriteString(w, "event: "+stringValue(ev["type"])+"\n")
		_, _ = w.Write(convert.EncodeSSE(b))
		flusher.Flush()
	}
	sendAnthropicMessageStart(send, st)

	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()
	lines := make(chan lineChunk)
	go relayReadLoop(ctx, r, lines)
	// lastWrite tracks the last frame actually written to the CLIENT; the
	// keepalive condition keys on it so a liveness signal is emitted after
	// any client-write silence, regardless of upstream comment/junk dribble
	// (those are dropped and never relayed â€” #161).
	lastWrite := time.Now()
	first := true

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if time.Since(lastWrite) >= keepaliveInterval {
				_, _ = io.WriteString(w, "event: ping\ndata: {\"type\": \"ping\"}\n\n")
				lastWrite = time.Now()
				flusher.Flush()
			}
		case lc := <-lines:
			if lc.err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("anthropic upstream stream error", "err", lc.err)
					s.flushAnthropicXMLToolCalls(send, st, xmlExtractor, &xmlCallIndex)
					send(map[string]any{
						"type": "error",
						"error": map[string]any{
							"type":    "api_error",
							"message": "upstream stream error: " + lc.err.Error(),
						},
					})
				}
				return
			}
			if lc.done {
				s.flushAnthropicXMLToolCalls(send, st, xmlExtractor, &xmlCallIndex)
				s.finalizeAnthropicStream(send, st)
				return
			}
			clean, drop := convert.SanitizeChunk(lc.line)
			if drop {
				// Dropped upstream lines are never relayed and must not
				// advance the keepalive timer (client sees only real
				// frames â€” #161).
				continue
			}
			var chunk map[string]any
			if err := json.Unmarshal(clean, &chunk); err != nil {
				continue
			}
			if first {
				first = false
				phasetiming.FromContext(ctx).Since(phasetiming.UpstreamTTFBMS, chatStart)
			}
			lastWrite = time.Now()
			stats.chunks++
			stats.bytes += len(clean)
			if m, _ := chunk["model"].(string); m != "" {
				st.model = m
			}
			if usage, ok := chunk["usage"]; ok && usage != nil {
				if um, ok := usage.(map[string]any); ok {
					st.usage = openAIUsageToAnthropic(um)
					stats.usageTokens = usageTotalTokens(um) // #122 spend ledger
				}
			}
			// Rewrite XML tool calls out of content before translation.
			feedAnthropicXMLToolCalls(xmlExtractor, chunk, &xmlCallIndex)
			s.accumulateAnthropicChunk(send, st, chunk)
		}
	}
}

// sendAnthropicMessageStart emits the message_start event.
func sendAnthropicMessageStart(send func(map[string]any), st *anthropicStreamState) {
	inTokens := st.inputTokens
	send(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            st.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         st.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": inTokens, "output_tokens": 0},
		},
	})
}

func (st *anthropicStreamState) closeOpenToolCalls(send func(map[string]any)) {
	for _, ts := range st.toolCalls {
		if ts.started && !ts.blockClosed {
			send(map[string]any{"type": "content_block_stop", "index": ts.index})
			ts.blockClosed = true
		}
	}
}

// accumulateAnthropicChunk translates one upstream chat chunk into
// Anthropic content events.
func (s *Server) accumulateAnthropicChunk(send func(map[string]any), st *anthropicStreamState, chunk map[string]any) {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, ok := choices[0].(map[string]any)
	if !ok || choice == nil {
		return
	}
	if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
		st.setStopReason(fr)
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return
	}
	// Reasoning deltas â†’ thinking block.
	if reasoning, ok := firstStringOf(delta, "reasoning_content", "reasoning", "reasoning_text", "thinking"); ok && reasoning != "" {
		st.thinkingParts = append(st.thinkingParts, reasoning)
		st.ensureThinking(send)
		send(map[string]any{
			"type":  "content_block_delta",
			"index": st.thinkingIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
		})
	}
	// Content deltas â†’ text block (closes an open thinking block).
	if content, ok := delta["content"].(string); ok && content != "" {
		st.textParts = append(st.textParts, content)
		st.ensureText(send)
		send(map[string]any{
			"type":  "content_block_delta",
			"index": st.textIndex,
			"delta": map[string]any{"type": "text_delta", "text": content},
		})
	}
	// Tool-call fragments â†’ tool_use blocks.
	if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
		// Sequential block lifecycle: an open thinking block must be closed
		// before a tool_use content_block_start fires â€” leaving it open until
		// finalize would straddle the tool_use block (both calls idempotent).
		st.closeThinking(send)
		st.closeText(send)
		for _, raw := range tcs {
			tc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			upIdx := 0
			if i, ok := numFloat64(tc["index"]); ok {
				upIdx = int(i)
			}
			fn, _ := tc["function"].(map[string]any)
			name := ""
			if fn != nil {
				name, _ = fn["name"].(string)
			}
			// Restore client tool names (#140 P2a): the request renamed
			// mapped client tools to official signature names upstream; a
			// tool_use block must open with the CLIENT's dispatch name.
			if name != "" {
				name = st.toolMap.RestoreName(name)
			}
			if name == "end_turn" {
				st.endTurnCallIndexes[upIdx] = true
				continue
			}
			if st.endTurnCallIndexes[upIdx] {
				continue
			}
			st.sawToolCall = true
			ts := st.toolState(upIdx)
			if id, ok := tc["id"].(string); ok && id != "" {
				ts.id = id
				if !st.toolIDsSeen[id] {
					if st.toolIDsSeen == nil {
						st.toolIDsSeen = make(map[string]bool)
					}
					st.toolIDsSeen[id] = true
					st.toolIDs = append(st.toolIDs, id)
					if sID := sanitizeToolID(id); sID != id && !st.toolIDsSeen[sID] {
						st.toolIDsSeen[sID] = true
						st.toolIDs = append(st.toolIDs, sID)
					}
				}
			}
			if name != "" && ts.name == "" {
				ts.name = name
				st.ensureStarted(ts, send)
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				if ts.name != "" {
					st.ensureStarted(ts, send)
					send(map[string]any{
						"type":  "content_block_delta",
						"index": ts.index,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
					})
				}
			}
		}
	}
}

// finalizeAnthropicStream closes every open content block and emits
// message_delta + message_stop.
func (s *Server) finalizeAnthropicStream(send func(map[string]any), st *anthropicStreamState) {
	st.closeThinking(send)
	st.closeText(send)
	indexes := make([]int, 0, len(st.toolCalls))
	for i := range st.toolCalls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		ts := st.toolCalls[i]
		if !ts.started || ts.blockClosed {
			continue
		}
		send(map[string]any{"type": "content_block_stop", "index": ts.index})
		ts.blockClosed = true
	}
	// stop_reason parity with the non-streaming path
	// (anthropicMessageFromCompletion): "end_turn" is promoted to "tool_use"
	// only when real tool fragments were relayed, and a "tool_use" with zero
	// relayed tool blocks (end_turn-only turn) demotes to "end_turn". Any
	// other recorded reason — max_tokens from a truncated stream,
	// content_filter, etc. — is preserved exactly: a max_tokens-truncated
	// stream with partial tool fragments must NOT read as a complete
	// tool_use turn (issue #170).
	stopReason := st.finishReason
	if stopReason == "end_turn" && st.sawToolCall {
		stopReason = "tool_use"
	} else if !st.sawToolCall && stopReason == "tool_use" {
		// finish_reason "tool_calls" whose fragments were ALL stripped
		// (end_turn-only turn) must not emit a tool_use stop_reason with zero
		// tool_use blocks (mirror of anthropicMessageFromCompletion).
		stopReason = "end_turn"
	}
	usagePayload := map[string]any{"output_tokens": 0}
	if st.usage != nil {
		if outToks, ok := intOf(st.usage["output_tokens"]); ok {
			usagePayload["output_tokens"] = outToks
		}
		if cr, ok := intOf(st.usage["cache_read_input_tokens"]); ok && cr > 0 {
			usagePayload["cache_read_input_tokens"] = cr
		}
		if cc, ok := intOf(st.usage["cache_creation_input_tokens"]); ok && cc > 0 {
			usagePayload["cache_creation_input_tokens"] = cc
		}
	}
	send(map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": usagePayload,
	})
	send(map[string]any{"type": "message_stop"})
	if s.reasoningCache != nil && len(st.thinkingParts) > 0 {
		thinking := strings.Join(st.thinkingParts, "")
		if thinking != "" {
			content := strings.Join(st.textParts, "")
			s.reasoningCache.Put(st.toolIDs, content, "", thinking, "", st.model)
		}
	}
}

// ensureThinking opens the thinking content block on first reasoning delta.
// Reasoning arriving AFTER a text block opened (which closed the thinking
// block via ensureText) reopens a FRESH thinking block at a new index â€”
// mirror of ensureText's reopen pattern. Emitting a delta against the closed
// index would violate the sequential block lifecycle.
func (st *anthropicStreamState) ensureThinking(send func(map[string]any)) {
	if st.thinkingStarted && !st.thinkingClosed {
		return
	}
	// Mirror ensureText: close any open text or tool_use block before the
	// thinking block opens (both calls idempotent). The previous code only
	// closed the text block on the REOPEN path and never closed open
	// tool_use blocks, so a thinking start could straddle an open block and
	// violate the sequential block lifecycle (issue #171).
	st.closeText(send)
	st.closeOpenToolCalls(send)
	st.thinkingIndex = st.nextBlockIdx
	st.nextBlockIdx++
	st.thinkingStarted = true
	st.thinkingClosed = false
	send(map[string]any{
		"type":  "content_block_start",
		"index": st.thinkingIndex,
		"content_block": map[string]any{
			"type":      "thinking",
			"thinking":  "",
			"signature": "",
		},
	})
}

// closeThinking closes the thinking block with a signature_delta (empty
// signature â€” the upstream never emits signatures).
func (st *anthropicStreamState) closeThinking(send func(map[string]any)) {
	if !st.thinkingStarted || st.thinkingClosed {
		return
	}
	send(map[string]any{
		"type":  "content_block_delta",
		"index": st.thinkingIndex,
		"delta": map[string]any{"type": "signature_delta", "signature": ""},
	})
	send(map[string]any{"type": "content_block_stop", "index": st.thinkingIndex})
	st.thinkingClosed = true
}

// ensureText opens the text content block on first text delta (closing any
// open thinking block first).
func (st *anthropicStreamState) ensureText(send func(map[string]any)) {
	if st.textStarted {
		return
	}
	st.closeThinking(send)
	st.closeOpenToolCalls(send)
	st.textIndex = st.nextBlockIdx
	st.nextBlockIdx++
	st.textStarted = true
	st.textClosed = false // a reopened text block needs its own stop frame
	send(map[string]any{
		"type":  "content_block_start",
		"index": st.textIndex,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
}

// closeText closes the text block before tool blocks open. The textStarted
// flag is cleared so a LATER text delta reopens the block at a fresh index
// (review P2 â€” some GLM/DeepSeek outputs interleave trailing text after
// tool-call fragments; keeping textStarted set would silently drop it).
func (st *anthropicStreamState) closeText(send func(map[string]any)) {
	if !st.textStarted || st.textClosed {
		return
	}
	send(map[string]any{"type": "content_block_stop", "index": st.textIndex})
	st.textClosed = true
	st.textStarted = false
}

// toolState returns (creating on first use) the tool block for an upstream
// tool index.
func (st *anthropicStreamState) toolState(upIdx int) *anthropicToolState {
	if ts, ok := st.toolCalls[upIdx]; ok {
		return ts
	}
	ts := &anthropicToolState{index: st.nextBlockIdx}
	st.nextBlockIdx++
	st.toolCalls[upIdx] = ts
	return ts
}

// ensureStarted emits the tool_use content_block_start once the name is
// known. A block closed by an interleaved text/thinking block (or finalize)
// REOPENS on a later fragment for the same upstream index: the block is
// reset and assigned a FRESH index — the closed index must never receive a
// second start, and the reopened block carries the accumulated id/name
// (issue #171). Sequential block lifecycle preserved.
func (st *anthropicStreamState) ensureStarted(ts *anthropicToolState, send func(map[string]any)) {
	if ts.started && !ts.blockClosed {
		return
	}
	if ts.started && ts.blockClosed {
		ts.index = st.nextBlockIdx
		st.nextBlockIdx++
		ts.started = false
		ts.blockClosed = false
	}
	ts.started = true
	send(map[string]any{
		"type":  "content_block_start",
		"index": ts.index,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    sanitizeToolID(ts.id),
			"name":  ts.name,
			"input": map[string]any{},
		},
	})
}

// setStopReason maps an OpenAI finish reason onto the Anthropic vocabulary.
func (st *anthropicStreamState) setStopReason(reason string) {
	switch reason {
	case "tool_calls", "function_call":
		// sawToolCall is deliberately NOT set here: it must reflect actual
		// relayed tool fragments (accumulateAnthropicChunk), not the terminal
		// chunk's claim â€” an end_turn-only stream whose finish_reason is
		// "tool_calls" must still finalize as "end_turn".
		st.finishReason = "tool_use"
	case "stop", "":
		st.finishReason = "end_turn"
	case "length":
		st.finishReason = "max_tokens"
	default:
		st.finishReason = reason
	}
}

// openAIUsageToAnthropic maps a chat usage object onto the Anthropic usage
// shape (input/output tokens plus cache_read_input_tokens).
func openAIUsageToAnthropic(usage map[string]any) map[string]any {
	out := map[string]any{}
	promptTokens, hasPrompt := intOf(usage["prompt_tokens"])
	completionTokens, hasCompletion := intOf(usage["completion_tokens"])
	cacheRead, hasCacheRead := intOf(usage["prompt_cache_hit_tokens"])
	if !hasCacheRead {
		if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			cacheRead, hasCacheRead = intOf(details["cached_tokens"])
		}
	}
	if hasPrompt {
		input := promptTokens
		if hasCacheRead && cacheRead > 0 && promptTokens >= cacheRead {
			input = promptTokens - cacheRead
		}
		out["input_tokens"] = input
	}
	if hasCacheRead && cacheRead > 0 {
		out["cache_read_input_tokens"] = cacheRead
	}
	if hasCompletion {
		out["output_tokens"] = completionTokens
	}
	if len(out) == 0 {
		out["output_tokens"] = 0
	}
	return out
}

// firstStringOf returns the first non-empty string value among keys.
func firstStringOf(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// intOf extracts an int64 from a JSON-decoded number value.
func intOf(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case int:
		return int64(n), true
	}
	return 0, false
}

// stringValue returns the string value of a JSON field ("" when absent).
func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
