package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"freebuff-proxy/internal/convert"
	"freebuff-proxy/internal/phasetiming"
	"freebuff-proxy/internal/tokenestimate"
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

// handleMessages is the Anthropic /v1/messages entry point: convert the
// request to chat params, then route through chatCore with an Anthropic
// wire relay.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
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
	if rawModel == "" {
		s.writeJSONError(w, http.StatusBadRequest,
			"missing required field \"model\"; available: "+strings.Join(s.reg.Models(), ", "),
			"invalid_request_error", "model_not_found", 0)
		return
	}
	model := s.reg.ResolveModel(rawModel)
	stream := false
	if v, ok := raw["stream"].(bool); ok {
		stream = v
	}
	chatParams, err := anthropicToChatParams(raw)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"invalid messages request: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	normalized, err := convert.NormalizeRequest(chatParams, model)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	var relay relayFunc
	if stream {
		relay = func(ctx context.Context, w http.ResponseWriter, up io.Reader, stats *relayStats, chatStart time.Time) {
			s.relayAnthropicStream(ctx, w, up, stats, chatStart, rawModel)
		}
	} else {
		relay = func(ctx context.Context, w http.ResponseWriter, up io.Reader, stats *relayStats, chatStart time.Time) {
			s.relayAnthropicJSON(ctx, w, up, stats, chatStart, rawModel)
		}
	}
	s.chatCore(w, r, model, stream, normalized, convert.ExtractReasoningEffort(raw), "messages", relay)
}

// handleMessagesCountTokens answers POST /v1/messages/count_tokens with a
// local estimate of the request's input tokens. The estimate mirrors the
// FreeBuff upstream context estimator (o200k_base tokenization scaled by its
// multiplier, per-message overhead, flat image cost, structured tool
// counting) and is fully local: no session, quota, or upstream call. The
// result is an estimate for context planning, not a provider-exact token
// count — Anthropic documents its own count endpoint the same way.
func (s *Server) handleMessagesCountTokens(w http.ResponseWriter, r *http.Request) {
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
	// Decode with UseNumber so integers in tool schemas round-trip
	// byte-identically through CountJSON instead of losing precision as
	// float64 (numbers > 2^53 would otherwise shift the estimate).
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object", "invalid_request_error", "invalid_json", 0)
		return
	}
	// json.Decoder stops after the first value; reject trailing garbage the
	// way json.Unmarshal would.
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object", "invalid_request_error", "invalid_json", 0)
		return
	}
	rawModel, _ := raw["model"].(string)
	if rawModel == "" {
		s.writeJSONError(w, http.StatusBadRequest,
			"missing required field \"model\"; available: "+strings.Join(s.reg.Models(), ", "),
			"invalid_request_error", "model_not_found", 0)
		return
	}
	// Reject unknown models the way the chat path does (registry
	// ErrModelNotFound → 400 model_not_found) without acquiring a session
	// or touching upstream.
	if _, err := s.reg.AgentForModel(rawModel); err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			err.Error()+"; available: "+strings.Join(s.reg.Models(), ", "),
			"invalid_request_error", "model_not_found", 0)
		return
	}
	if s.tokenEstimator == nil {
		s.writeJSONError(w, http.StatusInternalServerError,
			"token estimator unavailable", "server_error", "estimator_unavailable", 0)
		return
	}
	count, err := s.tokenEstimator.CountAnthropicRequest(raw)
	if err != nil {
		// Document blocks get their own code: the body is valid JSON, so
		// invalid_json would mislead clients that key on the code.
		if errors.Is(err, tokenestimate.ErrDocument) {
			s.writeJSONError(w, http.StatusBadRequest,
				err.Error(), "invalid_request_error", "unsupported_content", 0)
			return
		}
		s.writeJSONError(w, http.StatusBadRequest,
			"invalid messages request: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"input_tokens": count})
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
	if v, ok := raw["user"]; ok && v != nil {
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
	return map[string]any{
		"role":         "tool",
		"tool_call_id": sanitizeToolID(toolUseID),
		"content":      anthropicToolResultContent(part["content"]),
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
	messageID       string
	model           string
	thinkingStarted bool
	thinkingIndex   int
	thinkingClosed  bool
	textStarted     bool
	textIndex       int
	textClosed      bool
	nextBlockIdx    int
	toolCalls       map[int]*anthropicToolState
	finishReason    string
	sawToolCall     bool
	usage           map[string]any

	thinkingParts []string
	textParts     []string
	toolIDs       []string
	toolIDsSeen   map[string]bool
}

// relayAnthropicStream translates the upstream chat SSE stream into
// Anthropic message events: message_start, content_block_start (thinking/
// text/tool_use), thinking_delta, text_delta, input_json_delta,
// signature_delta, content_block_stop, message_delta, message_stop.
func (s *Server) relayAnthropicStream(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time, requestedModel string) {
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

	st := &anthropicStreamState{
		messageID:    "msg_" + randHexString(10),
		model:        requestedModel,
		toolCalls:    make(map[int]*anthropicToolState),
		finishReason: "end_turn",
	}
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
					s.logger.Warn("anthropic upstream stream error", "err", lc.err)
					s.finalizeAnthropicStream(send, st)
				}
				return
			}
			if lc.done {
				s.finalizeAnthropicStream(send, st)
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
			relayed = time.Now()
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
			s.accumulateAnthropicChunk(send, st, chunk)
		}
	}
}

// sendAnthropicMessageStart emits the message_start event.
func sendAnthropicMessageStart(send func(map[string]any), st *anthropicStreamState) {
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
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// accumulateAnthropicChunk translates one upstream chat chunk into
// Anthropic content events.
func (s *Server) accumulateAnthropicChunk(send func(map[string]any), st *anthropicStreamState, chunk map[string]any) {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
	if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
		st.setStopReason(fr)
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return
	}
	// Reasoning deltas → thinking block.
	if reasoning, ok := firstStringOf(delta, "reasoning_content", "reasoning", "reasoning_text", "thinking"); ok && reasoning != "" {
		st.thinkingParts = append(st.thinkingParts, reasoning)
		st.ensureThinking(send)
		send(map[string]any{
			"type":  "content_block_delta",
			"index": st.thinkingIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
		})
	}
	// Content deltas → text block (closes an open thinking block).
	if content, ok := delta["content"].(string); ok && content != "" {
		st.textParts = append(st.textParts, content)
		st.ensureText(send)
		send(map[string]any{
			"type":  "content_block_delta",
			"index": st.textIndex,
			"delta": map[string]any{"type": "text_delta", "text": content},
		})
	}
	// Tool-call fragments → tool_use blocks.
	if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
		st.closeText(send)
		st.sawToolCall = true
		for _, raw := range tcs {
			tc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			upIdx := 0
			if i, ok := numFloat64(tc["index"]); ok {
				upIdx = int(i)
			}
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
			fn, _ := tc["function"].(map[string]any)
			if fn == nil {
				continue
			}
			if name, ok := fn["name"].(string); ok && name != "" && ts.name == "" {
				ts.name = name
				ts.ensureStarted(send)
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				ts.ensureStarted(send)
				send(map[string]any{
					"type":  "content_block_delta",
					"index": ts.index,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
				})
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
	stopReason := st.finishReason
	if st.sawToolCall {
		stopReason = "tool_use"
	}
	usagePayload := map[string]any{"output_tokens": 0}
	if st.usage != nil {
		usagePayload = st.usage
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
func (st *anthropicStreamState) ensureThinking(send func(map[string]any)) {
	if st.thinkingStarted {
		return
	}
	st.thinkingIndex = st.nextBlockIdx
	st.nextBlockIdx++
	st.thinkingStarted = true
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
// signature — the upstream never emits signatures).
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
// (review P2 — some GLM/DeepSeek outputs interleave trailing text after
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
// known.
func (ts *anthropicToolState) ensureStarted(send func(map[string]any)) {
	if ts.started {
		return
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
		st.finishReason = "tool_use"
		st.sawToolCall = true
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

// --- non-streaming translation ---

// relayAnthropicJSON drains the upstream stream and writes one Anthropic
// message object. On any decode/stream error a 502 is returned.
func (s *Server) relayAnthropicJSON(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time, requestedModel string) {
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
	var completion map[string]any
	if err := json.Unmarshal(acc.Finish(), &completion); err != nil {
		s.writeJSONError(w, http.StatusBadGateway,
			"failed to decode upstream stream: "+err.Error(), "upstream_error", "upstream_unavailable", 0)
		return
	}
	msgObj := anthropicMessageFromCompletion(completion, requestedModel)
	out, err := json.Marshal(msgObj)
	if err != nil {
		s.writeJSONError(w, http.StatusBadGateway,
			"failed to build response: "+err.Error(), "upstream_error", "upstream_unavailable", 0)
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
// an accumulated chat.completion.
func anthropicMessageFromCompletion(completion map[string]any, requestedModel string) map[string]any {
	id, _ := completion["id"].(string)
	if id == "" {
		id = "msg_" + randHexString(10)
	}
	model, _ := completion["model"].(string)
	if model == "" {
		model = requestedModel
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
		choice, _ := choices[0].(map[string]any)
		msg, _ := choice["message"].(map[string]any)
		if msg != nil {
			content := []any{}
			if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
				content = append(content, map[string]any{"type": "thinking", "thinking": rc})
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
	if message["stop_reason"] == "end_turn" && hasToolCall {
		message["stop_reason"] = "tool_use"
	}
	if usage, ok := completion["usage"].(map[string]any); ok {
		message["usage"] = openAIUsageToAnthropic(usage)
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
