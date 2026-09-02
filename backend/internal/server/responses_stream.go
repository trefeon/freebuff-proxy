package server

// Responses streaming translation: relayResponsesStream converts the upstream
// chat SSE stream into Responses SSE events (response.created ->
// response.completed, output_item add/delta/done) with streaming XML tool-call
// handling, and relayResponsesJSON drains the non-streaming path into one
// completed Responses object.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/convert"
	"freebuff-proxy/backend/internal/phasetiming"
)

// responsesItem is one output item being assembled during stream relay:
// either a message (text), a reasoning item or a function_call.
type responsesItem struct {
	id          string
	kind        string // "message" | "reasoning" | "function_call"
	outputIndex int
	callID      string
	name        string
	text        string
	args        strings.Builder
	contentIdx  int
	started     bool
}

// responsesStreamState tracks the relayed output items.
type responsesStreamState struct {
	items       []*responsesItem
	nextIndex   int
	toolByUpIdx map[int]*responsesItem
	model       string
	usage       any
	// finishReason is the last upstream finish_reason seen (recorded in
	// accumulateResponsesChunk); the terminal response status keys on it
	// ("length" → incomplete/max_output_tokens, everything else completed).
	finishReason string
	// toolMap restores client tool names (issue #140): the request renamed
	// mapped tools to official signature names upstream, so function_call
	// items must carry the CLIENT's dispatch name.
	toolMap convert.ToolMapper
}

// relayResponsesStream translates upstream chat SSE chunks into Responses
// SSE events. On an in-band upstream error chunk it emits response.failed
// with the error attached and stops (the client gets a terminal, parseable
// signal instead of a chat-shaped error frame).
func (s *Server) relayResponsesStream(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time, model, respID string) {
	flusher, keepalive, lines, lastWrite, ok := newStreamRelay(ctx, w, r)
	if !ok {
		s.logger.Warn("response writer does not support flushing")
		return
	}
	defer keepalive.Stop()

	createdAt := time.Now().Unix()
	// Issue #164 parity: the response object names the proxy's served model
	// (lease.Model, fallbacks included); only a lease-less direct relay
	// falls back to the requested model / upstream echo.
	servedModel := stats.servedModel
	if servedModel == "" {
		servedModel = model
	}
	st := &responsesStreamState{toolByUpIdx: make(map[int]*responsesItem), model: servedModel, toolMap: stats.toolMap}
	// send writes one SSE frame. The event name is a separate literal
	// argument (never taken from the payload map) so the event: line is
	// always constant; the payload map keeps its "type" key — OpenAI
	// Responses data frames carry it (conformance contract).
	send := func(event string, ev map[string]any) {
		b, _ := json.Marshal(ev)
		_, _ = io.WriteString(w, "event: "+event+"\n")
		_, _ = w.Write(convert.EncodeSSE(b))
		flusher.Flush()
	}
	send("response.created", map[string]any{"type": "response.created", "response": responsesBase(model, respID, createdAt, "in_progress")})
	send("response.in_progress", map[string]any{"type": "response.in_progress", "response": responsesBase(model, respID, createdAt, "in_progress")})

	first := true
	endTurnCallIndexes := make(map[int]bool)
	// XML tool-call extractor: models such as MiMo/Hermes/Qwen emit tool
	// calls as XML/JSON text blocks inline in delta.content instead of
	// native delta.tool_calls. One instance per stream; Feed every content
	// delta in order; Flush once before the terminal frame.
	xmlExtractor := &convert.XMLToolCallExtractor{}
	xmlCallIndex := 0 // sequential synthetic tool-call indexes for extracted calls
	lastID := ""

	// flushXMLCalls releases any still-open candidate block at stream end:
	// extracted calls become native tool_calls fragments (with sequential
	// synthetic indexes) and any scrubbed text is relayed as a content
	// delta, so accumulateResponsesChunk creates the items and the terminal
	// frame carries complete output (shared core, issue #245).
	flushXMLCalls := func() {
		ft, frags := drainXMLToolCalls(xmlExtractor, &xmlCallIndex)
		if ft == "" && len(frags) == 0 {
			return
		}
		delta := make(map[string]any, 2)
		if ft != "" {
			delta["content"] = ft
		}
		if len(frags) > 0 {
			delta["tool_calls"] = frags
		}
		id := "chatcmpl-flush"
		if lastID != "" {
			id = lastID
		}
		mdl := ""
		if st.model != "" {
			mdl = st.model
		}
		synthetic := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   mdl,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
		}
		s.accumulateResponsesChunk(st, synthetic, send)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			maybeKeepalive(w, flusher, lastWrite, "event: ping\ndata: {\"type\": \"ping\"}\n\n")
		case lc := <-lines:
			if lc.err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("responses upstream stream error", "err", lc.err)
					flushXMLCalls()
					s.endResponsesStream(w, send, st, model, respID, createdAt, true, map[string]any{
						"type":    "upstream_stream_error",
						"message": "upstream stream error: " + lc.err.Error(),
					})
				}
				return
			}
			if lc.done {
				flushXMLCalls()
				s.endResponsesStream(w, send, st, model, respID, createdAt, false, nil)
				return
			}
			clean, drop := convert.SanitizeChunkOpts(lc.line, s.convertOptions())
			if drop {
				// Dropped upstream lines are never relayed and must not
				// advance the keepalive timer (client sees only real
				// frames — #161).
				continue
			}
			var chunk map[string]any
			if err := json.Unmarshal(clean, &chunk); err != nil {
				continue
			}
			// --- XML tool calls embedded in content ---
			// Feed each content delta through the stream extractor: safe text
			// is relayed as-is, extracted calls become native tool_calls
			// fragments with sequential synthetic indexes (existing native
			// indexes stay untouched). The rest of the pipeline
			// (processEndTurnCalls + accumulateResponsesChunk) translates
			// the mutated chunk as usual (shared core, issue #245).
			feedXMLToolCalls(xmlExtractor, chunk, &xmlCallIndex)
			// --- end_turn pseudo-tool-call filtering ---
			// Shared pipeline core (issue #246): record end_turn indexes
			// before stripping, strip, drop continuation fragments — the
			// same semantics the OpenAI relay orbits.
			foundEndTurn, toolCallsRemaining, _, _ := processEndTurnCalls(chunk, endTurnCallIndexes, nil, true)
			// Flip finish_reason only when end_turn calls were actually found
			// in this chunk and no real tool calls remain. Without the
			// foundEndTurn gate, the terminal chunk (finish_reason: "tool_calls",
			// empty delta) would be incorrectly rewritten to "stop" for
			// non-end_turn tool calls.
			if foundEndTurn && !toolCallsRemaining {
				if rawChoices, ok := chunk["choices"].([]any); ok {
					for _, c := range rawChoices {
						choice, _ := c.(map[string]any)
						if choice == nil {
							continue
						}
						if fr, ok := choice["finish_reason"].(string); ok && fr == "tool_calls" {
							choice["finish_reason"] = "stop"
						}
					}
				}
			}
			// Skip chunk if it was emptied by end_turn stripping (delta now
			// empty AND finish_reason is null/absent — a real terminal chunk
			// with finish_reason must never be dropped).
			if foundEndTurn && !toolCallsRemaining {
				if rawChoices, ok := chunk["choices"].([]any); ok {
					for _, c := range rawChoices {
						choice, _ := c.(map[string]any)
						if choice == nil {
							continue
						}
						if delta, ok := choice["delta"].(map[string]any); ok && len(delta) == 0 {
							if fr, ok := choice["finish_reason"]; !ok || fr == nil {
								drop = true
							}
						}
					}
				}
			}
			if drop {
				// Chunk emptied by end_turn stripping: nothing was written
				// to the client, so the keepalive timer must not advance.
				continue
			}
			if first {
				first = false
				phasetiming.FromContext(ctx).Since(phasetiming.UpstreamTTFBMS, chatStart)
			}
			if errVal, hasErr := chunk["error"]; hasErr && errVal != nil {
				// In-band upstream failure: mirror the error frame's
				// message/type on the response object and fail the stream.
				var msg, typ string
				if em, ok := errVal.(map[string]any); ok {
					msg, _ = em["message"].(string)
					typ, _ = em["type"].(string)
				} else if es, ok := errVal.(string); ok {
					msg = es
				}
				if msg == "" {
					msg = "upstream error"
				}
				if typ == "" {
					typ = "upstream_error"
				}
				s.endResponsesStream(w, send, st, model, respID, createdAt, true, map[string]any{"message": msg, "type": typ})
				return
			}
			*lastWrite = time.Now()
			stats.chunks++
			stats.bytes += len(clean)
			if stats.servedModel == "" {
				// Lease-less relay: trust the upstream echo (it is the only
				// identity available). With a lease, the served model is
				// authoritative — the upstream echo must not override it.
				if m, _ := chunk["model"].(string); m != "" {
					st.model = m
				}
			}
			if id, _ := chunk["id"].(string); id != "" {
				lastID = id
			}
			if usage, ok := chunk["usage"]; ok && usage != nil {
				st.usage = usage
				stats.usageTokens = usageTotalTokens(usage) // #122 spend ledger
			}
			s.accumulateResponsesChunk(st, chunk, send)
		}
	}
}

// endResponsesStream emits the per-item done events and the terminal
// response.completed (or response.failed) event. On the failure path no
// done events are emitted — the items stay in_progress and the terminal
// response.failed carries the error (a failed response must not claim
// completed items).
func (s *Server) endResponsesStream(w http.ResponseWriter, send func(string, map[string]any), st *responsesStreamState, model, respID string, createdAt int64, failed bool, errObj map[string]any) {
	if !failed {
		// Ensure at least one output item so output is never empty.
		if len(st.items) == 0 {
			item := &responsesItem{id: "msg_" + randHexString(12), kind: "message", outputIndex: st.nextIndex}
			st.nextIndex++
			st.items = append(st.items, item)
		}
		for _, item := range st.items {
			switch item.kind {
			case "message":
				if !item.started {
					sendResponsesItemAdded(send, item)
				}
				part := map[string]any{"type": "output_text", "text": item.text, "annotations": []any{}}
				send("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "text": item.text})
				send("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "part": part})
				send("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "message", "status": "completed", "role": "assistant", "content": []any{part}}})
			case "reasoning":
				send("response.reasoning_text.done", map[string]any{"type": "response.reasoning_text.done", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "text": item.text})
				send("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "reasoning", "status": "completed", "summary": []any{}, "content": []any{map[string]any{"type": "reasoning_text", "text": item.text}}}})
			case "function_call":
				// The spec's Responses stream sequence for a function call
				// item is: function_call_arguments.delta*,
				// function_call_arguments.done, then output_item.done.
				// The custom_tool_call_input.* pair carries the same
				// fragments under the newer event name (codex consumes it).
				send("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": item.id, "output_index": item.outputIndex, "call_id": item.callID, "name": item.name, "arguments": item.args.String()})
				send("response.custom_tool_call_input.done", map[string]any{"type": "response.custom_tool_call_input.done", "item_id": item.id, "output_index": item.outputIndex, "input": item.args.String()})
				send("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "function_call", "status": "completed", "call_id": item.callID, "name": item.name, "arguments": item.args.String()}})
			}
		}
	}
	resp := responsesBase(model, respID, createdAt, "completed")
	resp["model"] = st.model
	out := make([]any, 0, len(st.items))
	for _, item := range st.items {
		switch item.kind {
		case "message":
			out = append(out, map[string]any{
				"id": item.id, "type": "message", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": item.text, "annotations": []any{}}},
			})
		case "reasoning":
			out = append(out, map[string]any{
				"id": item.id, "type": "reasoning", "status": "completed",
				"summary": []any{},
				"content": []any{map[string]any{"type": "reasoning_text", "text": item.text}},
			})
		case "function_call":
			out = append(out, map[string]any{
				"id": item.id, "type": "function_call", "status": "completed",
				"call_id": item.callID, "name": item.name, "arguments": item.args.String(),
			})
		}
	}
	resp["output"] = out
	if st.usage != nil {
		resp["usage"] = responsesUsage(st.usage)
	}
	// Upstream "length" means the output was truncated by max_output_tokens:
	// the Responses object must read "incomplete" with the matching
	// incomplete_details (never "completed" — issue #172).
	if !failed && st.finishReason == "length" {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if failed {
		resp["status"] = "failed"
		if errObj != nil {
			resp["error"] = errObj
		}
		send("response.failed", map[string]any{"type": "response.failed", "response": resp})
		return
	}
	send("response.completed", map[string]any{"type": "response.completed", "response": resp})
}

// sendResponsesItemAdded emits the output_item.added + content_part.added
// pair for a message item.
func sendResponsesItemAdded(send func(string, map[string]any), item *responsesItem) {
	send("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}})
	send("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
}

// accumulateResponsesChunk translates one upstream chat chunk into
// Responses events: text, reasoning and tool-call argument deltas, creating
// output items on first use.
func (s *Server) accumulateResponsesChunk(st *responsesStreamState, chunk map[string]any, send func(string, map[string]any)) {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, ok := choices[0].(map[string]any)
	if !ok || choice == nil {
		return
	}
	// Record the upstream finish_reason (before the delta guard: the
	// terminal chunk can carry finish_reason with an empty delta).
	if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
		st.finishReason = fr
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return
	}
	// Tool-call fragments: one output item per upstream tool index.
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range tcs {
			tc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			upIdx := 0
			if i, ok := numFloat64(tc["index"]); ok {
				upIdx = int(i)
			}
			item := st.toolByUpIdx[upIdx]
			if item == nil {
				item = &responsesItem{id: "fc_" + randHexString(12), kind: "function_call", outputIndex: st.nextIndex}
				st.nextIndex++
				st.toolByUpIdx[upIdx] = item
				st.items = append(st.items, item)
				send("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "function_call", "status": "in_progress", "call_id": "", "name": "", "arguments": ""}})
			}
			if fn, ok := tc["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" && item.name == "" {
					item.name = st.toolMap.RestoreName(name)
				}
				if args, ok := fn["arguments"].(string); ok && args != "" {
					item.args.WriteString(args)
					send("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": item.id, "output_index": item.outputIndex, "delta": args})
					// The spec's newer event name for the same fragment: codex
					// consumes custom_tool_call_input.*, legacy clients consume
					// function_call_arguments.* — emit both.
					send("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "item_id": item.id, "output_index": item.outputIndex, "delta": args})
				}
			}
			if id, ok := tc["id"].(string); ok && id != "" && item.callID == "" {
				item.callID = id
			}
		}
	}
	// Reasoning deltas -> response.reasoning_text events: upstream chat
	// reasoning_content (and aliases) is relayed as a first-class reasoning
	// output item (added -> deltas -> done) so clients that consume the
	// reasoning_* event family receive it. The reasoning text NEVER becomes
	// output text.
	if reasoning, ok := firstStringOf(delta, "reasoning_content", "reasoning", "reasoning_text", "thinking"); ok && reasoning != "" {
		var item *responsesItem
		for _, it := range st.items {
			if it.kind == "reasoning" {
				item = it
				break
			}
		}
		if item == nil {
			item = &responsesItem{id: "rs_" + randHexString(12), kind: "reasoning", outputIndex: st.nextIndex}
			st.nextIndex++
			st.items = append(st.items, item)
		}
		if !item.started {
			item.started = true
			send("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "reasoning", "status": "in_progress", "summary": []any{}}})
		}
		item.text += reasoning
		send("response.reasoning_text.delta", map[string]any{"type": "response.reasoning_text.delta", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "delta": reasoning})
	}
	// Text deltas.
	if content, ok := delta["content"].(string); ok && content != "" {
		var item *responsesItem
		for _, it := range st.items {
			if it.kind == "message" {
				item = it
				break
			}
		}
		if item == nil {
			item = &responsesItem{id: "msg_" + randHexString(12), kind: "message", outputIndex: st.nextIndex}
			st.nextIndex++
			st.items = append(st.items, item)
		}
		if !item.started {
			item.started = true
			sendResponsesItemAdded(send, item)
		}
		item.text += content
		send("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "delta": content})
	}
}

// relayResponsesJSON drains the upstream stream and writes one completed
// Responses object. On any decode/stream error a 502 is returned.
func (s *Server) relayResponsesJSON(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time, model, respID string) {
	acc := convert.NewAccumulatorOpts(s.convertOptions())
	if err := drainUpstream(ctx, r, acc, stats, chatStart); err != nil {
		if errors.Is(err, errDrainUpstreamDecode) {
			s.writeJSONError(w, http.StatusBadGateway,
				"failed to decode upstream stream: "+errDrainCause(err), "upstream_error", "upstream_unavailable", 0)
		} else {
			s.writeJSONError(w, http.StatusBadGateway,
				"upstream stream error: "+errDrainCause(err), "upstream_error", "upstream_unavailable", 0)
		}
		return
	}
	// Accumulate into a Responses output list.
	var completion map[string]any
	if err := json.Unmarshal(acc.Finish(), &completion); err != nil {
		s.writeJSONError(w, http.StatusBadGateway,
			"failed to decode upstream stream: "+err.Error(), "upstream_error", "upstream_unavailable", 0)
		return
	}
	convert.StripEndTurnToolCalls(completion)
	// Restore client tool names (issue #140): the completion's tool_calls
	// carry official signature names; the client dispatches on its own.
	stats.toolMap.FromUpstreamChunk(completion)
	resp := responsesBase(model, respID, time.Now().Unix(), "completed")
	if stats.servedModel != "" {
		// Issue #164: the response names the model the lease actually
		// served (fallbacks included), never the upstream echo.
		resp["model"] = stats.servedModel
	} else if m, _ := completion["model"].(string); m != "" {
		resp["model"] = m
	}
	out := make([]any, 0, 2)
	choices, _ := completion["choices"].([]any)
	finishReason := ""
	if len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			finishReason, _ = choice["finish_reason"].(string)
			if msg, ok := choice["message"].(map[string]any); ok {
				// Stream/non-stream parity: relay upstream reasoning as a
				// first-class reasoning output item (never output text),
				// mirroring accumulateResponsesChunk's item shape. Emitted
				// first so it precedes the message item like the streaming
				// path's output order.
				if reasoning, ok := firstStringOf(msg, "reasoning_content", "reasoning", "reasoning_text", "thinking"); ok && reasoning != "" {
					out = append(out, map[string]any{
						"id":      "rs_" + randHexString(12),
						"type":    "reasoning",
						"status":  "completed",
						"summary": []any{},
						"content": []any{map[string]any{"type": "reasoning_text", "text": reasoning}},
					})
				}
				text, _ := msg["content"].(string)
				if text != "" {
					item := map[string]any{
						"id": "msg_" + randHexString(12), "type": "message", "status": "completed", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
					}
					out = append(out, item)
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, raw := range tcs {
						tc, ok := raw.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := tc["function"].(map[string]any)
						name, _ := fn["name"].(string)
						args, _ := fn["arguments"].(string)
						id, _ := tc["id"].(string)
						if id == "" {
							id = "call_" + randHexString(12)
						}
						out = append(out, map[string]any{
							"id": "fc_" + randHexString(12), "type": "function_call", "status": "completed",
							"call_id": id, "name": name, "arguments": args,
						})
					}
				}
			}
		}
	}
	resp["output"] = out
	// Upstream "length" = truncated by max_output_tokens: mirror the
	// streaming path's incomplete status (issue #172).
	if finishReason == "length" {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if usage, ok := completion["usage"]; ok && usage != nil {
		resp["usage"] = responsesUsage(usage)
		stats.usageTokens = usageTotalTokens(usage) // #122 spend ledger
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
