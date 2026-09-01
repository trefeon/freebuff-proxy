package convert

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// maxSchemaDepth caps recursive JSON-schema normalization. Ported from
// proxy-freebuff's normalizeSchemaMap, which resolves with depth 12.
const maxSchemaDepth = 12

// capHint returns a+b for use as a make() size hint, or 0 when the sum would
// overflow int. An unguarded len(a)+len(b) hint can wrap negative and panic
// the runtime (makeslice: "cap out of range" / "len out of range"; makemap
// clamps negative hints but a wrapped positive is still wrong). On overflow
// we drop the hint entirely and let the container grow dynamically - the
// hint is pure preallocation optimization, never correctness. CodeQL:
// "size computation for allocation may overflow" (go/allocation-size-overflow,
// convert.go:432,754,905,1006).
func capHint(a, b int) int {
	if a > math.MaxInt-b {
		return 0
	}
	return a + b
}

// ---------------------------------------------------------------------------
// Tool-schema normalization (ported from proxy-freebuff/lib/convert.js,
// normalizeToolSchemas / normalizeSchemaMap, lines ~40-154).
// ---------------------------------------------------------------------------

// normalizeToolSchemas normalizes fn.parameters for every function tool in
// the payload, in place. Each tool's parameters are normalized through the
// schema cache (#67): the raw schema JSON hash + starting node budget key a
// bounded, mutex-guarded LRU, so repeated tool-call loops re-send identical
// context without re-running normalization.
func normalizeToolSchemas(payload map[string]any, opts Options) {
	tools, _ := payload["tools"].([]any)
	if len(tools) == 0 {
		return
	}
	// One node budget per request, shared across tools.
	budget := opts.MaxSchemaNodes
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
		fn["parameters"] = normalizeToolSchemaCached(params, &budget)
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

// ---------------------------------------------------------------------------
// Tool-schema cache (issue #67/#95).
// ---------------------------------------------------------------------------

// schemaCacheKey identifies one cached normalization: the SHA-256 of the raw
// schema JSON plus the node budget it was computed under. normalizeSchemaMap
// is a pure function of (raw schema, starting budget, maxSchemaDepth), so the
// pair pins the output exactly.
type schemaCacheKey struct {
	hash   [sha256.Size]byte
	budget int
}

// schemaCacheMax bounds the LRU. Schemas are usually small; 256 entries
// covers a typical long tool-call loop many times over.
const schemaCacheMax = 256

// schemaCache is a small mutex-guarded LRU of normalized tool schemas.
type schemaCacheType struct {
	mu      sync.Mutex
	entries map[schemaCacheKey]map[string]any
	order   []schemaCacheKey // most-recently-used last
}

var schemaCache = schemaCacheType{entries: make(map[schemaCacheKey]map[string]any)}

// schemaCacheHits/Misses count cache outcomes (tests + diagnostics).
var (
	schemaCacheHits   atomic.Uint64
	schemaCacheMisses atomic.Uint64
)

// resetSchemaCache clears the cache and its counters (tests).
func resetSchemaCache() {
	schemaCache.mu.Lock()
	schemaCache.entries = make(map[schemaCacheKey]map[string]any)
	schemaCache.order = schemaCache.order[:0]
	schemaCache.mu.Unlock()
	schemaCacheHits.Store(0)
	schemaCacheMisses.Store(0)
}

// schemaCacheStats returns the hit/miss counters.
func schemaCacheStats() (hits, misses uint64) {
	return schemaCacheHits.Load(), schemaCacheMisses.Load()
}

// normalizeToolSchemaCached returns the normalized tool schema for params,
// consulting the cache first. Cache hits return a deep clone (callers must
// never mutate cached values); misses normalize, store the result and return
// it directly. The shared per-request budget is decremented only on misses.
func normalizeToolSchemaCached(params map[string]any, budget *int) map[string]any {
	raw, err := json.Marshal(params)
	if err != nil {
		// Values not produced by encoding/json cannot be hashed reliably:
		// normalize uncached.
		return normalizeSchemaMap(params, extractDefinitions(params), params, nil, maxSchemaDepth, budget)
	}
	key := schemaCacheKey{hash: sha256.Sum256(raw), budget: *budget}
	if cached, ok := schemaCacheGet(key); ok {
		schemaCacheHits.Add(1)
		return cloneValue(cached, maxSchemaDepth).(map[string]any)
	}
	schemaCacheMisses.Add(1)
	normalized := normalizeSchemaMap(params, extractDefinitions(params), params, nil, maxSchemaDepth, budget)
	schemaCachePut(key, normalized)
	return normalized
}

// schemaCacheGet returns the cached normalized schema for key, promoting it
// to most-recently-used.
func schemaCacheGet(key schemaCacheKey) (map[string]any, bool) {
	schemaCache.mu.Lock()
	defer schemaCache.mu.Unlock()
	v, ok := schemaCache.entries[key]
	if !ok {
		return nil, false
	}
	for i, k := range schemaCache.order {
		if k == key {
			schemaCache.order = append(schemaCache.order[:i], schemaCache.order[i+1:]...)
			break
		}
	}
	schemaCache.order = append(schemaCache.order, key)
	return v, true
}

// schemaCachePut stores normalized under key, evicting the least-recently-used
// entry when the cache is full.
func schemaCachePut(key schemaCacheKey, normalized map[string]any) {
	schemaCache.mu.Lock()
	defer schemaCache.mu.Unlock()
	if _, ok := schemaCache.entries[key]; ok {
		schemaCache.entries[key] = normalized
		return
	}
	if len(schemaCache.entries) >= schemaCacheMax {
		oldest := schemaCache.order[0]
		delete(schemaCache.entries, oldest)
		schemaCache.order = schemaCache.order[1:]
	}
	schemaCache.entries[key] = normalized
	schemaCache.order = append(schemaCache.order, key)
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
	merged := make(map[string]any, capHint(len(parent), len(local)))
	for k, v := range parent {
		merged[k] = v
	}
	for k, v := range local {
		merged[k] = v
	}
	return merged
}

// normalizeSchemaMap normalizes one JSON-schema node: resolves $ref nodes
// against the definition table AND the schema root (issue #95 — JSON-pointer
// lookup with sibling merging and a cycle guard), recurses into values
// (depth-capped and node-budgeted), drops definitions/$defs/nullable,
// simplifies nullable anyOf/oneOf, and cleans up type/enum/const fields.
//
// root is the tool's raw parameters schema, used to resolve "#/..." JSON
// pointers (inlineLocalSchemaRefs semantics); refStack tracks the refs on the
// current descent path so a re-entered ref resolves to {} instead of looping.
// The returned map is always freshly allocated except at the depth cap or
// budget exhaustion, where the node is returned as-is.
func normalizeSchemaMap(node map[string]any, defs map[string]any, root map[string]any, refStack map[string]bool, maxDepth int, budget *int) map[string]any {
	if maxDepth <= 0 || *budget <= 0 {
		return node // cap: leave the remaining structure untouched
	}
	*budget--
	defs = mergeDefinitions(defs, extractDefinitions(node))

	if ref, _ := node["$ref"].(string); ref != "" && strings.HasPrefix(ref, "#/") {
		if refStack[ref] {
			return map[string]any{} // cycle guard: re-entered ref resolves to {}
		}
		nextStack := make(map[string]bool, len(refStack)+1)
		for k, v := range refStack {
			nextStack[k] = v
		}
		nextStack[ref] = true

		// 1. Bare refs against the merged definition table (existing
		//    behavior; also handles #/definitions/<name> and #/$defs/<name>).
		if replaced, ok := tryResolveRef(node, defs, maxDepth); ok {
			if resolved, isMap := replaced.(map[string]any); isMap {
				return normalizeSchemaMap(resolved, defs, root, nextStack, maxDepth-1, budget)
			}
			return node
		}

		// 2. JSON-pointer resolution against the schema root, merging $ref
		//    siblings over the resolved target (inlineLocalSchemaRefs).
		if target, ok := lookupJsonPointer(root, ref); ok {
			resolved := normalizeSchemaValue(target, defs, root, nextStack, maxDepth-1, budget)
			rm, isMap := resolved.(map[string]any)
			if !isMap {
				return node
			}
			if siblings := withoutRef(node); len(siblings) > 0 {
				merged := mergeMaps(rm, siblings) // siblings win, JS {...resolved, ...siblings}
				return normalizeSchemaMap(merged, defs, root, refStack, maxDepth-1, budget)
			}
			return rm
		}

		// 3. Unresolvable: keep bare refs as-is (existing behavior); visit
		//    the remaining siblings when the ref carries any.
		if siblings := withoutRef(node); len(siblings) > 0 {
			return normalizeSchemaMap(siblings, defs, root, refStack, maxDepth-1, budget)
		}
		return node
	}

	normalized := make(map[string]any, len(node))
	for key, value := range node {
		normalized[key] = normalizeSchemaValue(value, defs, root, refStack, maxDepth-1, budget)
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
func normalizeSchemaValue(value any, defs map[string]any, root map[string]any, refStack map[string]bool, maxDepth int, budget *int) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = normalizeSchemaValue(e, defs, root, refStack, maxDepth, budget)
		}
		return out
	case map[string]any:
		return normalizeSchemaMap(v, defs, root, refStack, maxDepth, budget)
	default:
		return value
	}
}

// lookupJsonPointer resolves a "#/..." JSON pointer against the schema root,
// decoding ~1 → "/" and ~0 → "~" per segment (JS decodeJsonPointerSegment in
// openai-compatible-prepare-tools.ts). Array indices resolve numerically.
func lookupJsonPointer(root map[string]any, pointer string) (any, bool) {
	if !strings.HasPrefix(pointer, "#/") {
		return nil, false
	}
	var current any = root
	for _, segment := range strings.Split(pointer[2:], "/") {
		segment = strings.ReplaceAll(segment, "~1", "/")
		segment = strings.ReplaceAll(segment, "~0", "~")
		switch c := current.(type) {
		case map[string]any:
			v, ok := c[segment]
			if !ok {
				return nil, false
			}
			current = v
		case []any:
			idx, err := strconv.Atoi(segment)
			if err != nil || idx < 0 || idx >= len(c) {
				return nil, false
			}
			current = c[idx]
		default:
			return nil, false
		}
	}
	return current, true
}

// withoutRef returns a copy of node with the "$ref" key removed, or nil when
// node has no $ref key.
func withoutRef(node map[string]any) map[string]any {
	if _, ok := node["$ref"]; !ok {
		return nil
	}
	out := make(map[string]any, len(node)-1)
	for k, v := range node {
		if k != "$ref" {
			out[k] = v
		}
	}
	return out
}

// mergeMaps combines base with override; override wins on key collision
// (JS object spread semantics).
func mergeMaps(base, override map[string]any) map[string]any {
	out := make(map[string]any, capHint(len(base), len(override)))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// tryResolveRef resolves a node that is a BARE {"$ref": "..."} (no sibling
// keys) against the definition table. Returns (replacement, true) on
// success; the replacement is a deep clone, depth-capped so cyclic or deeply
// nested definitions cannot explode. Ported from JS tryResolveRef: only
// "#/definitions/<name>" and "#/$defs/<name>" pointers resolve, and only
// when the name exists in the table.
func tryResolveRef(node map[string]any, defs map[string]any, maxDepth int) (any, bool) {
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
	return cloneValue(resolved, maxDepth), true
}

// cloneValue deep-clones any JSON value, stopping below depth 0 (returning
// the value unchanged there) so a single $ref cannot balloon on deeply
// nested or cyclic definition subtrees. Values shared by the depth cap are
// only ever read downstream, never mutated.
func cloneValue(v any, maxDepth int) any {
	if maxDepth <= 0 {
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = cloneValue(val, maxDepth-1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = cloneValue(val, maxDepth-1)
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
			merged := make(map[string]any, capHint(len(schema), len(single)))
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
