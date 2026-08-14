// Package convert implements pure OpenAI request/response normalization for
// the freebuff-proxy bridge.
//
// It performs no I/O and depends on no other internal package. Everything
// here is a pure function over JSON: request sanitization (parameter
// whitelist, developer→system role rewrite, tool-schema normalization), SSE
// frame encoding, per-chunk stream sanitization, and the non-streaming
// response accumulator. Envelope injection (codebuff_metadata, forced
// stream, x-freebuff headers) is deliberately out of scope — that lives in
// internal/upstream.
package convert

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// maxSchemaDepth caps recursive JSON-schema normalization. Ported from
// proxy-freebuff's normalizeSchemaMap, which resolves with depth 12.
const maxSchemaDepth = 12

// upstreamKeys is the whitelist of chat-completions body keys forwarded to
// the upstream, plus messages/model which are always kept. Ported from
// freebuff-api-kiprana's _UPSTREAM_CHAT_KEYS. Note "stream" is NOT
// whitelisted: the upstream layer forces stream:true itself.
var upstreamKeys = map[string]bool{
	"frequency_penalty":     true,
	"logit_bias":            true,
	"logprobs":              true,
	"max_completion_tokens": true,
	"max_tokens":            true,
	"metadata":              true,
	"modalities":            true,
	"parallel_tool_calls":   true,
	"presence_penalty":      true,
	"reasoning_effort":      true,
	"response_format":       true,
	"seed":                  true,
	"service_tier":          true,
	"stop":                  true,
	"store":                 true,
	"stream_options":        true,
	"temperature":           true,
	"tool_choice":           true,
	"tools":                 true,
	"top_logprobs":          true,
	"top_p":                 true,
	"user":                  true,
}

var fallbackCounter atomic.Uint64

// randHex returns n random bytes hex-encoded (16 bytes → the 32 hex chars of
// a uuid4 hex, 12 bytes → 24 hex chars). Falls back to time+counter if
// crypto/rand fails, which practically never happens.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("%x%x", time.Now().UnixNano(), fallbackCounter.Add(1))
}

// NormalizeRequest sanitizes a client OpenAI chat-completions request body:
//
//   - keeps ONLY the whitelisted upstream keys (plus messages and model);
//     null-valued whitelisted keys are dropped (they are meaningless upstream,
//     matching kiprana's "value is not None" filter)
//   - converts message role "developer" to "system"
//   - normalizes tool JSON schemas (bare $ref/$defs resolution, nullable
//     anyOf/oneOf simplification, type/enum/const cleanup, depth cap 12)
//
// The returned bytes are compact JSON. Errors only occur on invalid JSON or
// a non-object body.
func NormalizeRequest(body []byte) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload == nil {
		return nil, fmt.Errorf("convert: request body must be a JSON object")
	}
	out := make(map[string]any, len(payload))
	for key, value := range payload {
		if key == "model" || key == "messages" {
			out[key] = value
			continue
		}
		if upstreamKeys[key] && value != nil {
			out[key] = value
		}
	}
	normalizeRoles(out)
	normalizeToolSchemas(out)
	return json.Marshal(out)
}

// normalizeRoles rewrites message role "developer" to "system" in place.
func normalizeRoles(payload map[string]any) {
	msgs, _ := payload["messages"].([]any)
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "developer" {
			msg["role"] = "system"
		}
	}
}

// ---------------------------------------------------------------------------
// Tool-schema normalization (ported from proxy-freebuff/lib/convert.js,
// normalizeToolSchemas / normalizeSchemaMap, lines ~40-154).
// ---------------------------------------------------------------------------

// normalizeToolSchemas normalizes fn.parameters for every function tool in
// the payload, in place.
func normalizeToolSchemas(payload map[string]any) {
	tools, _ := payload["tools"].([]any)
	if len(tools) == 0 {
		return
	}
	hasEndTurn := false
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, ok := fn["name"].(string); ok && name == "end_turn" {
			hasEndTurn = true
		}
		params, ok := fn["parameters"].(map[string]any)
		if !ok {
			continue
		}
		fn["parameters"] = normalizeSchemaMap(params, extractDefinitions(params), maxSchemaDepth)
	}
	// Inject end_turn tool definition to pass Codebuff's foreign_toolset validation
	if !hasEndTurn {
		payload["tools"] = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "end_turn",
				"description": "Signal the end of the current task.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		})
	}
}

// extractDefinitions merges a schema node's "definitions" and "$defs" maps
// ($defs wins on name collision, matching JS Object.assign order). Returns
// nil when neither exists.
func extractDefinitions(node map[string]any) map[string]any {
	var merged map[string]any
	if defs, ok := node["definitions"].(map[string]any); ok {
		merged = make(map[string]any, len(defs))
		for k, v := range defs {
			merged[k] = v
		}
	}
	if defs, ok := node["$defs"].(map[string]any); ok {
		if merged == nil {
			merged = make(map[string]any, len(defs))
		}
		for k, v := range defs {
			merged[k] = v
		}
	}
	return merged
}

// mergeDefinitions combines a parent definition table with one extracted
// from a nested node; the local table wins on collision.
func mergeDefinitions(parent, local map[string]any) map[string]any {
	if parent == nil {
		return local
	}
	if local == nil {
		return parent
	}
	merged := make(map[string]any, len(parent)+len(local))
	for k, v := range parent {
		merged[k] = v
	}
	for k, v := range local {
		merged[k] = v
	}
	return merged
}

// normalizeSchemaMap normalizes one JSON-schema node: resolves bare $ref
// nodes against the definition table, recurses into values (depth-capped),
// drops definitions/$defs/nullable, simplifies nullable anyOf/oneOf, and
// cleans up type/enum/const fields. The returned map is always freshly
// allocated except at the depth cap, where the node is returned as-is.
func normalizeSchemaMap(node map[string]any, defs map[string]any, maxDepth int) map[string]any {
	if maxDepth <= 0 {
		return node // depth cap: leave the remaining structure untouched
	}
	defs = mergeDefinitions(defs, extractDefinitions(node))
	if replaced, ok := tryResolveRef(node, defs); ok {
		if resolved, isMap := replaced.(map[string]any); isMap {
			return normalizeSchemaMap(resolved, defs, maxDepth-1)
		}
		return node
	}
	normalized := make(map[string]any, len(node))
	for key, value := range node {
		normalized[key] = normalizeSchemaValue(value, defs, maxDepth-1)
	}
	delete(normalized, "definitions")
	delete(normalized, "$defs")
	delete(normalized, "nullable")
	normalized = simplifyNullableCombinator(normalized, "anyOf")
	normalized = simplifyNullableCombinator(normalized, "oneOf")
	normalizeTypeField(normalized)
	normalizeEnumField(normalized)
	normalizeConstField(normalized)
	return normalized
}

// normalizeSchemaValue recurses into arrays and objects; scalars pass
// through untouched.
func normalizeSchemaValue(value any, defs map[string]any, maxDepth int) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = normalizeSchemaValue(e, defs, maxDepth)
		}
		return out
	case map[string]any:
		return normalizeSchemaMap(v, defs, maxDepth)
	default:
		return value
	}
}

// tryResolveRef resolves a node that is a BARE {"$ref": "..."} (no sibling
// keys) against the definition table. Returns (replacement, true) on
// success; the replacement is a deep clone. Ported from JS tryResolveRef:
// only "#/definitions/<name>" and "#/$defs/<name>" pointers resolve, and
// only when the name exists in the table.
func tryResolveRef(node map[string]any, defs map[string]any) (any, bool) {
	ref, _ := node["$ref"].(string)
	if ref == "" || len(node) != 1 || defs == nil {
		return nil, false
	}
	var name string
	switch {
	case strings.HasPrefix(ref, "#/definitions/"):
		name = ref[len("#/definitions/"):]
	case strings.HasPrefix(ref, "#/$defs/"):
		name = ref[len("#/$defs/"):]
	}
	if name == "" {
		return nil, false
	}
	resolved, ok := defs[name]
	if !ok {
		return nil, false
	}
	return cloneValue(resolved), true
}

// cloneValue deep-clones any JSON value (maps, arrays, scalars).
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = cloneValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = cloneValue(val)
		}
		return out
	default:
		return v
	}
}

// isNullSchema reports whether a sub-schema only admits null: type "null",
// const null, or enum [null]. Ported from JS isNullSchema.
func isNullSchema(schema map[string]any) bool {
	if t, _ := schema["type"].(string); t == "null" {
		return true
	}
	if c, ok := schema["const"]; ok && c == nil {
		return true
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) == 1 && enum[0] == nil {
		return true
	}
	return false
}

// simplifyNullableCombinator rewrites an anyOf/oneOf that contains null-only
// sub-schemas: null options are removed; with one option left the combinator
// is inlined (its option's keys override the schema's other keys); with zero
// left the key is dropped. Non-object entries (e.g. a bare null literal) are
// kept, matching the JS filter. Ported from JS simplifyNullableCombinator.
func simplifyNullableCombinator(schema map[string]any, key string) map[string]any {
	raw, ok := schema[key].([]any)
	if !ok {
		return schema
	}
	filtered := make([]any, 0, len(raw))
	for _, o := range raw {
		if m, isMap := o.(map[string]any); isMap && isNullSchema(m) {
			continue
		}
		filtered = append(filtered, o)
	}
	switch {
	case len(filtered) == 0:
		delete(schema, key)
	case len(filtered) == 1:
		if single, isMap := filtered[0].(map[string]any); isMap {
			merged := make(map[string]any, len(schema)+len(single))
			for k, v := range schema {
				if k != key {
					merged[k] = v
				}
			}
			for k, v := range single {
				merged[k] = v
			}
			return merged
		}
		schema[key] = filtered
	default:
		schema[key] = filtered
	}
	return schema
}

// normalizeTypeField reduces a string-array "type" to its first non-null
// entry, or drops the key when none remains. Non-string entries are dropped.
// Ported from JS normalizeTypeField.
func normalizeTypeField(schema map[string]any) {
	raw, ok := schema["type"].([]any)
	if !ok {
		return
	}
	var nonNull []string
	for _, t := range raw {
		if s, isStr := t.(string); isStr && strings.TrimSpace(s) != "" && s != "null" {
			nonNull = append(nonNull, s)
		}
	}
	if len(nonNull) == 0 {
		delete(schema, "type")
	} else {
		schema["type"] = nonNull[0]
	}
}

// normalizeEnumField removes null entries and duplicates from an "enum"
// array, dropping the key entirely when nothing remains. Dedupe keys combine
// Go type with JSON encoding, mirroring JS's `${typeof}:${JSON.stringify}`.
// Ported from JS normalizeEnumField.
func normalizeEnumField(schema map[string]any) {
	raw, ok := schema["enum"].([]any)
	if !ok {
		return
	}
	seen := make(map[string]bool, len(raw))
	filtered := make([]any, 0, len(raw))
	for _, entry := range raw {
		if entry == nil {
			continue
		}
		key := fmt.Sprintf("%T:%s", entry, jsonRepr(entry))
		if seen[key] {
			continue
		}
		seen[key] = true
		filtered = append(filtered, entry)
	}
	if len(filtered) == 0 {
		delete(schema, "enum")
	} else {
		schema["enum"] = filtered
	}
}

// normalizeConstField drops a const:null key. Ported from JS normalizeConstField.
func normalizeConstField(schema map[string]any) {
	if c, ok := schema["const"]; ok && c == nil {
		delete(schema, "const")
	}
}

// jsonRepr marshals a JSON-decoded value; it cannot fail on values that came
// from encoding/json, and the fallback keeps the dedupe key deterministic.
func jsonRepr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

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

// SanitizeChunk cleans ONE upstream SSE data line (with or without the
// "data: " prefix) for relay to the client:
//
//   - chunks with no choices AND no usage are dropped (returns drop=true)
//   - id/object/created/model are ensured (default id "chatcmpl-"+hex,
//     object "chat.completion.chunk", created now, model "")
//   - reasoning_content stays in delta as its own key and is never merged
//     into content; an explicit null content is removed
//   - system_fingerprint/logprobs/usage/finish_reason pass through
//
// Output is compact JSON. Malformed or non-JSON lines are dropped.
// Ported from freebuff-api-kiprana sanitize_stream_chunk.
func SanitizeChunk(line []byte) ([]byte, bool) {
	data, ok := parseSSEData(line)
	if !ok {
		return nil, true
	}
	var chunk map[string]any
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, true
	}
	clean := sanitizeChunk(chunk)
	if clean == nil {
		return nil, true
	}
	out, err := json.Marshal(clean)
	if err != nil {
		return nil, true
	}
	return out, false
}

// sanitizeChunk implements the per-chunk cleanup; returns nil to drop.
func sanitizeChunk(chunk map[string]any) map[string]any {
	clean := make(map[string]any, 5)
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
		if rc, ok := delta["reasoning_content"]; ok {
			delete(delta, "reasoning_content")
			if s, isStr := rc.(string); isStr {
				delta["reasoning_content"] = s
			}
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
	contentParts      []string
	reasoningParts    []string
	finishReason      string
	usage             any
	systemFingerprint string
	toolCalls         map[int]*toolCall
}

// NewAccumulator returns an accumulator with a fresh chatcmpl- id and
// created timestamp; model/id/created are refined by the first chunks seen.
func NewAccumulator() *Accumulator {
	return &Accumulator{
		id:        "chatcmpl-" + randHex(16),
		created:   time.Now().Unix(),
		toolCalls: make(map[int]*toolCall),
	}
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
		if rc, ok := delta["reasoning_content"].(string); ok {
			a.reasoningParts = append(a.reasoningParts, rc)
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
	msg := map[string]any{
		"role":    "assistant",
		"content": strings.Join(a.contentParts, ""),
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
		finish = "stop"
	}
	resp := map[string]any{
		"id":      a.id,
		"object":  "chat.completion",
		"created": a.created,
		"model":   a.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": usage,
	}
	if a.systemFingerprint != "" {
		resp["system_fingerprint"] = a.systemFingerprint
	}
	// Values came from encoding/json, so marshaling cannot fail.
	b, _ := json.Marshal(resp)
	return b
}
