package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"freebuff-proxy/internal/convert"
)

// --- Anthropic Messages API (/v1/messages) ---
//
// Native Claude-protocol surface for Claude Code and other Anthropic
// clients: the request body (system, messages with text/thinking/tool_use/
// tool_result parts, tools with input_schema, tool_choice, stop_sequences,
// thinking) is converted to the chat-completions shape, the upstream forced
// stream is relayed, and the output is translated back to Anthropic message
// events (message_start → content_block_start/delta → message_delta →
// message_stop) or a single message object. Ported from the reference
// freebuff2api-quorinex/anthropic.go and freebuff-reverse
// internal/channels/freebuff/anthropic.go. Clients authenticate with
// x-api-key (requireAuth accepts it) and may send anthropic-version; the
// proxy is liberal and does not validate the version header.

// registerAnthropicRoutes wires the Anthropic-compatible surface
// (/v1/messages, /v1/messages/count_tokens) onto the mux. The
// OpenAI-compatible surface registers separately (registerOpenAIRoutes,
// openai.go); shared routes (healthz/metrics/admin) live in server.go's
// Handler.
func (s *Server) registerAnthropicRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/messages", s.requireAuth(s.handleMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.requireAuth(s.handleMessagesCountTokens))
}

// handleMessages is the Anthropic /v1/messages entry point: convert the
// request to chat params, then route through chatCore with an Anthropic
// wire relay.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	version := "2023-06-01"
	if reqVer := r.Header.Get("anthropic-version"); reqVer != "" {
		version = reqVer
	}
	w.Header().Set("anthropic-version", version)

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.writeAnthropicError(w, r, http.StatusRequestEntityTooLarge,
				"request body exceeds the 32MB limit", "content_too_large", 0)
		} else {
			s.writeAnthropicError(w, r, http.StatusBadRequest,
				"failed to read request body: "+err.Error(), "invalid_json", 0)
		}
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		s.writeAnthropicError(w, r, http.StatusBadRequest,
			"request body must be a valid JSON object", "invalid_json", 0)
		return
	}
	rawMsgs, ok := raw["messages"].([]any)
	if !ok || len(rawMsgs) == 0 {
		s.writeAnthropicError(w, r, http.StatusBadRequest,
			"messages: array must not be empty", "invalid_request_error", 0)
		return
	}
	rawModel, _ := raw["model"].(string)
	if rawModel == "" {
		s.writeAnthropicError(w, r, http.StatusBadRequest,
			"missing required field \"model\"; available: "+strings.Join(s.servedModels(), ", "),
			"model_not_found", 0)
		return
	}
	model := s.reg.ResolveModel(rawModel)
	if !s.modelAllowed(model) {
		s.writeAnthropicError(w, r, http.StatusBadRequest,
			ModelUnavailableMessage(rawModel), "invalid_request_error", 0)
		return
	}
	stream := false
	if v, ok := raw["stream"].(bool); ok {
		stream = v
	}
	chatParams, err := anthropicToChatParams(raw)
	if err != nil {
		s.writeAnthropicError(w, r, http.StatusBadRequest,
			"invalid messages request: "+err.Error(), "invalid_json", 0)
		return
	}
	normalized, err := convert.NormalizeRequest(chatParams, model)
	if err != nil {
		s.writeAnthropicError(w, r, http.StatusBadRequest,
			"request body must be a valid JSON object: "+err.Error(), "invalid_json", 0)
		return
	}
	inputTokens := 0
	if s.tokenEstimator != nil {
		if count, err := s.tokenEstimator.CountAnthropicRequest(raw); err == nil && count > 0 {
			inputTokens = count
		}
	}
	var relay relayFunc
	if stream {
		relay = func(ctx context.Context, w http.ResponseWriter, up io.Reader, stats *relayStats, chatStart time.Time) {
			s.relayAnthropicStream(ctx, w, up, stats, chatStart, rawModel, inputTokens)
		}
	} else {
		relay = func(ctx context.Context, w http.ResponseWriter, up io.Reader, stats *relayStats, chatStart time.Time) {
			s.relayAnthropicJSON(ctx, w, r, up, stats, chatStart, rawModel)
		}
	}
	s.chatCore(w, r, model, stream, normalized, convert.ExtractReasoningEffort(raw), "messages", relay)
}

// --- request conversion ---

// anthropicToChatParams converts an Anthropic messages request body into
// chat completions parameters (compact JSON). The model key is left as the
// client's raw model; chatCore's NormalizeRequest modelOverride replaces it
// with the resolved upstream id. Reasoning effort is derived from the
// thinking block and fed through the wave-2 clamp by NormalizeRequest.
func anthropicToChatParams(raw map[string]any) ([]byte, error) {
	chat := make(map[string]any)
	if v, ok := raw["model"]; ok {
		chat["model"] = v
	}
	chat["messages"] = anthropicMessagesToOpenAI(raw)
	if v, ok := raw["max_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	}
	if v, ok := raw["temperature"]; ok && v != nil {
		chat["temperature"] = v
	}
	if v, ok := raw["top_p"]; ok && v != nil {
		chat["top_p"] = v
	}
	if stops, ok := anthropicStopSequencesToOpenAI(raw["stop_sequences"]); ok {
		chat["stop"] = stops
	}
	if meta, ok := raw["metadata"].(map[string]any); ok && meta != nil {
		if uid, ok := meta["user_id"].(string); ok && uid != "" {
			chat["user"] = uid
		}
	} else if v, ok := raw["user"]; ok && v != nil {
		chat["user"] = v
	}
	if effort, ok := anthropicThinkingToEffort(raw["thinking"]); ok {
		chat["reasoning_effort"] = effort
	}
	if tools, ok := raw["tools"].([]any); ok && len(tools) > 0 {
		chatTools := make([]any, 0, len(tools))
		for _, rawTool := range tools {
			tool, ok := rawTool.(map[string]any)
			if !ok {
				continue
			}
			name, _ := tool["name"].(string)
			desc, _ := tool["description"].(string)
			fn := map[string]any{"name": name, "description": desc}
			if schema, ok := tool["input_schema"]; ok && schema != nil {
				fn["parameters"] = schema
			}
			chatTools = append(chatTools, map[string]any{"type": "function", "function": fn})
		}
		if len(chatTools) > 0 {
			chat["tools"] = chatTools
		}
	}
	if tc, ok := anthropicToolChoiceToOpenAI(raw["tool_choice"]); ok {
		chat["tool_choice"] = tc
	}
	if tcm, ok := raw["tool_choice"].(map[string]any); ok {
		if disable, _ := tcm["disable_parallel_tool_use"].(bool); disable {
			chat["parallel_tool_calls"] = false
		}
	}
	return json.Marshal(chat)
}

// anthropicMessagesToOpenAI converts the anthropic messages array (plus the
// top-level system field) into chat messages.
func anthropicMessagesToOpenAI(raw map[string]any) []any {
	messages := make([]any, 0, 8)
	if system := raw["system"]; system != nil {
		if m := anthropicSystemToOpenAI(system); m != nil {
			messages = append(messages, m)
		}
	}
	rawMessages, _ := raw["messages"].([]any)
	for _, rawMsg := range rawMessages {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "assistant":
			messages = append(messages, anthropicAssistantToOpenAI(msg)...)
		case "user":
			messages = append(messages, anthropicUserToOpenAI(msg)...)
		case "system":
			if m := anthropicSystemToOpenAI(msg["content"]); m != nil {
				messages = append(messages, m)
			}
		}
	}
	return messages
}

// anthropicSystemToOpenAI converts the top-level system field (string or an
// array of text parts) into a chat system message, or nil when empty.
func anthropicSystemToOpenAI(system any) map[string]any {
	switch typed := system.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return map[string]any{"role": "system", "content": typed}
	case []any:
		parts := make([]any, 0, len(typed))
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if strings.EqualFold(stringValue(part["type"]), "text") {
				text, _ := part["text"].(string)
				if strings.TrimSpace(text) != "" {
					parts = append(parts, map[string]any{"type": "text", "text": text})
				}
			}
		}
		if len(parts) == 0 {
			return nil
		}
		return map[string]any{"role": "system", "content": normalizeOpenAIContent(parts)}
	default:
		return nil
	}
}

// anthropicUserToOpenAI converts a user message. tool_result parts become
// role "tool" messages (emitted before the user's own text), text/image
// parts become the user message content.
func anthropicUserToOpenAI(msg map[string]any) []any {
	out := make([]any, 0, 2)
	content := msg["content"]
	switch typed := content.(type) {
	case string:
		if strings.TrimSpace(typed) != "" {
			out = append(out, map[string]any{"role": "user", "content": typed})
		}
		return out
	case []any:
		userParts := make([]any, 0, len(typed))
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(stringValue(part["type"])) {
			case "text":
				text, _ := part["text"].(string)
				if strings.TrimSpace(text) != "" {
					userParts = append(userParts, map[string]any{"type": "text", "text": text})
				}
			case "image":
				if img := anthropicImageToOpenAI(part); img != nil {
					userParts = append(userParts, img)
				}
			case "tool_result":
				if tr := anthropicToolResultToOpenAI(part); tr != nil {
					out = append(out, tr)
				}
			}
		}
		if len(userParts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": normalizeOpenAIContent(userParts)})
		}
		return out
	default:
		return out
	}
}

// anthropicAssistantToOpenAI converts an assistant message: text parts →
// content, thinking parts → reasoning_content, tool_use parts → tool_calls
// (with id preserved so the following tool_result messages line up),
// tool_result parts (rare in assistant turns) → trailing tool messages.
func anthropicAssistantToOpenAI(msg map[string]any) []any {
	content, _ := msg["content"].([]any)
	textParts := make([]any, 0, 4)
	reasoning := make([]string, 0, 2)
	toolCalls := make([]any, 0, 2)
	var trailing []any
	for _, rawPart := range content {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(stringValue(part["type"])) {
		case "text":
			text, _ := part["text"].(string)
			if strings.TrimSpace(text) != "" {
				textParts = append(textParts, map[string]any{"type": "text", "text": text})
			}
		case "thinking":
			text, _ := part["thinking"].(string)
			if text == "" {
				text, _ = part["text"].(string)
			}
			if strings.TrimSpace(text) != "" {
				reasoning = append(reasoning, text)
			}
		case "redacted_thinking":
			if data, ok := part["data"].(string); ok && data != "" {
				reasoning = append(reasoning, "[redacted_thinking: "+data+"]")
			}
		case "tool_use", "server_tool_use":
			id := sanitizeToolID(stringValue(part["id"]))
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      stringValue(part["name"]),
					"arguments": marshalJSONArgs(part["input"]),
				},
			})
		case "tool_result":
			if tr := anthropicToolResultToOpenAI(part); tr != nil {
				trailing = append(trailing, tr)
			}
		}
	}
	out := map[string]any{"role": "assistant"}
	if len(textParts) > 0 {
		out["content"] = normalizeOpenAIContent(textParts)
	} else if len(toolCalls) > 0 {
		out["content"] = nil
	} else {
		out["content"] = ""
	}
	if len(reasoning) > 0 {
		out["reasoning_content"] = strings.Join(reasoning, "\n\n")
	}
	if len(toolCalls) > 0 {
		out["tool_calls"] = toolCalls
	}
	result := []any{out}
	result = append(result, trailing...)
	return result
}

// anthropicToolResultToOpenAI converts a tool_result part into a chat role
// "tool" message (or nil when the tool_use_id is missing).
func anthropicToolResultToOpenAI(part map[string]any) map[string]any {
	toolUseID := strings.TrimSpace(stringValue(part["tool_use_id"]))
	if toolUseID == "" {
		return nil
	}
	content := anthropicToolResultContent(part["content"])
	if isErr, ok := part["is_error"].(bool); ok && isErr {
		if content == "" {
			content = "Error: tool execution failed"
		} else if !strings.HasPrefix(content, "Error:") && !strings.HasPrefix(content, "error:") {
			content = "Error: " + content
		}
	}
	return map[string]any{
		"role":         "tool",
		"tool_call_id": sanitizeToolID(toolUseID),
		"content":      content,
	}
}

// anthropicToolResultContent flattens a tool_result content payload to a
// string: text parts joined, image parts as data URLs, anything structured
// JSON-encoded.
func anthropicToolResultContent(content any) string {
	switch typed := content.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		var sb strings.Builder
		for _, rawItem := range typed {
			switch item := rawItem.(type) {
			case string:
				if sb.Len() > 0 {
					sb.WriteString("\n\n")
				}
				sb.WriteString(item)
			case map[string]any:
				switch strings.ToLower(stringValue(item["type"])) {
				case "text":
					if sb.Len() > 0 {
						sb.WriteString("\n\n")
					}
					sb.WriteString(stringValue(item["text"]))
				case "image":
					if img := anthropicImageToOpenAI(item); img != nil {
						if u, ok := img["image_url"].(map[string]any); ok {
							if sb.Len() > 0 {
								sb.WriteString("\n\n")
							}
							sb.WriteString(stringValue(u["url"]))
						}
					}
				default:
					if b, err := json.Marshal(item); err == nil {
						if sb.Len() > 0 {
							sb.WriteString("\n\n")
						}
						sb.WriteString(string(b))
					}
				}
			default:
				if b, err := json.Marshal(item); err == nil {
					if sb.Len() > 0 {
						sb.WriteString("\n\n")
					}
					sb.WriteString(string(b))
				}
			}
		}
		return sb.String()
	default:
		if b, err := json.Marshal(typed); err == nil {
			return string(b)
		}
		return ""
	}
}

// anthropicImageToOpenAI converts a base64/url image part into an OpenAI
// image_url part (base64 images become data URLs).
func anthropicImageToOpenAI(part map[string]any) map[string]any {
	imageURL := ""
	if source, ok := part["source"].(map[string]any); ok {
		switch strings.ToLower(stringValue(source["type"])) {
		case "base64":
			data := stringValue(source["data"])
			if data != "" {
				mediaType := stringValue(source["media_type"])
				if mediaType == "" {
					mediaType = "application/octet-stream"
				}
				imageURL = "data:" + mediaType + ";base64," + data
			}
		case "url":
			imageURL = stringValue(source["url"])
		}
	}
	if imageURL == "" {
		imageURL = stringValue(part["url"])
	}
	if strings.TrimSpace(imageURL) == "" {
		return nil
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}}
}

// normalizeOpenAIContent renders content parts: a single text part becomes
// a plain string, otherwise the parts array.
func normalizeOpenAIContent(parts []any) any {
	if len(parts) == 1 {
		if part, ok := parts[0].(map[string]any); ok && strings.EqualFold(stringValue(part["type"]), "text") {
			return stringValue(part["text"])
		}
	}
	return parts
}

// anthropicStopSequencesToOpenAI maps stop_sequences to the chat "stop"
// field: a single sequence stays a string, multiple become an array.
func anthropicStopSequencesToOpenAI(v any) (any, bool) {
	seqs, ok := v.([]any)
	if !ok || len(seqs) == 0 {
		return nil, false
	}
	stops := make([]string, 0, len(seqs))
	for _, s := range seqs {
		if str, ok := s.(string); ok {
			stops = append(stops, str)
		}
	}
	if len(stops) == 0 {
		return nil, false
	}
	if len(stops) == 1 {
		return stops[0], true
	}
	return stops, true
}

// anthropicThinkingToEffort maps the thinking block to a reasoning effort
// (the wave-2 clamp then normalizes it per model): disabled → unset,
// enabled with a budget → budget-scaled effort, enabled without → "high".
func anthropicThinkingToEffort(v any) (string, bool) {
	thinking, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(stringValue(thinking["type"]))) {
	case "disabled":
		return "", false
	case "enabled":
		if budget, ok := thinking["budget_tokens"].(float64); ok && budget > 0 {
			return budgetToEffort(int(budget)), true
		}
		return "high", true
	case "adaptive", "auto":
		return "high", true
	default:
		return "", false
	}
}

// budgetToEffort scales an anthropic thinking budget to the reasoning
// ladder (mirrors the reference budgetToReasoningEffort).
func budgetToEffort(budget int) string {
	switch {
	case budget <= 0:
		return "none"
	case budget <= 512:
		return "minimal"
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 24576:
		return "high"
	default:
		return "xhigh"
	}
}

// anthropicToolChoiceToOpenAI maps the anthropic tool_choice to the chat
// shape: none → "none", auto → "auto", any → "required", tool → a named
// function choice.
func anthropicToolChoiceToOpenAI(v any) (any, bool) {
	if s, ok := v.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "none":
			return "none", true
		case "auto":
			return "auto", true
		case "any":
			return "required", true
		}
		return nil, false
	}
	tc, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	switch strings.ToLower(stringValue(tc["type"])) {
	case "none":
		return "none", true
	case "auto":
		return "auto", true
	case "any":
		return "required", true
	case "tool":
		name := strings.TrimSpace(stringValue(tc["name"]))
		if name == "" {
			return nil, false
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}, true
	default:
		return nil, false
	}
}

// sanitizeToolID restricts a tool call id to the charset OpenAI accepts,
// generating a fresh toolu_ id when empty (mirrors the reference
// sanitizeClaudeToolID).
func sanitizeToolID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "toolu_" + randHexString(6)
	}
	var sb strings.Builder
	for _, ch := range id {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '_', ch == '-':
			sb.WriteRune(ch)
		}
	}
	if sb.Len() == 0 {
		return "toolu_" + randHexString(6)
	}
	return sb.String()
}

// marshalJSONArgs renders a tool input object as the JSON arguments string
// (already-JSON strings pass through; empty/nil become "{}").
func marshalJSONArgs(v any) string {
	switch typed := v.(type) {
	case nil:
		return "{}"
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return "{}"
		}
		if json.Valid([]byte(trimmed)) {
			return trimmed
		}
		b, _ := json.Marshal(typed)
		return string(b)
	default:
		b, err := json.Marshal(typed)
		if err != nil || len(b) == 0 {
			return "{}"
		}
		return string(b)
	}
}

// parseJSONArgs parses a tool arguments string into a JSON object, falling
// back to an empty object when the string is not valid JSON.
func parseJSONArgs(args string) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(args) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(args), &out); err != nil {
		return map[string]any{}
	}
	if out == nil {
		return map[string]any{}
	}
	return out
}
