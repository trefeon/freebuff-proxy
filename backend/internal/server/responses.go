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

	"freebuff-proxy/backend/internal/convert"
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
	normalized, _, err := convert.NormalizeRequestMappedOpts(chatParams, model, s.convertOptions())
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	r = r.WithContext(withOriginalBody(r.Context(), chatParams)) // #140: response-side restore map
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
// completions parameters (compact JSON) and ERRORS on feature-flagged
// params the stateless chat-completions gateway cannot honor (the upstream
// has no stored conversation, no built-in tools, no background runs, no
// moderation). Ported from the reference responsesToChatParams /
// responsesInputToMessages, with silent-filter gaps replaced by honest
// errors per the parity table.
//
// Mapping policy:
//   - model, stream: passthrough (handler-level).
//   - instructions → system message; input (string / message items /
//     function_call_output → tool) → messages; input_image parts map to
//     chat image_url parts (the upstream chat endpoint accepts them).
//   - max_output_tokens → max_completion_tokens; reasoning.effort →
//     reasoning_effort (only when enabled is not false); text.format →
//     response_format (json_object/json_schema); tool_choice auto/required/
//     none/function → chat tool_choice; flat function tools → chat function
//     envelope; parallel_tool_calls, temperature, top_p, user, metadata,
//     seed, stop: passthrough.
//   - store: forwarded (chat wire accepts it) but the gateway performs no
//     storage — documented no-op.
//   - previous_response_id, conversation (server-side conversation state),
//     background:true (async), moderation, built-in tools (web_search &
//     co.) and built-in tool_choice targets: explicit 400 — the client
//     asked for behavior the gateway does not implement.
//   - include (incl. reasoning.encrypted_content), truncation, service_tier, max_tool_calls, prompt,
//     safety_identifier, prompt_cache_key, stream_options,
//     context_management: no chat-completion analogue — ignored
//     (documented, not silent); the input item type function_call is
//     replayed as assistant tool_calls so its matching
//     function_call_output tool message is not orphaned, while
//     reasoning / item_reference remain non-replayable without stored
//     state and skipped (documented).
func responsesToChatParams(raw map[string]any) ([]byte, error) {
	// Hard-unsupported gate: server-side conversation state, background
	// runs and moderation have no chat-completion mapping — fail loudly
	// rather than silently degrading the request.
	if v, ok := raw["previous_response_id"]; ok && v != nil {
		if s, _ := v.(string); s != "" {
			return nil, fmt.Errorf("unsupported parameter \"previous_response_id\": the gateway is stateless and cannot resume a previous response; send the full input")
		}
	}
	if v, ok := raw["conversation"]; ok && v != nil {
		return nil, fmt.Errorf("unsupported parameter \"conversation\": server-side conversation state is not supported by this gateway; send the full input")
	}
	if bg, ok := raw["background"].(bool); ok && bg {
		return nil, fmt.Errorf("unsupported parameter \"background\": background runs are not supported by this gateway")
	}
	if v, ok := raw["moderation"]; ok && v != nil {
		return nil, fmt.Errorf("unsupported parameter \"moderation\": request moderation is not supported by this gateway")
	}

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
		enabled := true
		if en, ok := re["enabled"].(bool); ok {
			enabled = en
		}
		if enabled {
			if eff, ok := re["effort"].(string); ok && eff != "" {
				chat["reasoning_effort"] = strings.ToLower(strings.TrimSpace(eff))
			}
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
	// Flat Responses function tools → chat function envelope. Built-in tool
	// types (web_search, file_search, code_interpreter, computer_use) have
	// no chat-completions mapping — an explicit 400 beats silently sending
	// model output the client cannot consume; the gateway has no built-in
	// tool runner either.
	if tools, ok := raw["tools"].([]any); ok {
		chatTools := make([]any, 0, len(tools))
		for _, t := range tools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := tool["type"].(string)
			if typ != "function" {
				return nil, fmt.Errorf("unsupported parameter \"tools\": built-in tool type %q is not supported by this gateway (only function tools translate to the upstream chat endpoint)", typ)
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
	// Responses tool_choice: "auto" | "required" | "none" map directly;
	// {type:"function"} maps to the chat function choice; a built-in tool
	// target has no chat mapping (same honest-error rule as tools).
	if tc, ok := raw["tool_choice"]; ok && tc != nil {
		if s, ok := tc.(string); ok {
			switch s {
			case "auto", "required", "none":
				chat["tool_choice"] = s
			default:
				return nil, fmt.Errorf("unsupported parameter \"tool_choice\": value %q is not supported by this gateway", s)
			}
		} else if tcm, ok := tc.(map[string]any); ok {
			typ, _ := tcm["type"].(string)
			switch typ {
			case "function":
				if name, _ := tcm["name"].(string); name != "" {
					chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
				} else {
					chat["tool_choice"] = "auto"
				}
			case "":
				return nil, fmt.Errorf("unsupported parameter \"tool_choice\": object without a \"type\"")
			default:
				return nil, fmt.Errorf("unsupported parameter \"tool_choice\": built-in tool type %q is not supported by this gateway (only function tools translate)", typ)
			}
		}
	}
	messages, err := responsesInputToMessages(raw["input"], raw["instructions"])
	if err != nil {
		return nil, err
	}
	chat["messages"] = messages
	return json.Marshal(chat)
}

// responsesInputToMessages converts the Responses input (string or item
// array) plus instructions into chat messages. Ported from the reference
// responsesInputToMessages, extended with honest multimodal handling:
// input_image parts map to chat image_url parts (the upstream chat endpoint
// accepts them — the CLI converts image file parts to image_url itself),
// input_audio errors (no chat-completion analogue). function_call items are
// replayed as assistant tool_calls so their matching function_call_output
// tool message is not orphaned; reasoning / item_reference carry no state
// the upstream can use and are skipped (documented — the reference does the
// same).
func responsesInputToMessages(input any, instructions any) ([]any, error) {
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
			case "function_call":
				// Replay the assistant turn that requested the tool. It must
				// survive translation even though the upstream never runs it:
				// the matching "function_call_output" above becomes a
				// role:"tool" message, and a tool message whose tool_call_id
				// has no preceding assistant tool_calls entry is an orphan.
				// Chat backends drop orphaned tool replies, so the model never
				// sees the result and re-issues the identical call forever.
				callID, _ := entry["call_id"].(string)
				name, _ := entry["name"].(string)
				arguments, _ := entry["arguments"].(string)
				if callID == "" || name == "" {
					continue
				}
				if arguments == "" {
					arguments = "{}"
				}
				messages = append(messages, map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{map[string]any{
						"id":   callID,
						"type": "function",
						"function": map[string]any{
							"name":      name,
							"arguments": arguments,
						},
					}},
				})
			case "reasoning", "item_reference":
				// Non-replayable items are skipped: the upstream cannot use
				// them and they carry no state the model needs.
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
						case "input_image":
							imageURL, _ := part["image_url"].(string)
							if imageURL == "" {
								if urlObj, ok := part["image_url"].(map[string]any); ok {
									imageURL, _ = urlObj["url"].(string)
								}
							}
							if imageURL == "" {
								continue
							}
							parts = append(parts, map[string]any{
								"type":      "image_url",
								"image_url": map[string]any{"url": imageURL},
							})
						case "input_audio":
							return nil, fmt.Errorf("unsupported parameter \"input\": audio input parts are not supported by this gateway")
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
	return messages, nil
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
	// Accept both the Responses-native and the chat-completion detail key
	// names: a gateway upstream echoes prompt_tokens_details /
	// completion_tokens_details, the Responses shape carries
	// input_tokens_details / output_tokens_details. Later maps win, so the
	// Responses-native names take precedence when both are present.
	inputDetails := details("prompt_tokens_details", "input_tokens_details")
	outputDetails := details("completion_tokens_details", "output_tokens_details")
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
