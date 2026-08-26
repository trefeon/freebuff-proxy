package server

// Anthropic non-streaming translation: relayAnthropicJSON drains the upstream
// stream into one Anthropic message object (anthropicMessageFromCompletion) and
// maps the finish reason to the Anthropic vocabulary.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"freebuff-proxy/internal/convert"
	"freebuff-proxy/internal/phasetiming"
)

// --- non-streaming translation ---

// relayAnthropicJSON drains the upstream stream and writes one Anthropic
// message object. On any decode/stream error a 502 is returned with an
// Anthropic error envelope â€” this path serves only /v1/messages, so the
// OpenAI-shaped body writeJSONError produces is never correct here.
func (s *Server) relayAnthropicJSON(ctx context.Context, w http.ResponseWriter, r *http.Request, up io.Reader, stats *relayStats, chatStart time.Time, requestedModel string) {
	acc := convert.NewAccumulator()
	scanner := bufio.NewScanner(up)
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
			s.writeAnthropicError(w, r, http.StatusBadGateway,
				"failed to decode upstream stream: "+err.Error(), "upstream_error", 0)
			return
		}
		stats.chunks++
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() == nil {
			s.writeAnthropicError(w, r, http.StatusBadGateway,
				"upstream stream error: "+err.Error(), "upstream_error", 0)
		}
		return
	}
	var completion map[string]any
	if err := json.Unmarshal(acc.Finish(), &completion); err != nil {
		s.writeAnthropicError(w, r, http.StatusBadGateway,
			"failed to decode upstream stream: "+err.Error(), "upstream_error", 0)
		return
	}
	// Issue #164: the message object names the proxy's served model (lease.Model,
	// fallbacks included), not the raw requested id â€” fall back to the
	// requested model only when the relay ran without a lease.
	servedModel := stats.servedModel
	if servedModel == "" {
		servedModel = requestedModel
	}
	// Restore client tool names (#140 P2a) BEFORE the Anthropic translation
	// reads them: tool_use blocks must carry the client's dispatch name.
	stats.toolMap.FromUpstreamChunk(completion)
	msgObj := anthropicMessageFromCompletion(completion, servedModel)
	out, err := json.Marshal(msgObj)
	if err != nil {
		s.writeAnthropicError(w, r, http.StatusBadGateway,
			"failed to build response: "+err.Error(), "upstream_error", 0)
		return
	}
	if s.reasoningCache != nil {
		var reasoningStr string
		var toolIDs []string
		var contentStr string
		var toolCallsJSON string
		model, _ := msgObj["model"].(string)

		if choices, ok := completion["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				if msg, ok := choice["message"].(map[string]any); ok {
					if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
						reasoningStr = rc
					} else if r, ok := msg["reasoning"].(string); ok && r != "" {
						reasoningStr = r
					}
					if c, ok := msg["content"].(string); ok {
						contentStr = c
					}
					if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
						for _, raw := range tcs {
							if tc, ok := raw.(map[string]any); ok {
								if id, ok := tc["id"].(string); ok && id != "" {
									toolIDs = append(toolIDs, id)
									if sID := sanitizeToolID(id); sID != id && sID != "" {
										toolIDs = append(toolIDs, sID)
									}
								}
							}
						}
						if b, err := json.Marshal(tcs); err == nil {
							toolCallsJSON = string(b)
						}
					}
				}
			}
		}

		if reasoningStr == "" || len(toolIDs) == 0 {
			if contentBlocks, ok := msgObj["content"].([]any); ok {
				for _, block := range contentBlocks {
					if bm, ok := block.(map[string]any); ok {
						bType, _ := bm["type"].(string)
						if bType == "thinking" && reasoningStr == "" {
							reasoningStr, _ = bm["thinking"].(string)
						} else if bType == "tool_use" {
							if id, ok := bm["id"].(string); ok && id != "" {
								toolIDs = append(toolIDs, id)
							}
						} else if bType == "text" && contentStr == "" {
							contentStr, _ = bm["text"].(string)
						}
					}
				}
			}
		}

		if reasoningStr != "" && len(toolIDs) > 0 {
			s.reasoningCache.Put(toolIDs, contentStr, toolCallsJSON, reasoningStr, "", model)
		}
	}
	if usage, ok := completion["usage"].(map[string]any); ok {
		stats.usageTokens = usageTotalTokens(usage) // #122 spend ledger
	}
	stats.bytes = len(out)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// anthropicMessageFromCompletion builds the Anthropic message object from
// an accumulated chat.completion. servedModel is the authoritative model the
// proxy's lease was bound to (issue #164) and wins over the upstream echo;
// the echo only fills the field when no served model is known (direct unit
// calls) â€” the response must name what actually served the request.
func anthropicMessageFromCompletion(completion map[string]any, servedModel string) map[string]any {
	id, _ := completion["id"].(string)
	if id == "" {
		id = "msg_" + randHexString(10)
	}
	model := servedModel
	if model == "" {
		model, _ = completion["model"].(string)
	}
	message := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []any{},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
	}
	hasToolCall := false
	choices, _ := completion["choices"].([]any)
	if len(choices) > 0 {
		choice, ok := choices[0].(map[string]any)
		if ok && choice != nil {
			msg, _ := choice["message"].(map[string]any)
			if msg != nil {
				content := []any{}
				if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
					content = append(content, map[string]any{"type": "thinking", "thinking": rc, "signature": ""})
				}
				if text, ok := msg["content"].(string); ok && text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, raw := range tcs {
						tc, ok := raw.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := tc["function"].(map[string]any)
						name, _ := fn["name"].(string)
						if name == "end_turn" {
							continue // strip proxy-injected end_turn pseudo tool
						}
						args, _ := fn["arguments"].(string)
						toolID, _ := tc["id"].(string)
						if toolID == "" {
							toolID = "toolu_" + randHexString(6)
						}
						hasToolCall = true
						content = append(content, map[string]any{
							"type":  "tool_use",
							"id":    sanitizeToolID(toolID),
							"name":  name,
							"input": parseJSONArgs(args),
						})
					}
				}
				message["content"] = content
				if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
					message["stop_reason"] = anthropicStopReason(fr, hasToolCall)
				}
			}
		}
	}
	if message["stop_reason"] == "end_turn" && hasToolCall {
		message["stop_reason"] = "tool_use"
	} else if !hasToolCall && message["stop_reason"] == "tool_use" {
		message["stop_reason"] = "end_turn"
	}
	if usage, ok := completion["usage"].(map[string]any); ok {
		message["usage"] = openAIUsageToAnthropic(usage)
	}
	uMap, _ := message["usage"].(map[string]any)
	if uMap == nil {
		uMap = map[string]any{"input_tokens": 0, "output_tokens": 0}
		message["usage"] = uMap
	}
	if _, ok := uMap["input_tokens"]; !ok {
		uMap["input_tokens"] = 0
	}
	if _, ok := uMap["output_tokens"]; !ok {
		uMap["output_tokens"] = 0
	}
	return message
}

// anthropicStopReason maps an OpenAI finish reason to the Anthropic
// vocabulary (tool_calls handled by the caller's hasToolCall flag).
func anthropicStopReason(reason string, hasToolCall bool) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		if hasToolCall {
			return "tool_use"
		}
		return reason
	}
}
