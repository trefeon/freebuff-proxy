package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"freebuff-proxy/internal/convert"
	"freebuff-proxy/internal/phasetiming"
)

// randCounter backs randHexString's crypto/rand failure fallback.
var randCounter atomic.Uint64

// --- OpenAI Responses API (/v1/responses) ---
//
// The Responses API surface is translated to the existing chat-completions
// path: the request body is converted to chat params (tools are wrapped
// from the flat Responses function shape into the chat function shape,
// input+instructions become messages, tool_choice is translated), the
// upstream forced stream is relayed, and the output is translated back to
// Responses SSE events (response.created → response.completed) or a single
// response object. Ported from the reference worker's
// responsesToChatParams / pipeUpstreamToResponsesStream.

// handleResponses is the OpenAI Responses API entry point: translate the
// request to chat params, then route through chatCore with a Responses wire
// relay. The model is optional per the Responses spec; the reference
// defaults it, and probeModel picks the same safest default the smoke test
// uses (deepseek-v4-flash when present).
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.writeJSONError(w, http.StatusRequestEntityTooLarge,
				"request body exceeds the 32MB limit", "invalid_request_error", "content_too_large", 0)
		} else {
			s.writeJSONError(w, http.StatusBadRequest,
				"failed to read request body: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		}
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object", "invalid_request_error", "invalid_json", 0)
		return
	}
	rawModel, _ := raw["model"].(string)
	model := s.reg.ResolveModel(rawModel)
	if rawModel == "" {
		// Responses allows omitting the model; default to the safest
		// catalog model (the reference defaults to DEFAULT_MODEL).
		model = probeModel(s.reg)
		if model == "" {
			s.writeJSONError(w, http.StatusBadRequest,
				"missing required field \"model\"; available: "+strings.Join(s.reg.Models(), ", "),
				"invalid_request_error", "model_not_found", 0)
			return
		}
	}
	if !s.modelAllowed(model) {
		// MODELS_ALLOW: the resolved/probed model is outside the operator
		// allowlist — reject like an unknown model.
		s.writeJSONError(w, http.StatusNotFound,
			"model not allowed by MODELS_ALLOW", "invalid_request_error", "model_not_found", 0)
		return
	}
	stream := false
	if v, ok := raw["stream"].(bool); ok {
		stream = v
	}
	chatParams, err := responsesToChatParams(raw)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"invalid responses request: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	normalized, err := convert.NormalizeRequest(chatParams, model)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	respID := "resp_" + randHexString(12)
	reasoningEffort := convert.ExtractReasoningEffort(raw)
	var relay relayFunc
	if stream {
		relay = func(ctx context.Context, w http.ResponseWriter, up io.Reader, stats *relayStats, chatStart time.Time) {
			s.relayResponsesStream(ctx, w, up, stats, chatStart, model, respID)
		}
	} else {
		relay = func(ctx context.Context, w http.ResponseWriter, up io.Reader, stats *relayStats, chatStart time.Time) {
			s.relayResponsesJSON(ctx, w, up, stats, chatStart, model, respID)
		}
	}
	s.chatCore(w, r, model, stream, normalized, reasoningEffort, "responses", relay)
}

// responsesToChatParams translates a Responses API request body into chat
// completions parameters (compact JSON). Tools are filtered to the
// flat "function" type and wrapped in the chat function envelope; input
// (string or item array) plus instructions become messages; tool_choice is
// translated; max_output_tokens maps to max_completion_tokens and
// reasoning.effort to reasoning_effort. Ported from the reference
// responsesToChatParams / responsesInputToMessages.
func responsesToChatParams(raw map[string]any) ([]byte, error) {
	chat := make(map[string]any)
	for _, k := range []string{"temperature", "top_p", "parallel_tool_calls", "stop", "seed", "store", "metadata", "user"} {
		if v, ok := raw[k]; ok && v != nil {
			chat[k] = v
		}
	}
	if v, ok := raw["max_output_tokens"]; ok && v != nil {
		chat["max_completion_tokens"] = v
	}
	if re, ok := raw["reasoning"].(map[string]any); ok {
		if eff, ok := re["effort"].(string); ok && eff != "" {
			chat["reasoning_effort"] = strings.ToLower(strings.TrimSpace(eff))
		}
	}
	if text, ok := raw["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			ft, _ := format["type"].(string)
			if ft != "" && ft != "text" {
				rf := map[string]any{"type": ft}
				if schema, ok := format["json_schema"]; ok {
					rf["json_schema"] = schema
				}
				chat["response_format"] = rf
			}
		}
	}
	// Flat Responses function tools → chat function envelope; non-function
	// tools (web_search, etc.) are filtered so the upstream never sees a
	// tool type it rejects.
	if tools, ok := raw["tools"].([]any); ok {
		chatTools := make([]any, 0, len(tools))
		for _, t := range tools {
			tool, ok := t.(map[string]any)
			if !ok || tool["type"] != "function" {
				continue
			}
			name, _ := tool["name"].(string)
			desc, _ := tool["description"].(string)
			params := tool["parameters"]
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			chatTools = append(chatTools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": desc,
					"parameters":  params,
				},
			})
		}
		if len(chatTools) > 0 {
			chat["tools"] = chatTools
		}
	}
	// Responses tool_choice: only the function form translates; everything
	// else falls back to auto.
	if tc, ok := raw["tool_choice"].(map[string]any); ok {
		if tc["type"] == "function" {
			if name, _ := tc["name"].(string); name != "" {
				chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
			} else {
				chat["tool_choice"] = "auto"
			}
		} else {
			chat["tool_choice"] = "auto"
		}
	}
	chat["messages"] = responsesInputToMessages(raw["input"], raw["instructions"])
	return json.Marshal(chat)
}

// responsesInputToMessages converts the Responses input (string or item
// array) plus instructions into chat messages. Ported from the reference
// responsesInputToMessages.
func responsesInputToMessages(input any, instructions any) []any {
	messages := make([]any, 0, 8)
	if instructions != nil {
		if s, ok := instructions.(string); ok && strings.TrimSpace(s) != "" {
			messages = append(messages, map[string]any{"role": "system", "content": s})
		}
	}
	switch typed := input.(type) {
	case nil:
		// No input: leave messages with just the instructions (if any).
	case string:
		if strings.TrimSpace(typed) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": typed})
		}
	case []any:
		for _, item := range typed {
			if s, ok := item.(string); ok {
				if strings.TrimSpace(s) != "" {
					messages = append(messages, map[string]any{"role": "user", "content": s})
				}
				continue
			}
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := entry["type"].(string)
			switch typ {
			case "function_call_output":
				callID, _ := entry["call_id"].(string)
				output := entry["output"]
				content := ""
				switch o := output.(type) {
				case nil:
					content = ""
				case string:
					content = o
				default:
					b, _ := json.Marshal(o)
					content = string(b)
				}
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": callID,
					"content":      content,
				})
			case "function_call", "reasoning", "item_reference":
				// Locally unexecutable or non-replayable items are skipped
				// (the reference does the same — the upstream cannot run
				// them).
				continue
			default:
				role, _ := entry["role"].(string)
				if role == "" {
					role = "user"
				}
				content := entry["content"]
				switch c := content.(type) {
				case nil:
					messages = append(messages, map[string]any{"role": role, "content": ""})
				case string:
					messages = append(messages, map[string]any{"role": role, "content": c})
				case []any:
					parts := make([]any, 0, len(c))
					for _, p := range c {
						part, ok := p.(map[string]any)
						if !ok {
							continue
						}
						pt, _ := part["type"].(string)
						switch pt {
						case "input_text", "output_text":
							text, _ := part["text"].(string)
							parts = append(parts, map[string]any{"type": "text", "text": text})
						case "text":
							if text, ok := part["text"].(string); ok {
								parts = append(parts, map[string]any{"type": "text", "text": text})
							}
						}
					}
					msg := map[string]any{"role": role}
					if len(parts) > 0 {
						msg["content"] = parts
					} else {
						msg["content"] = ""
					}
					messages = append(messages, msg)
				default:
					messages = append(messages, map[string]any{"role": role, "content": ""})
				}
			}
		}
	}
	return messages
}

// responsesBase builds the Responses object skeleton with the given status.
func responsesBase(model, id string, createdAt int64, status string) map[string]any {
	return map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           createdAt,
		"status":               status,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"model":                model,
		"output":               []any{},
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            map[string]any{"effort": nil, "summary": nil},
		"store":                true,
		"temperature":          1.0,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                1.0,
		"truncation":           "disabled",
		"usage":                nil,
		"user":                 nil,
		"metadata":             map[string]any{},
	}
}

// responsesUsage renders the Responses usage shape from an upstream chat
// usage object (input/output tokens, cached/reasoning details).
func responsesUsage(usage any) map[string]any {
	u, _ := usage.(map[string]any)
	toInt := func(names ...string) int64 {
		for _, n := range names {
			if v, ok := u[n]; ok {
				switch nv := v.(type) {
				case float64:
					return int64(nv)
				case json.Number:
					if i, err := nv.Int64(); err == nil {
						return i
					}
				}
			}
		}
		return 0
	}
	input := toInt("input_tokens", "prompt_tokens")
	output := toInt("output_tokens", "completion_tokens")
	total := toInt("total_tokens")
	if total == 0 {
		total = input + output
	}
	details := func(names ...string) map[string]any {
		out := map[string]any{}
		for _, n := range names {
			if d, ok := u[n].(map[string]any); ok {
				for k, v := range d {
					out[k] = v
				}
			}
		}
		return out
	}
	inputDetails := details("input_tokens_details")
	outputDetails := details("output_tokens_details")
	cached, _ := inputDetails["cached_tokens"].(float64)
	reasoning, _ := outputDetails["reasoning_tokens"].(float64)
	return map[string]any{
		"input_tokens": input,
		"input_tokens_details": map[string]any{
			"cached_tokens": cached,
		},
		"output_tokens": output,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": reasoning,
		},
		"total_tokens": total,
	}
}

// responsesItem is one output item being assembled during stream relay:
// either a message (text) or a function_call.
type responsesItem struct {
	id          string
	kind        string // "message" | "function_call"
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
}

// relayResponsesStream translates upstream chat SSE chunks into Responses
// SSE events. On an in-band upstream error chunk it emits response.failed
// with the error attached and stops (the client gets a terminal, parseable
// signal instead of a chat-shaped error frame).
func (s *Server) relayResponsesStream(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time, model, respID string) {
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

	createdAt := time.Now().Unix()
	st := &responsesStreamState{toolByUpIdx: make(map[int]*responsesItem), model: model}
	send := func(ev map[string]any) {
		b, _ := json.Marshal(ev)
		_, _ = w.Write(convert.EncodeSSE(b))
		flusher.Flush()
	}
	send(map[string]any{"type": "response.created", "response": responsesBase(model, respID, createdAt, "in_progress")})
	send(map[string]any{"type": "response.in_progress", "response": responsesBase(model, respID, createdAt, "in_progress")})

	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()
	lines := make(chan lineChunk)
	go relayReadLoop(ctx, r, lines)
	relayed := time.Now()
	first := true

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if time.Since(relayed) >= keepaliveInterval {
				_, _ = io.WriteString(w, ": keepalive\n\n")
				relayed = time.Now()
				flusher.Flush()
			}
		case lc := <-lines:
			if lc.err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("responses upstream stream error", "err", lc.err)
					s.endResponsesStream(w, send, st, model, respID, createdAt, true, nil)
				}
				return
			}
			if lc.done {
				s.endResponsesStream(w, send, st, model, respID, createdAt, false, nil)
				return
			}
			clean, drop := convert.SanitizeChunk(lc.line)
			if drop {
				relayed = time.Now()
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
			relayed = time.Now()
			stats.chunks++
			stats.bytes += len(clean)
			if m, _ := chunk["model"].(string); m != "" {
				st.model = m
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
// response.completed (or response.failed) event.
func (s *Server) endResponsesStream(w http.ResponseWriter, send func(map[string]any), st *responsesStreamState, model, respID string, createdAt int64, failed bool, errObj map[string]any) {
	// Ensure at least one output item so output is never empty.
	if len(st.items) == 0 {
		item := &responsesItem{id: "msg_" + randHexString(12), kind: "message", outputIndex: st.nextIndex}
		st.nextIndex++
		st.items = append(st.items, item)
	}
	for _, item := range st.items {
		if item.kind == "message" {
			if !item.started {
				sendResponsesItemAdded(send, item)
			}
			part := map[string]any{"type": "output_text", "text": item.text, "annotations": []any{}}
			send(map[string]any{"type": "response.output_text.done", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "text": item.text})
			send(map[string]any{"type": "response.content_part.done", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "part": part})
			send(map[string]any{"type": "response.output_item.done", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "message", "status": "completed", "role": "assistant", "content": []any{part}}})
		} else {
			send(map[string]any{"type": "response.output_item.done", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "function_call", "status": "completed", "call_id": item.callID, "name": item.name, "arguments": item.args.String()}})
		}
	}
	resp := responsesBase(model, respID, createdAt, "completed")
	resp["model"] = st.model
	out := make([]any, 0, len(st.items))
	for _, item := range st.items {
		if item.kind == "message" {
			out = append(out, map[string]any{
				"id": item.id, "type": "message", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": item.text, "annotations": []any{}}},
			})
		} else {
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
	if failed {
		resp["status"] = "failed"
		if errObj != nil {
			resp["error"] = errObj
		}
		send(map[string]any{"type": "response.failed", "response": resp})
		return
	}
	send(map[string]any{"type": "response.completed", "response": resp})
}

// sendResponsesItemAdded emits the output_item.added + content_part.added
// pair for a message item.
func sendResponsesItemAdded(send func(map[string]any), item *responsesItem) {
	send(map[string]any{"type": "response.output_item.added", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}})
	send(map[string]any{"type": "response.content_part.added", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
}

// accumulateResponsesChunk translates one upstream chat chunk into
// Responses events: text deltas and tool-call argument deltas, creating
// output items on first use.
func (s *Server) accumulateResponsesChunk(st *responsesStreamState, chunk map[string]any, send func(map[string]any)) {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
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
				send(map[string]any{"type": "response.output_item.added", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "function_call", "status": "in_progress", "call_id": "", "name": "", "arguments": ""}})
			}
			if fn, ok := tc["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" && item.name == "" {
					item.name = name
				}
				if args, ok := fn["arguments"].(string); ok && args != "" {
					item.args.WriteString(args)
					send(map[string]any{"type": "response.function_call_arguments.delta", "item_id": item.id, "output_index": item.outputIndex, "delta": args})
				}
			}
			if id, ok := tc["id"].(string); ok && id != "" && item.callID == "" {
				item.callID = id
			}
		}
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
		send(map[string]any{"type": "response.output_text.delta", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "delta": content})
	}
}

// relayResponsesJSON drains the upstream stream and writes one completed
// Responses object. On any decode/stream error a 502 is returned.
func (s *Server) relayResponsesJSON(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time, model, respID string) {
	acc := convert.NewAccumulator()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLine)
	first := true
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		if first {
			first = false
			phasetiming.FromContext(ctx).Since(phasetiming.UpstreamTTFBMS, chatStart)
		}
		if err := acc.Add(scanner.Bytes()); err != nil {
			s.writeJSONError(w, http.StatusBadGateway,
				"failed to decode upstream stream: "+err.Error(), "upstream_error", "upstream_unavailable", 0)
			return
		}
		stats.chunks++
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() == nil {
			s.writeJSONError(w, http.StatusBadGateway,
				"upstream stream error: "+err.Error(), "upstream_error", "upstream_unavailable", 0)
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
	resp := responsesBase(model, respID, time.Now().Unix(), "completed")
	if m, _ := completion["model"].(string); m != "" {
		resp["model"] = m
	}
	out := make([]any, 0, 2)
	choices, _ := completion["choices"].([]any)
	if len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if msg, ok := choice["message"].(map[string]any); ok {
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
	if usage, ok := completion["usage"]; ok && usage != nil {
		resp["usage"] = responsesUsage(usage)
		stats.usageTokens = usageTotalTokens(usage) // #122 spend ledger
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// numFloat64 extracts a float64 from a JSON-decoded value.
func numFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
		return float64(n), true
	}
	return 0, false
}

// randHexString returns n random bytes hex-encoded (for response/item ids),
// falling back to time+counter when crypto/rand fails (practically never).
func randHexString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("%x%x", time.Now().UnixNano(), randCounter.Add(1))
}
