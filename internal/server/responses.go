package server

import (
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
				"missing required field \"model\"; available: "+strings.Join(s.servedModels(), ", "),
				"invalid_request_error", "model_not_found", 0)
			return
		}
	}
	if !s.modelAllowed(model) {
		s.writeJSONError(w, http.StatusBadRequest,
			ModelUnavailableMessage(rawModel), "invalid_request_error", "model_unavailable", 0)
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
