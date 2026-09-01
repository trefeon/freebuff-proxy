package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Non-streaming accumulator (ported from freebuff-api-kiprana
// CompletionAccumulator).
// ---------------------------------------------------------------------------

// toolCall is one assembled tool call: id/type/function.name come from the
// first fragment, function.arguments is concatenated across fragments.
type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Accumulator assembles a non-streaming chat.completion response from
// upstream SSE lines. It is not safe for concurrent use (one stream, one
// accumulator).
type Accumulator struct {
	id                string
	created           int64
	model             string
	requestedModel    string // when set, overrides upstream model in Finish()
	contentParts      []string
	reasoningParts    []string
	finishReason      string
	usage             any
	systemFingerprint string
	toolCalls         map[int]*toolCall
	opts              Options
}

// NewAccumulator returns an accumulator with a fresh chatcmpl- id and
// created timestamp; model/id/created are refined by the first chunks seen.
func NewAccumulator() *Accumulator {
	return NewAccumulatorOpts(DefaultOptions())
}

// NewAccumulatorOpts is NewAccumulator with an explicit Options (issue
// #277): the reasoning-in-content fold mode for Finish is taken from opts
// instead of the process environment.
func NewAccumulatorOpts(opts Options) *Accumulator {
	return &Accumulator{
		id:        "chatcmpl-" + randHex(16),
		created:   time.Now().Unix(),
		toolCalls: make(map[int]*toolCall),
		opts:      opts,
	}
}

// SetRequestedModel overrides the model field in the final response.
// Use when the upstream returns a different model than what was requested
// (e.g. upstream pins to MIMO but client asked for DeepSeek).
func (a *Accumulator) SetRequestedModel(model string) {
	a.requestedModel = model
}

// Add parses one SSE data line (with or without "data: " prefix; [DONE] and
// non-data lines are ignored) and accumulates its content into the response.
func (a *Accumulator) Add(line []byte) error {
	data, ok := parseSSEData(line)
	if !ok {
		return nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	var chunk map[string]any
	if err := json.Unmarshal(data, &chunk); err != nil {
		return fmt.Errorf("convert: invalid chunk JSON: %w", err)
	}
	if errVal, ok := chunk["error"]; ok && errVal != nil {
		if errMap, ok := errVal.(map[string]any); ok {
			if msg, ok := errMap["message"].(string); ok && msg != "" {
				return fmt.Errorf("upstream error: %s", msg)
			}
		} else if errStr, ok := errVal.(string); ok && errStr != "" {
			return fmt.Errorf("upstream error: %s", errStr)
		}
		return fmt.Errorf("upstream error: %v", errVal)
	}
	a.accumulate(chunk)
	return nil
}

func (a *Accumulator) accumulate(chunk map[string]any) {
	if id, ok := chunk["id"].(string); ok && id != "" {
		a.id = id
	}
	if created, ok := numInt64(chunk["created"]); ok && created > 0 {
		a.created = created
	}
	if model, ok := chunk["model"].(string); ok && model != "" {
		a.model = model
	}
	if usage, ok := chunk["usage"]; ok && usage != nil {
		a.usage = usage
	}
	if fp, ok := chunk["system_fingerprint"].(string); ok && fp != "" {
		a.systemFingerprint = fp
	}
	for _, c := range choicesOf(chunk) {
		delta, _ := c["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok {
			a.contentParts = append(a.contentParts, content)
		}
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			a.reasoningParts = append(a.reasoningParts, rc)
		} else if r, ok := delta["reasoning"].(string); ok && r != "" {
			a.reasoningParts = append(a.reasoningParts, r)
		}
		for _, tc := range toolCallsOf(delta) {
			a.addToolCall(tc)
		}
		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			a.finishReason = fr
		}
	}
}

// addToolCall stitches one tool-call fragment by index: id/type/name are
// taken from the first fragment that provides them, arguments are appended
// across fragments.
func (a *Accumulator) addToolCall(tc map[string]any) {
	index := 0
	if i, ok := numInt64(tc["index"]); ok {
		index = int(i)
	}
	cur, ok := a.toolCalls[index]
	if !ok {
		cur = &toolCall{ID: "call_" + randHex(12), Type: "function"}
		a.toolCalls[index] = cur
	}
	if id, ok := tc["id"].(string); ok && id != "" {
		cur.ID = id
	}
	if typ, ok := tc["type"].(string); ok && typ != "" {
		cur.Type = typ
	}
	if fn, ok := tc["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok && name != "" {
			cur.Function.Name = name
		}
		if args, ok := fn["arguments"].(string); ok && args != "" {
			cur.Function.Arguments += args
		}
	}
}

// Finish returns the assembled chat.completion response as compact JSON:
// content and reasoning_content are concatenated across chunks, tool_calls
// are stitched by index and sorted, finish_reason is the last non-empty one
// seen ("stop" when none), and usage is the last one seen (zeroed when none).
func (a *Accumulator) Finish() []byte {
	content := strings.Join(a.contentParts, "")
	// Issue #44: fold reasoning into message content for clients that don't
	// render the reasoning channel (same mode toggle as the streaming path).
	if tag := a.opts.ReasoningInContent; tag != "" {
		if rc := strings.Join(a.reasoningParts, ""); rc != "" {
			content = "<" + tag + ">" + rc + "</" + tag + ">" + content
		}
	}
	// If native toolCalls are empty, try extracting any inline XML tool calls
	// that were emitted into content (e.g. from Hermes/Qwen/MiMo models).
	if len(a.toolCalls) == 0 {
		var extracted []*toolCall
		content, extracted = extractXMLToolCalls(content)
		if len(extracted) == 0 && len(a.reasoningParts) > 0 {
			// Fallback: model might have emitted tool_call inside reasoning_content (smallcode finding / Tau2 fix)
			reasoningFull := strings.Join(a.reasoningParts, "")
			var cleanedReasoning string
			cleanedReasoning, extracted = extractXMLToolCalls(reasoningFull)
			if len(extracted) > 0 {
				a.reasoningParts = []string{cleanedReasoning}
			}
		}
		for idx, tc := range extracted {
			a.toolCalls[idx] = tc
		}
	}
	// Scrub dangling tool XML tags from content
	content = danglingToolTagsRe.ReplaceAllString(content, "")
	content = strings.TrimSpace(content)

	var msgContent any = content
	if len(a.toolCalls) > 0 && content == "" {
		msgContent = nil
	}
	msg := map[string]any{
		"role":    "assistant",
		"content": msgContent,
		"refusal": nil,
	}
	if len(a.toolCalls) > 0 {
		keys := make([]int, 0, len(a.toolCalls))
		for k := range a.toolCalls {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		calls := make([]any, 0, len(keys))
		for _, k := range keys {
			calls = append(calls, a.toolCalls[k])
		}
		msg["tool_calls"] = calls
	}
	if rc := strings.Join(a.reasoningParts, ""); rc != "" {
		msg["reasoning_content"] = rc
	}
	usage := a.usage
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	finish := a.finishReason
	if finish == "" {
		if len(a.toolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	resp := map[string]any{
		"id":      a.id,
		"object":  "chat.completion",
		"created": a.created,
		"model":   a.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"logprobs":      nil,
			"finish_reason": finish,
		}},
		"usage": usage,
	}
	// Override model when client requested a different one than upstream served.
	if a.requestedModel != "" {
		resp["model"] = a.requestedModel
	}
	if a.systemFingerprint != "" {
		resp["system_fingerprint"] = a.systemFingerprint
	}
	// Values came from encoding/json, so marshaling cannot fail.
	b, _ := json.Marshal(resp)
	return b
}
