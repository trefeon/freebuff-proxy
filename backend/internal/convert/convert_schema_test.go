package convert

import (
	"fmt"
	"math"
	"testing"
)

func TestNormalizeRequestToolSchemas(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		want   any
	}{
		{
			name: "bare ref resolved against definitions",
			params: map[string]any{
				"type":        "object",
				"properties":  map[string]any{"a": map[string]any{"$ref": "#/definitions/Args"}},
				"definitions": map[string]any{"Args": map[string]any{"type": "object"}},
			},
			want: map[string]any{
				"type":       "object",
				"properties": map[string]any{"a": map[string]any{"type": "object"}},
			},
		},
		{
			// Issue #95 (inlineLocalSchemaRefs): a $ref WITH siblings is
			// resolved against the schema root and the siblings are merged
			// over the target (JS {...resolved, ...siblings}); definitions is
			// dropped.
			name: "ref with siblings resolved against root, siblings merged",
			params: map[string]any{
				"$ref":        "#/definitions/Args",
				"description": "d",
				"definitions": map[string]any{"Args": map[string]any{"type": "object"}},
			},
			want: map[string]any{"description": "d", "type": "object"},
		},
		{
			name: "dollar defs ref resolved",
			params: map[string]any{
				"type":       "object",
				"properties": map[string]any{"a": map[string]any{"$ref": "#/$defs/Args"}},
				"$defs":      map[string]any{"Args": map[string]any{"type": "integer"}},
			},
			want: map[string]any{
				"type":       "object",
				"properties": map[string]any{"a": map[string]any{"type": "integer"}},
			},
		},
		{
			name:   "unresolvable ref kept",
			params: map[string]any{"$ref": "#/definitions/Missing"},
			want:   map[string]any{"$ref": "#/definitions/Missing"},
		},
		{
			name: "anyOf with null simplified to plain type",
			params: map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "null"},
				},
			},
			want: map[string]any{"type": "string"},
		},
		{
			name: "anyOf with null merged into sibling keys",
			params: map[string]any{
				"title": "t",
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "null"},
				},
			},
			want: map[string]any{"title": "t", "type": "string"},
		},
		{
			// Ported behavior (faithful to convert.js): options are recursed
			// BEFORE the null filter, so a const:null option is cleaned to an
			// empty object and kept; only type:"null" options are filtered.
			name: "anyOf with null via const null cleaned to empty object",
			params: map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"const": nil},
				},
			},
			want: map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{}}},
		},
		{
			name: "anyOf multi-option keeps array minus null",
			params: map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "null"},
					map[string]any{"type": "integer"},
				},
			},
			want: map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}},
		},
		{
			name:   "oneOf all null dropped",
			params: map[string]any{"oneOf": []any{map[string]any{"type": "null"}, map[string]any{"const": nil}}},
			want:   map[string]any{},
		},
		{
			name: "bare null literal in anyOf is kept",
			params: map[string]any{
				"anyOf": []any{nil, map[string]any{"type": "string"}},
			},
			want: map[string]any{"anyOf": []any{nil, map[string]any{"type": "string"}}},
		},
		{
			name:   "enum dedupe and null removal",
			params: map[string]any{"enum": []any{"a", "a", 1.0, 1.0, nil, "b"}},
			want:   map[string]any{"enum": []any{"a", 1.0, "b"}},
		},
		{
			name:   "enum all null dropped",
			params: map[string]any{"enum": []any{nil, nil}},
			want:   map[string]any{},
		},
		{
			name:   "type array reduced to first non-null",
			params: map[string]any{"type": []any{"string", "null"}},
			want:   map[string]any{"type": "string"},
		},
		{
			name:   "type array all null dropped",
			params: map[string]any{"type": []any{"null", "null"}},
			want:   map[string]any{},
		},
		{
			name:   "const null dropped",
			params: map[string]any{"const": nil, "type": "string"},
			want:   map[string]any{"type": "string"},
		},
		{
			name:   "nullable dropped",
			params: map[string]any{"nullable": true, "type": "string"},
			want:   map[string]any{"type": "string"},
		},
		{
			name: "nested properties normalized",
			params: map[string]any{
				"type":       "object",
				"properties": map[string]any{"x": map[string]any{"type": []any{"string", "null"}}},
			},
			want: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"model":    "m",
				"messages": []any{},
				"tools": []any{map[string]any{
					"type":     "function",
					"function": map[string]any{"name": "f", "parameters": tc.params},
				}},
			}
			out, err := NormalizeRequest(mustJSON(t, body), "")
			if err != nil {
				t.Fatalf("NormalizeRequest: %v", err)
			}
			got := decode(t, out)
			tools := got["tools"].([]any)
			fn := tools[0].(map[string]any)["function"].(map[string]any)
			assertJSONEq(t, mustJSON(t, fn["parameters"]), tc.want)
		})
	}
}

func TestNormalizeRequestSchemaDepthCap(t *testing.T) {
	// 20-deep $ref chain; normalization must stop at the depth cap (12
	// levels), leaving a dangling ref instead of resolving to the leaf.
	defs := map[string]any{}
	for i := 0; i < 20; i++ {
		if i == 19 {
			defs[fmt.Sprintf("D%d", i)] = map[string]any{"type": "string"}
		} else {
			defs[fmt.Sprintf("D%d", i)] = map[string]any{"$ref": fmt.Sprintf("#/definitions/D%d", i+1)}
		}
	}
	deep := map[string]any{
		"type":        "object",
		"properties":  map[string]any{"a": map[string]any{"$ref": "#/definitions/D0"}},
		"definitions": defs,
	}
	body := map[string]any{
		"model":    "m",
		"messages": []any{},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "f", "parameters": deep},
		}},
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	assertJSONEq(t, mustJSON(t, fn["parameters"]), map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"$ref": "#/definitions/D10"}},
	})

	// A short chain resolves fully.
	shortDefs := map[string]any{
		"D0": map[string]any{"$ref": "#/definitions/D1"},
		"D1": map[string]any{"$ref": "#/definitions/D2"},
		"D2": map[string]any{"type": "integer"},
	}
	short := map[string]any{
		"type":        "object",
		"properties":  map[string]any{"a": map[string]any{"$ref": "#/definitions/D0"}},
		"definitions": shortDefs,
	}
	body["tools"] = []any{map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "f", "parameters": short},
	}}
	out, err = NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got = decode(t, out)
	fn = got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	assertJSONEq(t, mustJSON(t, fn["parameters"]), map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "integer"}},
	})
}

func TestNormalizeRequestSchemaNodeBudget(t *testing.T) {
	// Shrink the per-request node budget so the pathological case below is
	// small enough to build cheaply.
	opts := Options{MaxSchemaNodes: 32}

	// Valid output preserved: a small schema (5 nodes) still normalizes
	// fully under the budget.
	shortDefs := map[string]any{
		"D0": map[string]any{"$ref": "#/definitions/D1"},
		"D1": map[string]any{"$ref": "#/definitions/D2"},
		"D2": map[string]any{"type": "integer"},
	}
	short := map[string]any{
		"type":        "object",
		"properties":  map[string]any{"a": map[string]any{"$ref": "#/definitions/D0"}},
		"definitions": shortDefs,
	}
	body := map[string]any{
		"model":    "m",
		"messages": []any{},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "f", "parameters": short},
		}},
	}
	out, err := NormalizeRequestOpts(mustJSON(t, body), "", opts)
	if err != nil {
		t.Fatalf("NormalizeRequestOpts (small schema): %v", err)
	}
	got := decode(t, out)
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	assertJSONEq(t, mustJSON(t, fn["parameters"]), map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "integer"}},
	})

	// Pathological schema: a wide tree far exceeding the budget, with a
	// normalization-triggering key ("nullable", dropped by normalization) on
	// every leaf. The budget must stop normalization partway (early leaves
	// normalized, the vast majority returned unchanged) instead of re-copying
	// the subtree at every ancestor up to maxSchemaDepth.
	wide := map[string]any{
		"type":        "object",
		"definitions": map[string]any{"Unused": map[string]any{"type": "string"}},
		"properties":  map[string]any{},
	}
	for i := 0; i < 100; i++ {
		child := map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
		for j := 0; j < 100; j++ {
			child["properties"].(map[string]any)[fmt.Sprintf("p%d_%d", i, j)] =
				map[string]any{"type": "string", "nullable": true}
		}
		wide["properties"].(map[string]any)[fmt.Sprintf("o%d", i)] = child
	}
	body["tools"] = []any{map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "f", "parameters": wide},
	}}
	out, err = NormalizeRequestOpts(mustJSON(t, body), "", opts)
	if err != nil {
		t.Fatalf("NormalizeRequestOpts (pathological schema): %v", err)
	}
	got = decode(t, out)
	fn = got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	// Normalization ran at the root (definitions dropped) and the untouched
	// remainder is still structurally intact.
	params := fn["parameters"].(map[string]any)
	if _, ok := params["definitions"]; ok {
		t.Error("root definitions survived normalization")
	}
	props := params["properties"].(map[string]any)
	if len(props) != 100 {
		t.Fatalf("properties = %d entries, want 100 (untouched remainder)", len(props))
	}
	total, normalized := 0, 0
	for _, o := range props {
		child := o.(map[string]any)
		for _, p := range child["properties"].(map[string]any) {
			total++
			if _, dropped := p.(map[string]any)["nullable"]; !dropped {
				normalized++
			}
		}
	}
	if normalized == 0 {
		t.Error("no leaf was normalized before the budget exhausted")
	}
	if normalized == total {
		t.Error("every leaf was normalized — the node budget did not stop early")
	}
}

func TestNormalizeRequestNestedDefinitionsMerge(t *testing.T) {
	// Definitions carried by a mid-tree node are merged into the table and
	// resolvable by deeper refs.
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"wrap": map[string]any{
				"type":        "object",
				"definitions": map[string]any{"Inner": map[string]any{"type": "integer"}},
				"properties":  map[string]any{"y": map[string]any{"$ref": "#/definitions/Inner"}},
			},
		},
	}
	body := map[string]any{
		"model":    "m",
		"messages": []any{},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "f", "parameters": params},
		}},
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	assertJSONEq(t, mustJSON(t, fn["parameters"]), map[string]any{
		"type": "object",
		"properties": map[string]any{
			"wrap": map[string]any{
				"type":       "object",
				"properties": map[string]any{"y": map[string]any{"type": "integer"}},
			},
		},
	})
}

func TestNormalizeRequestInjectsEndTurnTool(t *testing.T) {
	body := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "custom_tool",
					"description": "custom user tool",
					"parameters":  map[string]any{"type": "object"},
				},
			},
		},
	}

	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("want 2 tools (custom_tool + end_turn), got %v", tools)
	}

	hasCustom, hasEndTurn := false, false
	for _, tVal := range tools {
		if tm, ok := tVal.(map[string]any); ok {
			if fn, ok := tm["function"].(map[string]any); ok {
				if fn["name"] == "custom_tool" {
					hasCustom = true
				}
				if fn["name"] == "end_turn" {
					hasEndTurn = true
				}
			}
		}
	}
	if !hasCustom || !hasEndTurn {
		t.Errorf("hasCustom = %v, hasEndTurn = %v", hasCustom, hasEndTurn)
	}
}

// TestNormalizeEnumCrossTypeDedupe pins the enum dedupe key: Go type AND
// JSON encoding combine ("%T:repr"), so values with the same JSON
// representation but different Go types (float64(1.0) vs int(1) — both
// "1" after encoding) are BOTH kept, while exact duplicates are removed
// and null entries dropped. (Tested white-box because the request path
// round-trips numbers through encoding/json, which collapses every number
// to float64.)
func TestNormalizeEnumCrossTypeDedupe(t *testing.T) {
	schema := map[string]any{"enum": []any{1.0, 1, "1", "a", "a", true, true, nil}}
	normalizeEnumField(schema)
	enum := schema["enum"].([]any)
	want := []any{1.0, 1, "1", "a", true}
	if len(enum) != len(want) {
		t.Fatalf("enum = %v (%d), want %v (%d): cross-type entries kept, exact dupes removed", enum, len(enum), want, len(want))
	}
	for i := range want {
		if enum[i] != want[i] || fmt.Sprintf("%T", enum[i]) != fmt.Sprintf("%T", want[i]) {
			t.Errorf("enum[%d] = %v (%T), want %v (%T)", i, enum[i], enum[i], want[i], want[i])
		}
	}
}

// TestNormalizeRequestNonMapToolsEndTurn pins end_turn injection when the
// tools array holds ONLY non-map entries: each entry is skipped by the
// schema normalizer but left in place, and end_turn is still appended
// (the array is non-empty, so the foreign_toolset validation path
// triggers). An existing end_turn must never be duplicated.
func TestNormalizeRequestNonMapToolsEndTurn(t *testing.T) {
	t.Run("all non-map tools get end_turn appended", func(t *testing.T) {
		body := map[string]any{
			"model":    "m",
			"messages": []any{},
			"tools":    []any{"not-a-map", 42},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		tools, ok := got["tools"].([]any)
		if !ok {
			t.Fatal("tools missing")
		}
		if len(tools) != 3 {
			t.Fatalf("tools = %v, want [not-a-map, 42, end_turn]", tools)
		}
		if tools[0] != "not-a-map" || tools[1] != float64(42) {
			t.Errorf("non-map entries were modified: %v", tools[:2])
		}
		endTurn, ok := tools[2].(map[string]any)
		if !ok || endTurn["function"].(map[string]any)["name"] != "end_turn" {
			t.Errorf("tools[2] = %v, want the end_turn tool", tools[2])
		}
	})

	t.Run("existing end_turn not duplicated", func(t *testing.T) {
		body := map[string]any{
			"model":    "m",
			"messages": []any{},
			"tools": []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":       "end_turn",
					"parameters": map[string]any{"type": "object"},
				},
			}},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		tools, _ := got["tools"].([]any)
		count := 0
		for _, tVal := range tools {
			if tm, ok := tVal.(map[string]any); ok {
				if fn, ok := tm["function"].(map[string]any); ok && fn["name"] == "end_turn" {
					count++
				}
			}
		}
		if count != 1 {
			t.Errorf("end_turn appears %d times, want exactly 1: %v", count, tools)
		}
	})
}

// ---------------------------------------------------------------------------
// Issue #67/#95 — tool-schema cache + $ref inlining.
// ---------------------------------------------------------------------------

// TestSchemaCacheHitAndMiss verifies the normalized-schema cache: the second
// normalization of the same raw schema is a hit returning a deep clone, and
// mutating the returned map never corrupts the cached entry.
func TestSchemaCacheHitAndMiss(t *testing.T) {
	resetSchemaCache()
	t.Cleanup(resetSchemaCache)

	params := map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": []any{"string", "null"}}},
	}
	budget := DefaultMaxSchemaNodes
	first := normalizeToolSchemaCached(params, &budget)
	if hits, misses := schemaCacheStats(); hits != 0 || misses != 1 {
		t.Fatalf("after first normalize: hits=%d misses=%d, want 0/1", hits, misses)
	}

	budget2 := DefaultMaxSchemaNodes
	second := normalizeToolSchemaCached(params, &budget2)
	if hits, misses := schemaCacheStats(); hits != 1 || misses != 1 {
		t.Fatalf("after second normalize: hits=%d misses=%d, want 1/1", hits, misses)
	}
	assertJSONEq(t, mustJSON(t, second), map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "string"}},
	})

	// A cache hit returns a clone: mutating it must not poison the cache.
	second["type"] = "mutated"
	budget3 := DefaultMaxSchemaNodes
	third := normalizeToolSchemaCached(params, &budget3)
	if third["type"] == "mutated" {
		t.Error("cached value aliased by caller mutation")
	}
	if first["type"] == "mutated" {
		t.Error("first-call result aliases the cache entry")
	}
}

// TestInlineLocalSchemaRefs pins the #95 $ref inlining: "#/..." JSON pointers
// resolve against the schema root (including deep pointers and refs with
// siblings, merged over the target), and $defs/definitions are stripped.
func TestInlineLocalSchemaRefs(t *testing.T) {
	params := map[string]any{
		"$defs": map[string]any{
			"Args": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"x":      map[string]any{"type": "integer"},
					"nested": map[string]any{"$ref": "#/$defs/Inner"},
				},
			},
			"Inner": map[string]any{"type": "string"},
		},
		"type": "object",
		"properties": map[string]any{
			"args": map[string]any{"$ref": "#/$defs/Args", "description": "the args"},
		},
	}
	body := map[string]any{
		"model":    "m",
		"messages": []any{},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "f", "parameters": params},
		}},
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	assertJSONEq(t, mustJSON(t, fn["parameters"]), map[string]any{
		"type": "object",
		"properties": map[string]any{
			"args": map[string]any{
				"type":        "object",
				"description": "the args",
				"properties": map[string]any{
					"x":      map[string]any{"type": "integer"},
					"nested": map[string]any{"type": "string"},
				},
			},
		},
	})
}

// TestSchemaRefCycleGuard pins the #95 cycle guard: a $ref that re-enters a
// ref already on the current descent path resolves to {} instead of looping.
func TestSchemaRefCycleGuard(t *testing.T) {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"$ref": "#/$defs/A"},
		},
		"$defs": map[string]any{
			"A": map[string]any{
				"type":       "object",
				"properties": map[string]any{"b": map[string]any{"$ref": "#/$defs/A"}},
			},
		},
	}
	body := map[string]any{
		"model":    "m",
		"messages": []any{},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "f", "parameters": params},
		}},
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	assertJSONEq(t, mustJSON(t, fn["parameters"]), map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{
				"type":       "object",
				"properties": map[string]any{"b": map[string]any{}},
			},
		},
	})
}

// TestSchemaUnresolvableRefSiblings pins #95's unresolvable-ref handling: a
// bare unresolvable ref is kept (existing behavior), while the remaining
// siblings of an unresolvable ref are still visited.
func TestSchemaUnresolvableRefSiblings(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		want   any
	}{
		{
			name:   "bare unresolvable ref kept",
			params: map[string]any{"$ref": "#/definitions/Missing"},
			want:   map[string]any{"$ref": "#/definitions/Missing"},
		},
		{
			name:   "unresolvable ref with siblings keeps siblings",
			params: map[string]any{"$ref": "#/definitions/Missing", "description": "d"},
			want:   map[string]any{"description": "d"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"model":    "m",
				"messages": []any{},
				"tools": []any{map[string]any{
					"type":     "function",
					"function": map[string]any{"name": "f", "parameters": tc.params},
				}},
			}
			out, err := NormalizeRequest(mustJSON(t, body), "")
			if err != nil {
				t.Fatalf("NormalizeRequest: %v", err)
			}
			got := decode(t, out)
			fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
			assertJSONEq(t, mustJSON(t, fn["parameters"]), tc.want)
		})
	}
}

// ---------------------------------------------------------------------------
// Issue #63/#67 — benchmark for the tool-schema normalization cache.
// ---------------------------------------------------------------------------
func BenchmarkNormalizeToolSchema(b *testing.B) {
	params := map[string]any{
		"$defs": map[string]any{
			"Args": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"x":      map[string]any{"type": "integer"},
					"nested": map[string]any{"$ref": "#/$defs/Inner"},
				},
			},
			"Inner": map[string]any{"type": "string"},
		},
		"type": "object",
		"properties": map[string]any{
			"args": map[string]any{"$ref": "#/$defs/Args", "description": "the args"},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		budget := DefaultMaxSchemaNodes
		_ = normalizeToolSchemaCached(params, &budget)
	}
}

// capHint guards allocation-size hints against int overflow; a wrapped hint
// panics Go's slice runtime (makeslice: cap out of range) and corrupts map
// preallocation. Overflowing inputs must fall back to 0 (no hint) so the
// container grows dynamically. CodeQL: "size computation for allocation may
// overflow".
func TestCapHint(t *testing.T) {
	cases := []struct {
		name string
		a, b int
		want int
	}{
		{"zero", 0, 0, 0},
		{"normal", 3, 5, 8},
		{"slice cap", 4, 1, 5},
		{"at max", math.MaxInt, 0, math.MaxInt},
		{"overflow drops hint", math.MaxInt, 1, 0},
		{"overflow drops hint both", 1 << 20, math.MaxInt, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := capHint(tc.a, tc.b); got != tc.want {
				t.Fatalf("capHint(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// StripEndTurnToolCalls — strips injected end_turn pseudo-tool-calls from
// OpenAI-format relay chunks (streaming delta + non-streaming message).
// ---------------------------------------------------------------------------
func TestStripEndTurnToolCalls(t *testing.T) {
	t.Run("SingleFragment", func(t *testing.T) {
		// Streaming chunk: single end_turn tool_call in delta.tool_calls.
		// Expected: tool_calls removed, finish_reason flipped to "stop".
		chunk := decode(t, mustJSON(t, map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "tool_calls",
				"delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": 0,
						"id":    "call_001",
						"type":  "function",
						"function": map[string]any{
							"name":      "end_turn",
							"arguments": "",
						},
					}},
				},
			}},
		}))

		remaining, fr := StripEndTurnToolCalls(chunk)

		if remaining {
			t.Error("toolCallsRemaining = true, want false")
		}
		if fr != "stop" {
			t.Errorf("finishReason = %q, want %q", fr, "stop")
		}
		// delta.tool_calls should be deleted entirely.
		delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if _, ok := delta["tool_calls"]; ok {
			t.Error("delta.tool_calls should be deleted, but still present")
		}
		choice0 := chunk["choices"].([]any)[0].(map[string]any)
		if choice0["finish_reason"] != "stop" {
			t.Errorf("choice.finish_reason = %v, want %q", choice0["finish_reason"], "stop")
		}
	})

	t.Run("ContinuationFragment", func(t *testing.T) {
		// Streaming chunk: a continuation fragment with only index (no
		// function.name). The helper only strips entries where
		// function.name == "end_turn"; entries without function.name are
		// kept. The relay loop would have dropped these via
		// endTurnCallIndexes, but the helper itself preserves them.
		chunk := decode(t, mustJSON(t, map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "tool_calls",
				"delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": 0,
					}},
				},
			}},
		}))

		remaining, fr := StripEndTurnToolCalls(chunk)

		if !remaining {
			t.Error("toolCallsRemaining = false, want true")
		}
		if fr != "tool_calls" {
			t.Errorf("finishReason = %q, want %q", fr, "tool_calls")
		}
		// tool_calls should still be present.
		delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		tcs, ok := delta["tool_calls"]
		if !ok {
			t.Fatal("delta.tool_calls was deleted; should have been kept")
		}
		tcsSlice, ok := tcs.([]any)
		if !ok || len(tcsSlice) != 1 {
			t.Errorf("len(delta.tool_calls) = %v, want 1 element", tcs)
		}
	})

	t.Run("MixedToolCalls", func(t *testing.T) {
		// Streaming chunk: one end_turn + one real tool call (bash).
		// Expected: only end_turn stripped, bash kept, finish_reason stays "tool_calls".
		chunk := decode(t, mustJSON(t, map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "tool_calls",
				"delta": map[string]any{
					"tool_calls": []map[string]any{
						{
							"index": 0,
							"id":    "call_et",
							"type":  "function",
							"function": map[string]any{
								"name":      "end_turn",
								"arguments": "",
							},
						},
						{
							"index": 1,
							"id":    "call_bash",
							"type":  "function",
							"function": map[string]any{
								"name":      "bash",
								"arguments": `{"command":"echo hi"}`,
							},
						},
					},
				},
			}},
		}))

		remaining, fr := StripEndTurnToolCalls(chunk)

		if !remaining {
			t.Error("toolCallsRemaining = false, want true")
		}
		if fr != "tool_calls" {
			t.Errorf("finishReason = %q, want %q", fr, "tool_calls")
		}
		delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		tcsSlice := delta["tool_calls"].([]any)
		if len(tcsSlice) != 1 {
			t.Fatalf("len(delta.tool_calls) = %d, want 1", len(tcsSlice))
		}
		fn := tcsSlice[0].(map[string]any)["function"].(map[string]any)
		if fn["name"] != "bash" {
			t.Errorf("remaining tool_call name = %q, want %q", fn["name"], "bash")
		}
	})

	t.Run("NonStreamingMessage", func(t *testing.T) {
		// Non-streaming chunk: message.tool_calls with one end_turn entry.
		// Expected: end_turn stripped, finish_reason flipped to "stop".
		chunk := decode(t, mustJSON(t, map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []map[string]any{{
						"id":   "call_001",
						"type": "function",
						"function": map[string]any{
							"name":      "end_turn",
							"arguments": "{}",
						},
					}},
				},
			}},
		}))

		remaining, fr := StripEndTurnToolCalls(chunk)

		if remaining {
			t.Error("toolCallsRemaining = true, want false")
		}
		if fr != "stop" {
			t.Errorf("finishReason = %q, want %q", fr, "stop")
		}
		msg := chunk["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if _, ok := msg["tool_calls"]; ok {
			t.Error("message.tool_calls should be deleted, but still present")
		}
		choice0 := chunk["choices"].([]any)[0].(map[string]any)
		if choice0["finish_reason"] != "stop" {
			t.Errorf("choice.finish_reason = %v, want %q", choice0["finish_reason"], "stop")
		}
	})

	t.Run("NonStreamingMixed", func(t *testing.T) {
		// Non-streaming chunk: end_turn + real tool (bash).
		chunk := decode(t, mustJSON(t, map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []map[string]any{
						{
							"id":   "call_et",
							"type": "function",
							"function": map[string]any{
								"name":      "end_turn",
								"arguments": "{}",
							},
						},
						{
							"id":   "call_bash",
							"type": "function",
							"function": map[string]any{
								"name":      "bash",
								"arguments": `{"command":"ls"}`,
							},
						},
					},
				},
			}},
		}))

		remaining, fr := StripEndTurnToolCalls(chunk)

		if !remaining {
			t.Error("toolCallsRemaining = false, want true")
		}
		if fr != "tool_calls" {
			t.Errorf("finishReason = %q, want %q", fr, "tool_calls")
		}
		msg := chunk["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		tcs := msg["tool_calls"].([]any)
		if len(tcs) != 1 {
			t.Fatalf("len(message.tool_calls) = %d, want 1", len(tcs))
		}
		fn := tcs[0].(map[string]any)["function"].(map[string]any)
		if fn["name"] != "bash" {
			t.Errorf("remaining tool_call name = %q, want %q", fn["name"], "bash")
		}
	})

	t.Run("NoToolCalls", func(t *testing.T) {
		// Chunk with no tool_calls at all. Expected: no-op, finish_reason unchanged.
		chunk := decode(t, mustJSON(t, map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "stop",
				"delta": map[string]any{
					"content": "Hello!",
				},
			}},
		}))

		remaining, fr := StripEndTurnToolCalls(chunk)

		if remaining {
			t.Error("toolCallsRemaining = true, want false")
		}
		if fr != "stop" {
			t.Errorf("finishReason = %q, want %q", fr, "stop")
		}
	})

	t.Run("FinishReasonStop", func(t *testing.T) {
		// Chunk with end_turn tool_calls but finish_reason already "stop".
		// Expected: end_turn stripped, finish_reason stays "stop" (no flip needed).
		chunk := decode(t, mustJSON(t, map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "stop",
				"delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": 0,
						"id":    "call_001",
						"type":  "function",
						"function": map[string]any{
							"name":      "end_turn",
							"arguments": "",
						},
					}},
				},
			}},
		}))

		remaining, fr := StripEndTurnToolCalls(chunk)

		if remaining {
			t.Error("toolCallsRemaining = true, want false")
		}
		if fr != "stop" {
			t.Errorf("finishReason = %q, want %q", fr, "stop")
		}
		// finish_reason should still be "stop" — not flipped, just unchanged.
		choice0 := chunk["choices"].([]any)[0].(map[string]any)
		if choice0["finish_reason"] != "stop" {
			t.Errorf("choice.finish_reason = %v, want %q", choice0["finish_reason"], "stop")
		}
	})
}
