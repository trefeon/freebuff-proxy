package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// SSE framing (ported from freebuff-api-kiprana sse.py encode_sse).
// ---------------------------------------------------------------------------

// EncodeSSE renders one SSE frame: "data: " + data (callers pass compact
// JSON from json.Marshal) + "\n\n".
func EncodeSSE(data []byte) []byte {
	out := append([]byte("data: "), data...)
	out = append(out, '\n', '\n')
	return out
}

// DONE is the "[DONE]" SSE frame that terminates a stream.
var DONE = []byte("data: [DONE]\n\n")

// ErrorChunk renders an SSE error frame with the OpenAI error shape:
// data: {"error":{"message":m,"type":"upstream_error","code":c}}\n\n
// The code field is omitted when empty. The caller appends DONE after it.
func ErrorChunk(message, code string) []byte {
	err := map[string]any{"message": message, "type": "upstream_error"}
	if code != "" {
		err["code"] = code
	}
	b, _ := json.Marshal(map[string]any{"error": err})
	return EncodeSSE(b)
}

// parseSSEData extracts the payload from one SSE data line. It accepts both
// "data: {...}" and plain "{...}"; blank lines, comment lines (": ..."), and
// other SSE fields (event:/id:/retry:) are skipped. Returns (payload, true)
// when a payload is present.
func parseSSEData(line []byte) ([]byte, bool) {
	s := bytes.TrimSpace(line)
	if len(s) == 0 || s[0] == ':' {
		return nil, false // blank or comment line
	}
	if bytes.HasPrefix(s, []byte("data:")) {
		s = bytes.TrimSpace(s[len("data:"):])
		if len(s) == 0 {
			return nil, false
		}
		return s, true
	}
	if s[0] == '{' {
		return s, true // plain JSON (chunks are objects)
	}
	return nil, false // other SSE fields (event:, id:, retry:)
}

// chunkMapPool reuses the top-level decode map across SanitizeChunk calls.
// The pool hands out exclusive ownership and maps are cleared on reuse, so
// concurrent streams are safe.
var chunkMapPool = sync.Pool{
	New: func() any { return make(map[string]any, 8) },
}

// cleanMapPool reuses the sanitized output map across SanitizeChunk calls.
var cleanMapPool = sync.Pool{
	New: func() any { return make(map[string]any, 5) },
}

// SanitizeChunk cleans ONE upstream SSE data line (with or without the
// "data: " prefix) for relay to the client:
//
//   - chunks with no choices AND no usage are dropped (returns drop=true)
//   - id/object/created/model are ensured (default id "chatcmpl-"+hex,
//     object "chat.completion.chunk", created now, model "")
//   - reasoning_content stays in delta as its own key and is never merged
//     into content (unless REASONING_IN_CONTENT is enabled, issue #44);
//     an explicit null content is removed
//   - system_fingerprint/logprobs/usage/finish_reason and the optional
//     service_tier/obfuscation/moderation enrichment keys pass through
//
// Output is compact JSON. Malformed or non-JSON lines are dropped.
//
// Chunks that already satisfy every invariant are returned WITHOUT
// re-encoding: the returned slice then aliases line (a zero-allocation fast
// path), so callers must copy it if they retain it past the current
// processing step.
//
// Ported from freebuff-api-kiprana sanitize_stream_chunk.
func SanitizeChunk(line []byte) ([]byte, bool) {
	return SanitizeChunkOpts(line, DefaultOptions())
}

// SanitizeChunkOpts is SanitizeChunk with an explicit Options (issue #277):
// the reasoning-in-content fold mode is taken from opts instead of the
// process environment.
func SanitizeChunkOpts(line []byte, opts Options) ([]byte, bool) {
	data, ok := parseSSEData(line)
	if !ok {
		return nil, true
	}
	chunk := chunkMapPool.Get().(map[string]any)
	clear(chunk)
	if err := json.Unmarshal(data, &chunk); err != nil {
		chunkMapPool.Put(chunk)
		return nil, true
	}
	// Fast path: the chunk is already in sanitized shape, so re-encoding it
	// would be a no-op. Emit the raw payload (a subslice of line) untouched.
	// The reasoning-in-content fold changes output, so it disables the path.
	if opts.ReasoningInContent == "" && !needsSanitize(chunk) {
		chunkMapPool.Put(chunk)
		return data, false
	}
	clean := cleanMapPool.Get().(map[string]any)
	result := sanitizeChunk(chunk, clean, opts.ReasoningInContent)
	chunkMapPool.Put(chunk)
	if result == nil {
		clear(clean)
		cleanMapPool.Put(clean)
		return nil, true
	}
	out, err := json.Marshal(result)
	clear(result)
	cleanMapPool.Put(result)
	if err != nil {
		return nil, true
	}
	return out, false
}

// sanitizeChunk implements the per-chunk cleanup into the pooled clean map;
// returns nil to drop the chunk. clean is cleared by the caller on drop.
func sanitizeChunk(chunk map[string]any, clean map[string]any, reasoningTag string) map[string]any {
	if errVal, hasErr := chunk["error"]; hasErr && errVal != nil {
		clear(clean)
		if id, ok := chunk["id"].(string); ok && id != "" {
			clean["id"] = id
		}
		if obj, ok := chunk["object"].(string); ok && obj != "" {
			clean["object"] = obj
		}
		if created, ok := numInt64(chunk["created"]); ok && created > 0 {
			clean["created"] = created
		}
		if model, ok := chunk["model"].(string); ok && model != "" {
			clean["model"] = model
		}
		if errMap, ok := errVal.(map[string]any); ok {
			cleanErr := make(map[string]any, len(errMap))
			for k, v := range errMap {
				cleanErr[k] = v
			}
			if _, ok := cleanErr["message"]; !ok {
				cleanErr["message"] = "upstream error"
			}
			if _, ok := cleanErr["type"]; !ok {
				cleanErr["type"] = "upstream_error"
			}
			clean["error"] = cleanErr
		} else if errStr, ok := errVal.(string); ok {
			clean["error"] = map[string]any{
				"message": errStr,
				"type":    "upstream_error",
			}
		} else {
			clean["error"] = map[string]any{
				"message": fmt.Sprintf("%v", errVal),
				"type":    "upstream_error",
			}
		}
		return clean
	}

	clear(clean)
	if id, ok := chunk["id"].(string); ok && id != "" {
		clean["id"] = id
	} else {
		clean["id"] = "chatcmpl-" + randHex(16)
	}
	if obj, ok := chunk["object"].(string); ok && obj != "" {
		clean["object"] = obj
	} else {
		clean["object"] = "chat.completion.chunk"
	}
	if created, ok := numInt64(chunk["created"]); ok && created > 0 {
		clean["created"] = created
	} else {
		clean["created"] = time.Now().Unix()
	}
	if model, ok := chunk["model"].(string); ok {
		clean["model"] = model
	} else {
		clean["model"] = ""
	}
	clean["choices"] = []any{}
	if fp, ok := chunk["system_fingerprint"].(string); ok && fp != "" {
		clean["system_fingerprint"] = fp
	}
	// Spec-valid optional enrichment keys pass through when present (null
	// values are dropped like null usage/logprobs): service_tier,
	// obfuscation (stream_options.include_obfuscation), moderation results.
	for _, k := range []string{"service_tier", "obfuscation", "moderation"} {
		if v, ok := chunk[k]; ok && v != nil {
			clean[k] = v
		}
	}
	if usage, ok := chunk["usage"]; ok && usage != nil {
		clean["usage"] = usage
	}
	for _, c := range choicesOf(chunk) {
		item := make(map[string]any, 4)
		index := 0
		if i, ok := numInt64(c["index"]); ok {
			index = int(i)
		}
		item["index"] = index
		delta := make(map[string]any)
		if d, ok := c["delta"].(map[string]any); ok {
			for k, v := range d {
				delta[k] = v
			}
		}
		var reasoningStr string
		if rc, ok := delta["reasoning_content"]; ok {
			delete(delta, "reasoning_content")
			if s, isStr := rc.(string); isStr {
				reasoningStr = s
			}
		}
		if r, ok := delta["reasoning"]; ok {
			delete(delta, "reasoning")
			if s, isStr := r.(string); isStr && reasoningStr == "" {
				reasoningStr = s
			}
		}
		if reasoningStr != "" {
			delta["reasoning_content"] = reasoningStr
			delta["reasoning"] = reasoningStr
			// Issue #44: fold reasoning into content as <think> text for
			// clients that don't render a reasoning channel. reasoning_details
			// is never folded.
			foldReasoningIntoContent(delta, reasoningStr, reasoningTag)
		}
		if v, ok := delta["content"]; ok && v == nil {
			delete(delta, "content")
		}
		item["delta"] = delta
		fr, hasFR := c["finish_reason"]
		if !hasFR {
			fr = nil
		}
		item["finish_reason"] = fr
		if lp, ok := c["logprobs"]; ok && lp != nil {
			item["logprobs"] = lp
		}
		clean["choices"] = append(clean["choices"].([]any), item)
	}
	if len(clean["choices"].([]any)) == 0 && clean["usage"] == nil {
		return nil
	}
	return clean
}

// needsSanitize reports whether sanitizeChunk would change the decoded chunk
// in any way. When it returns false the chunk is already in output shape and
// can be relayed verbatim (the SanitizeChunk zero-allocation fast path).
func needsSanitize(chunk map[string]any) bool {
	if _, hasErr := chunk["error"]; hasErr {
		return true // error chunks are rebuilt entirely
	}
	id, ok := chunk["id"].(string)
	if !ok || id == "" {
		return true // default id injected
	}
	if obj, _ := chunk["object"].(string); obj != "chat.completion.chunk" {
		return true
	}
	cv, ok := chunk["created"]
	if !ok || !isJSONInteger(cv) {
		return true
	}
	if c, _ := numInt64(cv); c <= 0 {
		return true // default created injected
	}
	if _, ok := chunk["model"].(string); !ok {
		return true
	}
	choices, ok := chunk["choices"].([]any)
	if !ok {
		return true
	}
	usage, hasUsage := chunk["usage"]
	if hasUsage && usage == nil {
		return true // null usage is dropped
	}
	if len(choices) == 0 && (!hasUsage || usage == nil) {
		return true // the chunk would be dropped outright
	}
	if fp, hasFP := chunk["system_fingerprint"]; hasFP {
		if s, ok := fp.(string); !ok || s == "" {
			return true
		}
	}
	for k := range chunk {
		switch k {
		case "id", "object", "created", "model", "choices", "system_fingerprint", "usage",
			"service_tier", "obfuscation", "moderation":
		default:
			return true // unknown top-level keys are dropped
		}
	}
	for _, raw := range choices {
		c, ok := raw.(map[string]any)
		if !ok {
			return true // non-object choices are dropped
		}
		idx, ok := c["index"]
		if !ok || !isJSONInteger(idx) {
			return true // default index 0 injected / fraction truncated
		}
		delta, ok := c["delta"].(map[string]any)
		if !ok {
			return true // empty delta injected
		}
		if content, has := delta["content"]; has && content == nil {
			return true // null content removed
		}
		hasRC := false
		if rc, has := delta["reasoning_content"]; has {
			if _, isStr := rc.(string); !isStr {
				return true // non-string reasoning_content dropped
			}
			hasRC = true
		}
		hasR := false
		if r, has := delta["reasoning"]; has {
			if _, isStr := r.(string); !isStr {
				return true // non-string reasoning dropped
			}
			hasR = true
		}
		if hasRC != hasR {
			return true // normalize to both reasoning_content and reasoning
		}
		if _, ok := c["finish_reason"]; !ok {
			return true // explicit null finish_reason injected
		}
		if lp, has := c["logprobs"]; has && lp == nil {
			return true // null logprobs dropped
		}
		for k := range c {
			switch k {
			case "index", "delta", "finish_reason", "logprobs":
			default:
				return true // unknown choice keys are dropped
			}
		}
	}
	return false
}

// isJSONInteger reports whether v is a JSON number whose sanitize conversion
// round-trips: integral AND representable as int64 (encoding/json decodes
// numbers as float64; sanitize truncates fractions via numInt64). A value
// like 1e20 is integral but saturates int64, so it must still take the
// sanitize path.
func isJSONInteger(v any) bool {
	switch n := v.(type) {
	case float64:
		return n == math.Trunc(n) && n == float64(int64(n))
	case int, int64:
		return true
	}
	return false
}

// numInt64 extracts an integer from a JSON-decoded number.
func numInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

// choicesOf returns the object entries of a chunk's "choices" array.
func choicesOf(chunk map[string]any) []map[string]any {
	raw, _ := chunk["choices"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// toolCallsOf returns the object entries of a delta's "tool_calls" array.
func toolCallsOf(delta map[string]any) []map[string]any {
	raw, _ := delta["tool_calls"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// StripEndTurnToolCalls removes any tool_call whose function.name is "end_turn"
// from every choice in chunk. It returns whether non-end_turn tool calls remain
// and the effective finish reason. If no tool calls remain and finish_reason was
// "tool_calls", finish_reason is flipped to "stop" in-place.
//
// Every relay must strip end_turn before downstream emission; the Anthropic
// relays implement the strip in their own state machines rather than via
// this helper.
func StripEndTurnToolCalls(chunk map[string]any) (toolCallsRemaining bool, finishReason string) {
	anyRemaining := false
	for _, choice := range choicesOf(chunk) {
		// Read finish_reason from the choice (not top-level).
		if fr, ok := choice["finish_reason"].(string); ok {
			finishReason = fr
		}
		// Streaming shape: delta.tool_calls
		if delta, ok := choice["delta"].(map[string]any); ok {
			if tcs, ok := delta["tool_calls"].([]any); ok {
				filtered := filterEndTurn(tcs)
				if len(filtered) == 0 {
					delete(delta, "tool_calls")
				} else {
					delta["tool_calls"] = filtered
					anyRemaining = true
				}
			}
		}
		// Non-streaming shape: message.tool_calls
		if msg, ok := choice["message"].(map[string]any); ok {
			if tcs, ok := msg["tool_calls"].([]any); ok {
				filtered := filterEndTurn(tcs)
				if len(filtered) == 0 {
					delete(msg, "tool_calls")
				} else {
					msg["tool_calls"] = filtered
					anyRemaining = true
				}
			}
		}
	}
	toolCallsRemaining = anyRemaining
	if !anyRemaining && finishReason == "tool_calls" {
		finishReason = "stop"
		// Flip finish_reason in the choice, not top-level.
		for _, choice := range choicesOf(chunk) {
			choice["finish_reason"] = finishReason
		}
	}
	return
}

// filterEndTurn removes tool_call entries whose function.name is "end_turn"
// from a []any slice, preserving the original element type for downstream
// type assertions ([]any, not []map[string]any).
func filterEndTurn(tcs []any) []any {
	out := make([]any, 0, len(tcs))
	for _, raw := range tcs {
		tc, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		if name, _ := fn["name"].(string); name == "end_turn" {
			continue
		}
		out = append(out, raw)
	}
	return out
}
