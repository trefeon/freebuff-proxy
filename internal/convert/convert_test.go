package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// mustJSON marshals v, failing the test on error.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %#v: %v", v, err)
	}
	return b
}

// decode parses JSON bytes into a map, failing the test on error.
func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", b, err)
	}
	return m
}

// assertJSONEq asserts got (JSON bytes) equals want (Go value) after both
// are normalized through encoding/json.
func assertJSONEq(t *testing.T, got []byte, want any) {
	t.Helper()
	wantBytes := mustJSON(t, want)
	var gotV, wantV any
	if err := json.Unmarshal(got, &gotV); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(wantBytes, &wantV); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	if !reflect.DeepEqual(gotV, wantV) {
		t.Fatalf("mismatch:\n got: %s\nwant: %s", got, wantBytes)
	}
}

func TestNormalizeRequestWhitelist(t *testing.T) {
	body := map[string]any{
		"model":                 "deepseek-v3",
		"messages":              []any{},
		"frequency_penalty":     0.5,
		"logit_bias":            map[string]any{"123": 1.0},
		"logprobs":              true,
		"max_completion_tokens": 100.0,
		"max_tokens":            200.0,
		"metadata":              map[string]any{"k": "v"},
		"modalities":            []any{"text"},
		"parallel_tool_calls":   true,
		"presence_penalty":      -0.2,
		"reasoning_effort":      "high",
		"response_format":       map[string]any{"type": "json_object"},
		"seed":                  42.0,
		"service_tier":          "auto",
		"stop":                  []any{"END"},
		"store":                 false,
		"stream_options":        map[string]any{"include_usage": true},
		"temperature":           0.7,
		"tool_choice":           "auto",
		"tools":                 []any{},
		"top_logprobs":          5.0,
		"top_p":                 0.9,
		"user":                  "u-1",
		// not whitelisted — must be dropped
		"stream":        true,
		"n":             2.0,
		"function_call": "auto",
		"bogus":         "x",
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	for _, key := range []string{
		"model", "messages", "frequency_penalty", "logit_bias", "max_completion_tokens",
		"max_tokens", "metadata", "modalities", "parallel_tool_calls", "presence_penalty",
		"reasoning_effort", "response_format", "seed", "service_tier", "stop", "store",
		"stream_options", "temperature", "tool_choice", "tools", "top_logprobs", "top_p", "user",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("whitelisted key %q dropped", key)
		}
	}
	for _, key := range []string{"stream", "n", "function_call", "bogus"} {
		if _, ok := got[key]; ok {
			t.Errorf("non-whitelisted key %q kept", key)
		}
	}

	// Null-valued whitelisted keys are dropped (meaningless upstream).
	out, err = NormalizeRequest([]byte(`{"model":"m","messages":[],"temperature":null}`), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got = decode(t, out)
	if _, ok := got["temperature"]; ok {
		t.Error("null-valued whitelisted key kept")
	}
}

func TestNormalizeRequestDeveloperToSystem(t *testing.T) {
	body := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "developer", "content": "be brief"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "ok"},
			map[string]any{"role": "system", "content": "sys"},
		},
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	msgs := got["messages"].([]any)
	for i, role := range []string{"system", "user", "assistant", "system"} {
		if got := msgs[i].(map[string]any)["role"]; got != role {
			t.Errorf("message %d role = %v, want %q", i, got, role)
		}
	}
}

func TestNormalizeRequestInvalidBody(t *testing.T) {
	for name, body := range map[string]string{
		"not json":   "{oops",
		"not object": `"hello"`,
		"null":       `null`,
		"empty":      ``,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeRequest([]byte(body), ""); err == nil {
				t.Fatalf("expected error for body %q", body)
			}
		})
	}
}

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
	// small enough to build cheaply; restore for the rest of the package.
	old := maxSchemaNodes
	maxSchemaNodes = 32
	t.Cleanup(func() { maxSchemaNodes = old })

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
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest (small schema): %v", err)
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
	out, err = NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest (pathological schema): %v", err)
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

func TestEncodeSSE(t *testing.T) {
	payload := mustJSON(t, map[string]any{"id": "x", "choices": []any{}})
	frame := EncodeSSE(payload)
	want := "data: " + string(payload) + "\n\n"
	if string(frame) != want {
		t.Fatalf("frame = %q, want %q", frame, want)
	}
	if inner := strings.TrimSuffix(strings.TrimPrefix(string(frame), "data: "), "\n\n"); strings.Contains(inner, " ") {
		t.Fatalf("frame payload contains whitespace: %q", frame)
	}
	if !strings.HasSuffix(string(frame), "\n\n") {
		t.Fatalf("frame does not end with \\n\\n: %q", frame)
	}
}

func TestDONE(t *testing.T) {
	if string(DONE) != "data: [DONE]\n\n" {
		t.Fatalf("DONE = %q, want %q", DONE, "data: [DONE]\n\n")
	}
}

func TestSanitizeChunk(t *testing.T) {
	t.Run("drop empty choice chunk", func(t *testing.T) {
		out, drop := SanitizeChunk(mustJSON(t, map[string]any{"id": "x", "choices": []any{}}))
		if !drop || out != nil {
			t.Fatalf("drop = %v, out = %q; want drop with nil out", drop, out)
		}
	})

	t.Run("keep usage-only chunk", func(t *testing.T) {
		out, drop := SanitizeChunk(mustJSON(t, map[string]any{
			"id": "x", "usage": map[string]any{"prompt_tokens": 1.0},
		}))
		if drop {
			t.Fatal("usage-only chunk dropped")
		}
		got := decode(t, out)
		if got["usage"].(map[string]any)["prompt_tokens"] != float64(1) {
			t.Fatalf("usage not preserved: %v", got["usage"])
		}
		if ch := got["choices"].([]any); len(ch) != 0 {
			t.Fatalf("choices = %v, want empty", ch)
		}
	})

	t.Run("defaults id object created model", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"choices":[{"delta":{"content":"hi"}}]}`))
		if drop {
			t.Fatal("chunk dropped")
		}
		got := decode(t, out)
		if id, _ := got["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
			t.Fatalf("id = %q, want chatcmpl- prefix", id)
		}
		if got["object"] != "chat.completion.chunk" {
			t.Fatalf("object = %v", got["object"])
		}
		if c, _ := got["created"].(float64); c <= 0 {
			t.Fatalf("created = %v", got["created"])
		}
		if got["model"] != "" {
			t.Fatalf("model = %v, want empty", got["model"])
		}
	})

	t.Run("preserves reasoning_content separate from content", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"id":"c1","choices":[{"index":0,"delta":{"content":"","reasoning_content":"think step 1"}}]}`))
		if drop {
			t.Fatal("chunk dropped")
		}
		got := decode(t, out)
		delta := got["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if delta["reasoning_content"] != "think step 1" {
			t.Fatalf("reasoning_content = %v", delta["reasoning_content"])
		}
		if delta["content"] != "" {
			t.Fatalf("content = %v, want empty (not merged with reasoning)", delta["content"])
		}
	})

	t.Run("normalizes openrouter reasoning to reasoning_content", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"id":"c1","choices":[{"index":0,"delta":{"content":"","reasoning":"mimo think step"}}]}`))
		if drop {
			t.Fatal("chunk dropped")
		}
		got := decode(t, out)
		delta := got["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if delta["reasoning_content"] != "mimo think step" {
			t.Fatalf("reasoning_content = %v, want 'mimo think step'", delta["reasoning_content"])
		}
		if delta["reasoning"] != "mimo think step" {
			t.Fatalf("reasoning = %v, want 'mimo think step'", delta["reasoning"])
		}
		if delta["content"] != "" {
			t.Fatalf("content = %v, want empty", delta["content"])
		}
	})

	t.Run("null content removed", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"choices":[{"delta":{"content":null,"reasoning_content":"r"}}]}`))
		if drop {
			t.Fatal("chunk dropped")
		}
		got := decode(t, out)
		delta := got["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if _, ok := delta["content"]; ok {
			t.Fatalf("null content key kept: %v", delta)
		}
		if delta["reasoning_content"] != "r" {
			t.Fatalf("reasoning_content = %v", delta["reasoning_content"])
		}
	})

	t.Run("data prefix and passthrough fields", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`data: {"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","system_fingerprint":"fp","choices":[{"index":1,"delta":{"content":"x"},"finish_reason":"stop","logprobs":{"a":1}}]}`))
		if drop {
			t.Fatal("chunk dropped")
		}
		got := decode(t, out)
		if got["id"] != "c1" || got["object"] != "chat.completion.chunk" || got["created"] != float64(5) ||
			got["model"] != "m" || got["system_fingerprint"] != "fp" {
			t.Fatalf("passthrough fields mangled: %v", got)
		}
		choice := got["choices"].([]any)[0].(map[string]any)
		if choice["index"] != float64(1) || choice["finish_reason"] != "stop" || choice["logprobs"] == nil {
			t.Fatalf("choice fields mangle: %v", choice)
		}
	})

	t.Run("preserves in-band error chunk with map", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"id":"c1","error":{"message":"quota exceeded","type":"insufficient_quota","code":"quota_exceeded"}}`))
		if drop {
			t.Fatal("error chunk dropped")
		}
		got := decode(t, out)
		if got["id"] != "c1" {
			t.Fatalf("id = %v, want c1", got["id"])
		}
		errObj, ok := got["error"].(map[string]any)
		if !ok {
			t.Fatalf("error object = %v, want map", got["error"])
		}
		if errObj["message"] != "quota exceeded" || errObj["type"] != "insufficient_quota" || errObj["code"] != "quota_exceeded" {
			t.Fatalf("error obj mangled: %v", errObj)
		}
	})

	t.Run("preserves in-band error chunk with string", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"error":"upstream stream interrupted"}`))
		if drop {
			t.Fatal("error chunk with string dropped")
		}
		got := decode(t, out)
		errObj, ok := got["error"].(map[string]any)
		if !ok {
			t.Fatalf("error object = %v, want map", got["error"])
		}
		if errObj["message"] != "upstream stream interrupted" || errObj["type"] != "upstream_error" {
			t.Fatalf("error obj mangled: %v", errObj)
		}
	})

	t.Run("malformed and non-JSON lines dropped", func(t *testing.T) {
		for _, line := range []string{`{bad`, `data: {bad`, `hello`, `data: `, `: keep-alive`, ``} {
			out, drop := SanitizeChunk([]byte(line))
			if !drop || out != nil {
				t.Fatalf("line %q: drop = %v, out = %q; want drop with nil out", line, drop, out)
			}
		}
	})
}

func TestErrorChunk(t *testing.T) {
	frame := ErrorChunk("boom", "E1")
	want := `data: ` + string(mustJSON(t, map[string]any{
		"error": map[string]any{"message": "boom", "type": "upstream_error", "code": "E1"},
	})) + "\n\n"
	if string(frame) != want {
		t.Fatalf("ErrorChunk = %q, want %q", frame, want)
	}
	frame = ErrorChunk("boom", "")
	if strings.Contains(string(frame), "code") {
		t.Fatalf("code key present without code: %q", frame)
	}
	if !strings.HasSuffix(string(frame), "\n\n") {
		t.Fatalf("missing trailing newline: %q", frame)
	}
}

func TestAccumulator(t *testing.T) {
	t.Run("full stream", func(t *testing.T) {
		a := NewAccumulator()
		lines := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":100,"model":"m",` +
				`"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel","reasoning_content":"think "}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"lo","reasoning_content":"step"}}]}`, // reason "think step"
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"x\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
			`{"choices":[{"index":1,"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"g","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"finish_reason":"tool_calls"}]}`,
			`{"choices":[{"index":0,"delta":{"content":""}}]}`, // empty content: no finish change
			`{"id":"c1","model":"m","usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8},"system_fingerprint":"fp"}`,
			`data: [DONE]`,
		}
		for _, line := range lines {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add(%q): %v", line, err)
			}
		}
		out := decode(t, a.Finish())
		if out["id"] != "c1" || out["object"] != "chat.completion" || out["model"] != "m" || out["created"] != float64(100) {
			t.Fatalf("header fields: %v", out)
		}
		if out["system_fingerprint"] != "fp" {
			t.Fatalf("system_fingerprint = %v", out["system_fingerprint"])
		}
		choice := out["choices"].([]any)[0].(map[string]any)
		if choice["index"] != float64(0) || choice["finish_reason"] != "tool_calls" {
			t.Fatalf("choice = %v", choice)
		}
		msg := choice["message"].(map[string]any)
		if msg["role"] != "assistant" || msg["content"] != "Hello" {
			t.Fatalf("message = %v", msg)
		}
		if msg["reasoning_content"] != "think step" {
			t.Fatalf("reasoning_content = %v", msg["reasoning_content"])
		}
		calls := msg["tool_calls"].([]any)
		if len(calls) != 2 {
			t.Fatalf("tool_calls = %v", calls)
		}
		first := calls[0].(map[string]any)
		if first["id"] != "call_1" || first["type"] != "function" ||
			first["function"].(map[string]any)["name"] != "f" ||
			first["function"].(map[string]any)["arguments"] != `{"x":1}` {
			t.Fatalf("tool call 0 = %v", first)
		}
		if calls[1].(map[string]any)["id"] != "call_2" {
			t.Fatalf("tool call 1 = %v", calls[1])
		}
		usage := out["usage"].(map[string]any)
		if usage["prompt_tokens"] != float64(5) || usage["completion_tokens"] != float64(3) || usage["total_tokens"] != float64(8) {
			t.Fatalf("usage = %v", usage)
		}
	})

	t.Run("empty stream zeroed usage", func(t *testing.T) {
		a := NewAccumulator()
		if err := a.Add([]byte("data: [DONE]")); err != nil {
			t.Fatalf("Add: %v", err)
		}
		out := decode(t, a.Finish())
		choice := out["choices"].([]any)[0].(map[string]any)
		if choice["finish_reason"] != "stop" {
			t.Fatalf("finish_reason = %v, want stop", choice["finish_reason"])
		}
		if msg := choice["message"].(map[string]any); msg["content"] != "" {
			t.Fatalf("content = %v", msg["content"])
		}
		usage := out["usage"].(map[string]any)
		for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
			if usage[k] != float64(0) {
				t.Fatalf("usage[%s] = %v, want 0", k, usage[k])
			}
		}
	})

	t.Run("finish reason last non-empty wins", func(t *testing.T) {
		a := NewAccumulator()
		for _, line := range []string{
			`{"choices":[{"index":0,"finish_reason":"tool_calls"}]}`,
			`{"choices":[{"index":0,"delta":{"content":"x"}}]}`, // no finish_reason
			`{"choices":[{"index":0,"finish_reason":"stop"}]}`,
		} {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		out := decode(t, a.Finish())
		if fr := out["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "stop" {
			t.Fatalf("finish_reason = %v, want stop", fr)
		}
	})

	t.Run("default ids when missing", func(t *testing.T) {
		a := NewAccumulator()
		if err := a.Add([]byte(`{"choices":[{"index":0,"delta":{"content":"x"}}]}`)); err != nil {
			t.Fatalf("Add: %v", err)
		}
		out := decode(t, a.Finish())
		if id, _ := out["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
			t.Fatalf("id = %v", out["id"])
		}
		if c, _ := out["created"].(float64); c <= 0 {
			t.Fatalf("created = %v", out["created"])
		}
	})

	t.Run("malformed line errors", func(t *testing.T) {
		a := NewAccumulator()
		if err := a.Add([]byte(`data: {bad`)); err == nil {
			t.Fatal("expected error for malformed chunk")
		}
	})

	t.Run("non-data lines ignored", func(t *testing.T) {
		a := NewAccumulator()
		for _, line := range []string{"", ": keep-alive", "event: message", "id: 1", "retry: 100"} {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add(%q): %v", line, err)
			}
		}
		out := decode(t, a.Finish())
		if msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any); msg["content"] != "" {
			t.Fatalf("content = %v", msg["content"])
		}
	})

	t.Run("error chunk in stream returns descriptive error", func(t *testing.T) {
		a := NewAccumulator()
		err := a.Add([]byte(`{"error":{"message":"token rate limit reached","type":"rate_limit"}}`))
		if err == nil {
			t.Fatal("expected error for error chunk")
		}
		if !strings.Contains(err.Error(), "token rate limit reached") {
			t.Fatalf("error %v does not contain message", err)
		}

		a2 := NewAccumulator()
		err2 := a2.Add([]byte(`{"error":"context window exceeded"}`))
		if err2 == nil {
			t.Fatal("expected error for string error chunk")
		}
		if !strings.Contains(err2.Error(), "context window exceeded") {
			t.Fatalf("error %v does not contain message", err2)
		}
	})

	t.Run("finish reason defaults to tool_calls when tool calls present without finish_reason", func(t *testing.T) {
		a := NewAccumulator()
		for _, line := range []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"calc","arguments":"{}"}}]}}]}`,
		} {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		out := decode(t, a.Finish())
		if fr := out["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "tool_calls" {
			t.Fatalf("finish_reason = %v, want tool_calls", fr)
		}
	})

	t.Run("finish reason preserves explicit non-empty reason when tool calls present", func(t *testing.T) {
		a := NewAccumulator()
		for _, line := range []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"calc","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"finish_reason":"length"}]}`,
		} {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		out := decode(t, a.Finish())
		if fr := out["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "length" {
			t.Fatalf("finish_reason = %v, want length", fr)
		}
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

func TestExtractReasoningEffort(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			name:    "direct reasoning_effort",
			payload: map[string]any{"reasoning_effort": "max"},
			want:    "max",
		},
		{
			name:    "nested reasoning.effort",
			payload: map[string]any{"reasoning": map[string]any{"effort": "high"}},
			want:    "high",
		},
		{
			name:    "nested thinking.type",
			payload: map[string]any{"thinking": map[string]any{"type": "enabled"}},
			want:    "enabled",
		},
		{
			name:    "empty payload",
			payload: nil,
			want:    "",
		},
		{
			name:    "no effort fields",
			payload: map[string]any{"model": "deepseek-v4-flash"},
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractReasoningEffort(tc.payload); got != tc.want {
				t.Errorf("ExtractReasoningEffort() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeRequestReasoningEffort(t *testing.T) {
	// DeepSeek models (#111): nested reasoning.effort "max" is clamped (#65)
	// and sent as PLAIN reasoning_effort — no client-side thinking block; the
	// DeepSeek thinking translation is server-side.
	body := map[string]any{
		"model":     "deepseek/deepseek-v4-flash",
		"messages":  []any{map[string]any{"role": "user", "content": "hello"}},
		"reasoning": map[string]any{"effort": "max"},
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	if gotEff, ok := got["reasoning_effort"].(string); !ok || gotEff != "max" {
		t.Errorf("reasoning_effort = %v, want \"max\"", got["reasoning_effort"])
	}
	if _, ok := got["thinking"]; ok {
		t.Error("thinking block emitted for deepseek model")
	}

	// Non-DeepSeek models keep reasoning_effort (clamped to the model's
	// allowance when the model has one).
	body = map[string]any{
		"model":            "openai/gpt-5.6-luna",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning_effort": "max",
	}
	out, err = NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got = decode(t, out)
	if gotEff, ok := got["reasoning_effort"].(string); !ok || gotEff != "max" {
		t.Errorf("reasoning_effort = %v, want \"max\"", got["reasoning_effort"])
	}
	if _, ok := got["thinking"]; ok {
		t.Error("thinking block added for non-deepseek model")
	}
}

// TestSanitizeChunkBranchTable pins the branch-level edges of chunk
// sanitization: a non-string reasoning_content is dropped (only strings
// pass through), a fractional created is truncated to its integer part,
// and a choice carrying an empty delta is KEPT (the choice exists, so the
// chunk is not dropped).
func TestSanitizeChunkBranchTable(t *testing.T) {
	t.Run("non-string reasoning_content dropped", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"choices":[{"delta":{"content":"x","reasoning_content":123}}]}`))
		if drop {
			t.Fatal("chunk dropped")
		}
		got := decode(t, out)
		delta := got["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if _, ok := delta["reasoning_content"]; ok {
			t.Errorf("non-string reasoning_content kept: %v", delta)
		}
		if delta["content"] != "x" {
			t.Errorf("content = %v, want x", delta["content"])
		}
	})

	t.Run("fractional created truncated", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"created":123.7,"choices":[{"delta":{"content":"x"}}]}`))
		if drop {
			t.Fatal("chunk dropped")
		}
		got := decode(t, out)
		if got["created"] != float64(123) {
			t.Errorf("created = %v, want 123 (truncated, not rounded)", got["created"])
		}
	})

	t.Run("empty delta choice kept", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
		if drop {
			t.Fatal("choice with an empty delta must be kept")
		}
		got := decode(t, out)
		choice := got["choices"].([]any)[0].(map[string]any)
		if choice["finish_reason"] != "stop" {
			t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
		}
		if d := choice["delta"].(map[string]any); len(d) != 0 {
			t.Errorf("delta = %v, want empty", d)
		}
	})
}

// TestAccumulatorLaterFragmentToolCall pins the tool-call stitcher: id,
// type and function name may arrive on a LATER fragment (the first one
// carries only arguments); the accumulated arguments concatenate across
// fragments regardless of which fragment carries the metadata. A gap in
// tool-call indices sorts the assembled output by index (the index itself
// is not part of the output shape).
func TestAccumulatorLaterFragmentToolCall(t *testing.T) {
	t.Run("id and name on a later fragment", func(t *testing.T) {
		a := NewAccumulator()
		for _, line := range []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_later","type":"function","function":{"name":"fn_later","arguments":"1}"}}]}}]}`,
		} {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add(%q): %v", line, err)
			}
		}
		out := decode(t, a.Finish())
		calls := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
		if len(calls) != 1 {
			t.Fatalf("tool_calls = %v, want 1", calls)
		}
		first := calls[0].(map[string]any)
		if first["id"] != "call_later" || first["type"] != "function" {
			t.Errorf("tool call id/type = %v / %v, want call_later / function", first["id"], first["type"])
		}
		if fn := first["function"].(map[string]any); fn["name"] != "fn_later" || fn["arguments"] != `{"a":1}` {
			t.Errorf("tool call function = %v, want name fn_later args %q", fn, `{"a":1}`)
		}
	})

	t.Run("index gap sorted output", func(t *testing.T) {
		a := NewAccumulator()
		for _, line := range []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":3,"id":"c3","function":{"name":"f3","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c1","function":{"name":"f1","arguments":"{}"}}]}}]}`,
		} {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		out := decode(t, a.Finish())
		calls := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
		if len(calls) != 2 {
			t.Fatalf("tool_calls = %v, want 2", calls)
		}
		if id := calls[0].(map[string]any)["id"]; id != "c1" {
			t.Errorf("calls[0].id = %v, want c1 (sorted by index)", id)
		}
		if id := calls[1].(map[string]any)["id"]; id != "c3" {
			t.Errorf("calls[1].id = %v, want c3", id)
		}
	})
}

// TestAccumulatorReasoningOnlyFinish pins a reasoning-only stream: the
// Finish response carries the concatenated reasoning_content while content
// stays empty.
func TestAccumulatorReasoningOnlyFinish(t *testing.T) {
	a := NewAccumulator()
	for _, line := range []string{
		`{"choices":[{"index":0,"delta":{"reasoning_content":"think "}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"more"},"finish_reason":"stop"}]}`,
	} {
		if err := a.Add([]byte(line)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	out := decode(t, a.Finish())
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "" {
		t.Errorf("content = %v, want empty", msg["content"])
	}
	if msg["reasoning_content"] != "think more" {
		t.Errorf("reasoning_content = %v, want 'think more'", msg["reasoning_content"])
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

// TestParseSSEDataWhitespaceEdges pins the SSE data-line parser edges:
// leading whitespace is tolerated (on both "data:" and plain-JSON lines),
// a space between "data" and the colon is NOT a data line (it falls
// through to the plain-JSON check and is skipped), and extra spaces after
// the colon are trimmed.
func TestParseSSEDataWhitespaceEdges(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []byte
		ok   bool
	}{
		{"leading spaces before data", "  data: {\"a\":1}", []byte(`{"a":1}`), true},
		{"space before colon not matched", "data : {\"a\":1}", nil, false},
		{"extra space after colon", "data:  {\"a\":1}", []byte(`{"a":1}`), true},
		{"plain json", `{"a":1}`, []byte(`{"a":1}`), true},
		{"plain json with leading space", `  {"a":1}`, []byte(`{"a":1}`), true},
		{"blank", "", nil, false},
		{"comment", ": hi", nil, false},
		{"event field", "event: message", nil, false},
		{"data with empty payload", "data: ", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSSEData([]byte(tc.line))
			if ok != tc.ok || string(got) != string(tc.want) {
				t.Errorf("parseSSEData(%q) = %q, %v; want %q, %v", tc.line, got, ok, tc.want, tc.ok)
			}
		})
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
// Issue #63 — SSE fast path, sync.Pool reuse, benchmarks.
// ---------------------------------------------------------------------------

// TestSanitizeChunkFastPath pins the zero-allocation fast path: a chunk that
// already satisfies every sanitize invariant is relayed verbatim (the
// returned bytes alias the input payload — no re-encode), while a chunk
// needing defaults still takes the full sanitize path.
func TestSanitizeChunkFastPath(t *testing.T) {
	payload := `{"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","system_fingerprint":"fp","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null,"logprobs":{"a":1}}]}`
	line := []byte("data: " + payload)

	out, drop := SanitizeChunk(line)
	if drop || out == nil {
		t.Fatal("canonical chunk dropped")
	}
	// The fast path emits the raw payload byte-for-byte (skipping the
	// sanitize-map + marshal round trip).
	if string(out) != payload {
		t.Fatalf("fast path did not return the raw payload:\n got: %s\nwant: %s", out, payload)
	}
	got := decode(t, out)
	if got["id"] != "c1" || got["object"] != "chat.completion.chunk" || got["created"] != float64(5) || got["model"] != "m" {
		t.Fatalf("passthrough fields mangled: %v", got)
	}
	choice := got["choices"].([]any)[0].(map[string]any)
	if choice["index"] != float64(0) || choice["finish_reason"] != nil || choice["logprobs"] == nil {
		t.Fatalf("choice fields mangled: %v", choice)
	}

	// A chunk needing defaults still takes the sanitize path.
	out, drop = SanitizeChunk([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`))
	if drop || out == nil {
		t.Fatal("chunk dropped")
	}
	got = decode(t, out)
	if id, _ := got["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
		t.Fatalf("id = %v, want chatcmpl- prefix (sanitize path ran)", got["id"])
	}

	// A number that saturates int64 (1e20) is integral but does not
	// round-trip through numInt64: it must take the sanitize path (the exact
	// saturated output is platform-dependent) rather than the fast path
	// relaying the raw 1e20.
	out, drop = SanitizeChunk([]byte(`{"id":"c1","object":"chat.completion.chunk","created":1e20,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`))
	if drop || out == nil {
		t.Fatal("chunk dropped")
	}
	got = decode(t, out)
	if c, ok := got["created"].(float64); !ok || c == 1e20 {
		t.Errorf("created = %v, want sanitize-path output (fast path must not relay 1e20)", got["created"])
	}
}

func BenchmarkSanitizeChunkFastPath(b *testing.B) {
	line := []byte(`data: {"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, drop := SanitizeChunk(line)
		if drop || out == nil {
			b.Fatal("canonical chunk dropped")
		}
		_ = out
	}
}

func BenchmarkSanitizeChunkSanitizePath(b *testing.B) {
	line := []byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, drop := SanitizeChunk(line)
		if drop || out == nil {
			b.Fatal("chunk dropped")
		}
		_ = out
	}
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
	budget := maxSchemaNodes
	first := normalizeToolSchemaCached(params, &budget)
	if hits, misses := schemaCacheStats(); hits != 0 || misses != 1 {
		t.Fatalf("after first normalize: hits=%d misses=%d, want 0/1", hits, misses)
	}

	budget2 := maxSchemaNodes
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
	budget3 := maxSchemaNodes
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
// Issue #84 — DeepSeek prompt-cache cache_control injection.
// ---------------------------------------------------------------------------

func TestInjectCacheControl(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "u1"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "u2"},
			map[string]any{"type": "text", "text": "u3", "cache_control": map[string]any{"type": "existing"}},
		}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "u4"}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "u5"}}},
		map[string]any{"role": "user", "content": "plain string"}, // non-block content skipped
	}
	InjectCacheControl(messages)

	blocks := func(i int) []any {
		return messages[i].(map[string]any)["content"].([]any)
	}
	// Indices 2 and 3 (the stable context prefix) get ephemeral hints.
	if cc := blocks(2)[0].(map[string]any)["cache_control"]; cc == nil {
		t.Error("messages[2] block 0 missing cache_control")
	}
	if cc := blocks(2)[1].(map[string]any)["cache_control"].(map[string]any); cc["type"] != "existing" {
		t.Errorf("messages[2] block 1 cache_control overwritten: %v", cc)
	}
	if cc := blocks(3)[0].(map[string]any)["cache_control"]; cc == nil {
		t.Error("messages[3] block 0 missing cache_control")
	}
	// Everything outside indices 2-3 is untouched.
	if _, ok := messages[0].(map[string]any)["cache_control"]; ok {
		t.Error("messages[0] gained cache_control")
	}
	if _, ok := messages[1].(map[string]any)["cache_control"]; ok {
		t.Error("messages[1] gained cache_control")
	}
	if cc := blocks(4)[0].(map[string]any)["cache_control"]; cc != nil {
		t.Error("messages[4] gained cache_control (beyond the prefix window)")
	}
	if got := messages[5].(map[string]any)["content"]; got != "plain string" {
		t.Errorf("non-block content touched: %v", got)
	}
}

func TestNormalizeRequestCacheControlInjection(t *testing.T) {
	mkBody := func(model string) map[string]any {
		content := func(text string) []any {
			return []any{map[string]any{"type": "text", "text": text}}
		}
		return map[string]any{
			"model": model,
			"messages": []any{
				map[string]any{"role": "system", "content": "sys"},
				map[string]any{"role": "user", "content": "u1"},
				map[string]any{"role": "user", "content": content("u2")},
				map[string]any{"role": "user", "content": content("u3")},
			},
		}
	}
	hasHints := func(out []byte) bool {
		got := decode(t, out)
		for i, m := range got["messages"].([]any) {
			if i < 2 || i > 3 {
				continue
			}
			blocks, _ := m.(map[string]any)["content"].([]any)
			for _, b := range blocks {
				if _, ok := b.(map[string]any)["cache_control"]; ok {
					return true
				}
			}
		}
		return false
	}

	t.Run("default on for deepseek", func(t *testing.T) {
		t.Setenv("CACHE_CONTROL_INJECTION", "")
		out, err := NormalizeRequest(mustJSON(t, mkBody("deepseek/deepseek-v4-flash")), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		if !hasHints(out) {
			t.Error("deepseek request without cache_control hints")
		}
	})

	t.Run("disabled via env", func(t *testing.T) {
		t.Setenv("CACHE_CONTROL_INJECTION", "false")
		out, err := NormalizeRequest(mustJSON(t, mkBody("deepseek/deepseek-v4-flash")), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		if hasHints(out) {
			t.Error("cache_control injected with CACHE_CONTROL_INJECTION=false")
		}
	})

	t.Run("non-deepseek model untouched", func(t *testing.T) {
		t.Setenv("CACHE_CONTROL_INJECTION", "")
		out, err := NormalizeRequest(mustJSON(t, mkBody("minimax/minimax-m3")), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		if hasHints(out) {
			t.Error("cache_control injected for non-deepseek model")
		}
	})
}

// ---------------------------------------------------------------------------
// Issue #111 — plain reasoning_effort on DeepSeek routes (no thinking block).
// ---------------------------------------------------------------------------

func TestDeepSeekPlainReasoningEffort(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		effort string
		want   string
	}{
		{"flash low stays low", "deepseek/deepseek-v4-flash", "low", "low"},
		{"flash medium rewrites to high", "deepseek/deepseek-v4-flash", "medium", "high"},
		{"flash high stays high", "deepseek/deepseek-v4-flash", "high", "high"},
		{"flash max stays max", "deepseek/deepseek-v4-flash", "max", "max"},
		{"flash xhigh clamps down to high", "deepseek/deepseek-v4-flash", "xhigh", "high"},
		{"pro low stays low", "deepseek/deepseek-v4-pro", "low", "low"},
		{"pro medium rewrites to high", "deepseek/deepseek-v4-pro", "medium", "high"},
		{"pro max stays max", "deepseek/deepseek-v4-pro", "max", "max"},
		{"bare model id tolerated", "deepseek-v4-flash", "max", "max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"model":            tc.model,
				"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
				"reasoning_effort": tc.effort,
			}
			out, err := NormalizeRequest(mustJSON(t, body), "")
			if err != nil {
				t.Fatalf("NormalizeRequest: %v", err)
			}
			got := decode(t, out)
			if gotEff, ok := got["reasoning_effort"].(string); !ok || gotEff != tc.want {
				t.Errorf("reasoning_effort = %v, want %q", got["reasoning_effort"], tc.want)
			}
			if _, ok := got["thinking"]; ok {
				t.Errorf("thinking block emitted for deepseek model: %v", got["thinking"])
			}
		})
	}

	t.Run("reasoning.enabled=false suppresses effort", func(t *testing.T) {
		body := map[string]any{
			"model":     "deepseek/deepseek-v4-flash",
			"messages":  []any{map[string]any{"role": "user", "content": "hi"}},
			"reasoning": map[string]any{"enabled": false},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		if _, ok := got["reasoning_effort"]; ok {
			t.Error("reasoning_effort kept alongside reasoning.enabled=false")
		}
		if _, ok := got["thinking"]; ok {
			t.Error("thinking block emitted alongside reasoning.enabled=false")
		}
	})

	t.Run("thinking.type=disabled suppresses effort", func(t *testing.T) {
		body := map[string]any{
			"model":            "deepseek/deepseek-v4-flash",
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
			"reasoning_effort": "max",
			"thinking":         map[string]any{"type": "disabled"},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		if _, ok := got["reasoning_effort"]; ok {
			t.Error("reasoning_effort kept alongside thinking.type=disabled")
		}
		if _, ok := got["thinking"]; ok {
			t.Error("thinking block emitted alongside thinking.type=disabled")
		}
	})

	t.Run("no effort requested adds no thinking", func(t *testing.T) {
		body := map[string]any{
			"model":    "deepseek/deepseek-v4-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		if _, ok := got["thinking"]; ok {
			t.Errorf("thinking block added without an effort request: %v", got["thinking"])
		}
	})
}

// ---------------------------------------------------------------------------
// Issue #65 — per-model reasoning effort clamping.
// ---------------------------------------------------------------------------

func TestClampReasoningEffort(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		allowed   []string
		want      string
	}{
		{"max within allowance", "max", []string{"low", "high", "max"}, "max"},
		{"high within allowance", "high", []string{"low", "high", "max"}, "high"},
		{"medium clamps down to low", "medium", []string{"low", "high", "max"}, "low"},
		{"all above requested gives lowest", "low", []string{"high", "max"}, "high"},
		{"below everything gives lowest", "minimal", []string{"low", "high", "max"}, "low"},
		{"unknown effort falls back", "banana", []string{"low", "high", "max"}, defaultReasoningEffort},
		{"empty requested falls back", "", []string{"low"}, defaultReasoningEffort},
		{"nil allowance falls back", "high", nil, defaultReasoningEffort},
		{"empty allowance falls back", "high", []string{}, defaultReasoningEffort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampReasoningEffort(tc.requested, tc.allowed, defaultReasoningEffort); got != tc.want {
				t.Errorf("clampReasoningEffort(%q, %v) = %q, want %q", tc.requested, tc.allowed, got, tc.want)
			}
		})
	}
}

func TestEffortsForModel(t *testing.T) {
	if got := effortsForModel("deepseek/deepseek-v4-flash"); !reflect.DeepEqual(got, []string{"low", "high", "max"}) {
		t.Errorf("flash efforts = %v", got)
	}
	if got := effortsForModel("deepseek/deepseek-v4-pro"); !reflect.DeepEqual(got, []string{"low", "high", "max"}) {
		t.Errorf("pro efforts = %v", got)
	}
	// Luna EFFORTS_THROUGH_MAX includes xhigh; muse EFFORTS_THROUGH_XHIGH
	// includes minimal (08/13 catalog).
	if got := effortsForModel("openai/gpt-5.6-luna"); !reflect.DeepEqual(got, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Errorf("luna efforts = %v", got)
	}
	if got := effortsForModel("meta/muse-spark-1.2-contributor"); !reflect.DeepEqual(got, []string{"minimal", "low", "medium", "high", "xhigh"}) {
		t.Errorf("muse efforts = %v", got)
	}
	// Unlisted models get the full ladder (no clamping).
	if got := effortsForModel("minimax/minimax-m3"); !reflect.DeepEqual(got, reasoningLadder[:]) {
		t.Errorf("unlisted efforts = %v, want full ladder", got)
	}

	// Runtime override (registry data when present), nil → hardcoded table.
	SetModelEffortLookup(func(model string) []string {
		if model == "custom/model" {
			return []string{"low"}
		}
		return nil
	})
	t.Cleanup(func() { SetModelEffortLookup(nil) })
	if got := effortsForModel("custom/model"); !reflect.DeepEqual(got, []string{"low"}) {
		t.Errorf("overridden efforts = %v, want [low]", got)
	}
	if got := effortsForModel("deepseek/deepseek-v4-flash"); !reflect.DeepEqual(got, []string{"low", "high", "max"}) {
		t.Errorf("nil override must fall back to the table, got %v", got)
	}
}

func TestNormalizeRequestEffortClamp(t *testing.T) {
	effortFor := func(model, effort string) string {
		body := map[string]any{
			"model":            model,
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
			"reasoning_effort": effort,
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		return decode(t, out)["reasoning_effort"].(string)
	}

	// gpt-5.6-luna EFFORTS_THROUGH_MAX includes xhigh: it passes through.
	if got := effortFor("openai/gpt-5.6-luna", "xhigh"); got != "xhigh" {
		t.Errorf("gpt-5.6-luna xhigh = %q, want xhigh", got)
	}
	// muse-spark EFFORTS_THROUGH_XHIGH includes minimal: it passes through.
	if got := effortFor("meta/muse-spark-1.2-contributor", "minimal"); got != "minimal" {
		t.Errorf("muse-spark minimal = %q, want minimal", got)
	}
	// deepseek-v4-pro gained low on 08/13: it passes through unclamped.
	if got := effortFor("deepseek/deepseek-v4-pro", "low"); got != "low" {
		t.Errorf("deepseek-v4-pro low = %q, want low", got)
	}
	// medium on a DeepSeek route rewrites to high (resolveFreebuffReasoningEffort),
	// never down to low.
	if got := effortFor("deepseek/deepseek-v4-flash", "medium"); got != "high" {
		t.Errorf("deepseek-v4-flash medium = %q, want high", got)
	}
	// Unlisted models pass every rung through.
	if got := effortFor("minimax/minimax-m3", "ultra"); got != "ultra" {
		t.Errorf("minimax-m3 ultra = %q, want ultra", got)
	}
	// Unrecognized effort falls back to the default.
	if got := effortFor("openai/gpt-5.6-luna", "banana"); got != defaultReasoningEffort {
		t.Errorf("unknown effort = %q, want %q", got, defaultReasoningEffort)
	}
}

// ---------------------------------------------------------------------------
// Issue #44 — reasoning folded into delta.content for legacy clients.
// ---------------------------------------------------------------------------

func TestReasoningInContent(t *testing.T) {
	canonical := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"","reasoning_content":"think step"},"finish_reason":null}]}`
	deltaOf := func(out []byte) map[string]any {
		got := decode(t, out)
		return got["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	}

	t.Run("default off", func(t *testing.T) {
		t.Setenv("REASONING_IN_CONTENT", "")
		out, drop := SanitizeChunk([]byte(canonical))
		if drop || out == nil {
			t.Fatal("chunk dropped")
		}
		delta := deltaOf(out)
		if delta["content"] != "" {
			t.Errorf("content = %v, want empty (no fold when off)", delta["content"])
		}
		if delta["reasoning_content"] != "think step" {
			t.Errorf("reasoning_content = %v, want preserved", delta["reasoning_content"])
		}
	})

	t.Run("enabled folds into content", func(t *testing.T) {
		t.Setenv("REASONING_IN_CONTENT", "true")
		out, drop := SanitizeChunk([]byte(canonical))
		if drop || out == nil {
			t.Fatal("chunk dropped")
		}
		delta := deltaOf(out)
		if delta["content"] != "<think>think step</think>" {
			t.Errorf("content = %v, want folded think text", delta["content"])
		}
		if delta["reasoning_content"] != "think step" {
			t.Errorf("reasoning_content = %v, want preserved alongside the fold", delta["reasoning_content"])
		}
	})

	t.Run("custom tag label", func(t *testing.T) {
		t.Setenv("REASONING_IN_CONTENT", "thinking")
		out, drop := SanitizeChunk([]byte(canonical))
		if drop || out == nil {
			t.Fatal("chunk dropped")
		}
		if c := deltaOf(out)["content"]; c != "<thinking>think step</thinking>" {
			t.Errorf("content = %v, want the custom tag label", c)
		}
	})

	t.Run("fold precedes existing text", func(t *testing.T) {
		t.Setenv("REASONING_IN_CONTENT", "true")
		line := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"answer","reasoning_content":"r"},"finish_reason":null}]}`
		out, drop := SanitizeChunk([]byte(line))
		if drop || out == nil {
			t.Fatal("chunk dropped")
		}
		if c := deltaOf(out)["content"]; c != "<think>r</think>answer" {
			t.Errorf("content = %v, want reasoning before text", c)
		}
	})

	t.Run("reasoning_details never folded", func(t *testing.T) {
		t.Setenv("REASONING_IN_CONTENT", "true")
		line := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"","reasoning_content":"r","reasoning_details":[{"type":"signature","value":"sig"}]},"finish_reason":null}]}`
		out, drop := SanitizeChunk([]byte(line))
		if drop || out == nil {
			t.Fatal("chunk dropped")
		}
		delta := deltaOf(out)
		if c := delta["content"]; c != "<think>r</think>" {
			t.Errorf("content = %v, want folded", c)
		}
		details, ok := delta["reasoning_details"].([]any)
		if !ok || len(details) != 1 {
			t.Fatalf("reasoning_details not replayed verbatim: %v", delta["reasoning_details"])
		}
		if d := details[0].(map[string]any); d["type"] != "signature" || d["value"] != "sig" {
			t.Errorf("reasoning_details mangled: %v", details)
		}
	})
}

func TestAccumulatorReasoningInContent(t *testing.T) {
	t.Setenv("REASONING_IN_CONTENT", "true")
	a := NewAccumulator()
	for _, line := range []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"Hel","reasoning_content":"think "}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"lo","reasoning_content":"more"},"finish_reason":"stop"}]}`,
	} {
		if err := a.Add([]byte(line)); err != nil {
			t.Fatalf("Add(%q): %v", line, err)
		}
	}
	out := decode(t, a.Finish())
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "<think>think more</think>Hello" {
		t.Errorf("content = %v, want folded reasoning before text", msg["content"])
	}
	if msg["reasoning_content"] != "think more" {
		t.Errorf("reasoning_content = %v, want preserved", msg["reasoning_content"])
	}
}

// ---------------------------------------------------------------------------
// Issue #58 — optional prompt & context compression.
// ---------------------------------------------------------------------------

func TestCompressMessages(t *testing.T) {
	shrinkBudget := func(t *testing.T, keepLast int) {
		t.Helper()
		old := compressKeepLast
		compressKeepLast = keepLast
		t.Cleanup(func() { compressKeepLast = old })
	}
	msg := func(role, content string) map[string]any {
		return map[string]any{"role": role, "content": content}
	}

	t.Run("budget enforcement with marker", func(t *testing.T) {
		shrinkBudget(t, 4)
		msgs := []any{
			msg("system", "sys"),
			msg("user", "u1"),
			msg("assistant", "a1"),
			msg("user", "u2"),
			msg("assistant", "a2"),
			msg("user", "u3"),
			msg("assistant", "a3"),
			msg("user", "u4"),
		}
		got, dropped := compressMessages(msgs)
		if dropped != 3 {
			t.Fatalf("dropped = %d, want 3 (u1, a1, u2)", dropped)
		}
		// 8 messages - 3 dropped + 1 marker = 6.
		if len(got) != 6 {
			t.Fatalf("len = %d, want 6", len(got))
		}
		marker := got[1].(map[string]any)
		if marker["role"] != "system" {
			t.Errorf("marker role = %v, want system", marker["role"])
		}
		if marker["content"] != "[truncated by freebuff-proxy compression; 3 earlier messages omitted]" {
			t.Errorf("marker content = %v", marker["content"])
		}
		// The trailing window (last 4 messages) is preserved verbatim.
		for i, want := range []string{"a2", "u3", "a3", "u4"} {
			if got[2+i].(map[string]any)["content"] != want {
				t.Errorf("trailing message %d = %v, want %q", i, got[2+i], want)
			}
		}
	})

	t.Run("tool messages never dropped", func(t *testing.T) {
		shrinkBudget(t, 4)
		toolMsg := msg("tool", "result")
		assistantCall := map[string]any{
			"role":       "assistant",
			"content":    "",
			"tool_calls": []any{map[string]any{"id": "call_1", "type": "function"}},
		}
		msgs := []any{
			msg("system", "sys"),
			msg("user", "u1"),
			assistantCall,
			toolMsg,
			msg("user", "u2"),
			msg("assistant", "a1"),
			msg("user", "u3"),
		}
		got, dropped := compressMessages(msgs)
		if dropped != 1 {
			t.Fatalf("dropped = %d, want 1 (only u1; tool call/result survive)", dropped)
		}
		seenCall, seenTool := false, false
		for _, m := range got {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if _, has := mm["tool_calls"]; has {
				seenCall = true
			}
			if mm["role"] == "tool" {
				seenTool = true
			}
		}
		if !seenCall || !seenTool {
			t.Errorf("tool call/result lost: seenCall=%v seenTool=%v", seenCall, seenTool)
		}
		// The tool result's content is never truncated.
		if got[len(got)-1].(map[string]any)["role"] == "tool" {
			t.Error("last message should be the user's current turn")
		}
	})

	t.Run("last message never dropped or truncated", func(t *testing.T) {
		shrinkBudget(t, 3)
		long := strings.Repeat("x", 4096)
		msgs := []any{
			msg("system", "sys"),
			msg("user", "u1"),
			msg("assistant", "a1"),
			msg("user", long),
			msg("user", long),
		}
		got, _ := compressMessages(msgs)
		last := got[len(got)-1].(map[string]any)
		if last["content"] != long {
			t.Error("last (current) message was truncated")
		}
	})

	t.Run("content cap with marker", func(t *testing.T) {
		shrinkBudget(t, 10) // no middle drops; capping still applies
		oldBytes := compressMaxContentBytes
		compressMaxContentBytes = 16
		t.Cleanup(func() { compressMaxContentBytes = oldBytes })

		long := strings.Repeat("x", 100)
		msgs := []any{
			msg("system", "sys"),
			msg("user", long),
			msg("user", long), // the last message: never truncated
		}
		got, dropped := compressMessages(msgs)
		if dropped != 0 {
			t.Fatalf("dropped = %d, want 0", dropped)
		}
		capped := got[1].(map[string]any)["content"].(string)
		if !strings.HasPrefix(capped, "xxxxxxxx") || !strings.HasSuffix(capped, compressContentMarker) {
			t.Errorf("capped content = %q, want prefix + marker", capped)
		}
		if len(capped) >= 100 {
			t.Errorf("capped content not truncated: len %d", len(capped))
		}
		if got[2].(map[string]any)["content"] != long {
			t.Error("last message truncated")
		}
	})

	t.Run("short conversation untouched", func(t *testing.T) {
		shrinkBudget(t, 4)
		msgs := []any{msg("system", "sys"), msg("user", "u1"), msg("assistant", "a1")}
		got, dropped := compressMessages(msgs)
		if dropped != 0 || len(got) != len(msgs) {
			t.Fatalf("short conversation changed: dropped=%d len=%d", dropped, len(got))
		}
		for i := range msgs {
			if got[i].(map[string]any)["content"] != msgs[i].(map[string]any)["content"] {
				t.Errorf("message %d not identical", i)
			}
		}
	})
}

func TestNormalizeRequestCompression(t *testing.T) {
	old := compressKeepLast
	compressKeepLast = 4
	t.Cleanup(func() { compressKeepLast = old })

	body := map[string]any{
		"model":    "m",
		"messages": []any{},
	}
	for i := 0; i < 8; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		body["messages"] = append(body["messages"].([]any),
			map[string]any{"role": role, "content": fmt.Sprintf("msg-%d", i)})
	}

	t.Setenv("COMPRESS_PROMPT", "true")
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	msgs := got["messages"].([]any)
	// keepLast=4 with 8 messages: indices 0-3 are all plain user/assistant
	// turns (the first message is NOT the system prompt, so it is droppable)
	// → 4 dropped, replaced by one marker: 8 - 4 + 1 = 5.
	if len(msgs) != 5 {
		t.Fatalf("compressed messages = %d, want 5 (8 - 4 dropped + 1 marker)", len(msgs))
	}
	if !strings.Contains(msgs[0].(map[string]any)["content"].(string), "4 earlier messages omitted") {
		t.Errorf("marker missing: %v", msgs[0])
	}

	t.Setenv("COMPRESS_PROMPT", "false")
	out, err = NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	if got := decode(t, out); len(got["messages"].([]any)) != 8 {
		t.Errorf("COMPRESS_PROMPT=false still compressed: %d messages", len(got["messages"].([]any)))
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
		budget := maxSchemaNodes
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

func TestExtractXMLToolCalls(t *testing.T) {
	t.Run("hermes xml format", func(t *testing.T) {
		raw := "Let me run the command:\n<tool_call>\n<function=bash>\n<parameter=command>rtk --version 2>&1</parameter>\n</function>\n</tool_call>"
		cleaned, calls := extractXMLToolCalls(raw)
		if cleaned != "Let me run the command:" {
			t.Errorf("cleaned = %q, want 'Let me run the command:'", cleaned)
		}
		if len(calls) != 1 {
			t.Fatalf("calls len = %d, want 1", len(calls))
		}
		if calls[0].Function.Name != "bash" {
			t.Errorf("name = %q, want 'bash'", calls[0].Function.Name)
		}
		var argsMap map[string]string
		if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &argsMap); err != nil {
			t.Fatalf("unmarshal args: %v", err)
		}
		if argsMap["command"] != "rtk --version 2>&1" {
			t.Errorf("command = %q, want 'rtk --version 2>&1'", argsMap["command"])
		}
	})

	t.Run("json in tool_call tag", func(t *testing.T) {
		raw := "<tool_call>\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jakarta\"}}\n</tool_call>"
		cleaned, calls := extractXMLToolCalls(raw)
		if cleaned != "" {
			t.Errorf("cleaned = %q, want empty", cleaned)
		}
		if len(calls) != 1 {
			t.Fatalf("calls len = %d, want 1", len(calls))
		}
		if calls[0].Function.Name != "get_weather" {
			t.Errorf("name = %q, want 'get_weather'", calls[0].Function.Name)
		}
	})
	t.Run("fenced code block tool call", func(t *testing.T) {
		raw := "I will list the directory:\n```tool_call\n{\"name\": \"bash\", \"arguments\": {\"command\": \"ls -la\"}}\n```"
		cleaned, calls := extractXMLToolCalls(raw)
		if cleaned != "I will list the directory:" {
			t.Errorf("cleaned = %q, want 'I will list the directory:'", cleaned)
		}
		if len(calls) != 1 {
			t.Fatalf("calls len = %d, want 1", len(calls))
		}
		if calls[0].Function.Name != "bash" {
			t.Errorf("name = %q, want 'bash'", calls[0].Function.Name)
		}
	})
}

func TestAccumulatorReasoningToolCallFallback(t *testing.T) {
	a := NewAccumulator()
	line := `{"choices":[{"index":0,"delta":{"content":"","reasoning_content":"Thinking... I should run command:\n<tool_call>\n<function=bash>\n<parameter=command>pwd</parameter>\n</function>\n</tool_call>"}}]}`
	if err := a.Add([]byte(line)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out := decode(t, a.Finish())
	choice := out["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want 'tool_calls'", choice["finish_reason"])
	}
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %v, want 1 tool call", msg["tool_calls"])
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Errorf("fn name = %v, want 'bash'", fn["name"])
	}
}

func TestAccumulatorXMLToolCallFinish(t *testing.T) {
	a := NewAccumulator()
	line := `{"choices":[{"index":0,"delta":{"content":"<tool_call>\n<function=bash>\n<parameter=command>ls -la</parameter>\n</function>\n</tool_call>"}}]}`
	if err := a.Add([]byte(line)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out := decode(t, a.Finish())
	choice := out["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want 'tool_calls'", choice["finish_reason"])
	}
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %v, want 1 tool call", msg["tool_calls"])
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Errorf("fn name = %v, want 'bash'", fn["name"])
	}
}

func TestNormalizeRequest_AssistantToolCallContentNull(t *testing.T) {
	t.Run("empty string content with tool_calls becomes null", func(t *testing.T) {
		body := map[string]any{
			"model": "gpt-4o",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name":      "get_weather",
								"arguments": `{"city":"Tokyo"}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		if !bytes.Contains(out, []byte(`"content":null`)) {
			t.Fatalf("expected output to contain '\"content\":null', got: %s", string(out))
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["content"] != nil {
			t.Errorf("msg.content = %v, want nil", msg["content"])
		}
	})

	t.Run("nil content with tool_calls remains null", func(t *testing.T) {
		body := map[string]any{
			"model": "gpt-4o",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{
						map[string]any{
							"id":   "call_2",
							"type": "function",
							"function": map[string]any{
								"name":      "calc",
								"arguments": `{}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		if !bytes.Contains(out, []byte(`"content":null`)) {
			t.Fatalf("expected output to contain '\"content\":null', got: %s", string(out))
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["content"] != nil {
			t.Errorf("msg.content = %v, want nil", msg["content"])
		}
	})

	t.Run("non-empty content with tool_calls is preserved", func(t *testing.T) {
		body := map[string]any{
			"model": "gpt-4o",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "calling tool now",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_3",
							"type": "function",
							"function": map[string]any{
								"name":      "calc",
								"arguments": `{}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["content"] != "calling tool now" {
			t.Errorf("msg.content = %v, want 'calling tool now'", msg["content"])
		}
	})

	t.Run("empty content without tool_calls remains empty string", func(t *testing.T) {
		body := map[string]any{
			"model": "gpt-4o",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "",
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["content"] != "" {
			t.Errorf("msg.content = %v, want empty string", msg["content"])
		}
	})
}

func TestNormalizeRequest_LeakedThinkTagExtraction(t *testing.T) {
	t.Run("leaked think tag extracted to reasoning_content with tool_calls", func(t *testing.T) {
		body := map[string]any{
			"model": "mimo/mimo-v2.5",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "<think>foo</think>",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name":      "bash",
								"arguments": `{"command":"ls"}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["reasoning_content"] != "foo" {
			t.Errorf("reasoning_content = %v, want 'foo'", msg["reasoning_content"])
		}
		if msg["content"] != nil {
			t.Errorf("content = %v, want nil", msg["content"])
		}
	})

	t.Run("leaked think tag extracted leaving remaining content", func(t *testing.T) {
		body := map[string]any{
			"model": "mimo/mimo-v2.5",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "<think>planning steps</think>Here is my plan",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_2",
							"type": "function",
							"function": map[string]any{
								"name":      "bash",
								"arguments": `{"command":"pwd"}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["reasoning_content"] != "planning steps" {
			t.Errorf("reasoning_content = %v, want 'planning steps'", msg["reasoning_content"])
		}
		if msg["content"] != "Here is my plan" {
			t.Errorf("content = %v, want 'Here is my plan'", msg["content"])
		}
	})

	t.Run("other think tags variants", func(t *testing.T) {
		for tag, raw := range map[string]string{
			"thinking":       "<thinking>reason A</thinking>",
			"reasoning":      "<reasoning>reason B</reasoning>",
			"antml:thinking": "<antml:thinking>reason C</antml:thinking>",
		} {
			body := map[string]any{
				"model": "gpt-4o",
				"messages": []any{
					map[string]any{
						"role":    "assistant",
						"content": raw,
					},
				},
			}
			out, err := NormalizeRequest(mustJSON(t, body), "")
			if err != nil {
				t.Fatalf("NormalizeRequest (%s): %v", tag, err)
			}
			got := decode(t, out)
			msgs := got["messages"].([]any)
			msg := msgs[0].(map[string]any)
			if msg["reasoning_content"] == "" {
				t.Errorf("tag %s: expected reasoning_content to be extracted, got empty", tag)
			}
		}
	})
}

func TestNormalizeRequest_ReasoningLookupRestoration(t *testing.T) {
	defer SetReasoningLookup(nil)

	SetReasoningLookup(func(toolID string, content, toolCallsJSON string) (string, string, bool) {
		if toolID == "call_abc123" {
			return "restored reasoning for call_abc123", "sig_123", true
		}
		if strings.Contains(toolCallsJSON, "fallback_tool") {
			return "restored reasoning via json", "sig_json", true
		}
		return "", "", false
	})

	t.Run("restores reasoning_content by tool call id", func(t *testing.T) {
		body := map[string]any{
			"model": "mimo/mimo-v2.5",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_abc123",
							"type": "function",
							"function": map[string]any{
								"name":      "bash",
								"arguments": `{"command":"whoami"}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["reasoning_content"] != "restored reasoning for call_abc123" {
			t.Errorf("reasoning_content = %v, want 'restored reasoning for call_abc123'", msg["reasoning_content"])
		}
		if msg["content"] != nil {
			t.Errorf("content = %v, want nil", msg["content"])
		}
	})

	t.Run("restores reasoning_content by toolCalls JSON fallback", func(t *testing.T) {
		body := map[string]any{
			"model": "mimo/mimo-v2.5",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{
						map[string]any{
							"id":   "unknown_id",
							"type": "function",
							"function": map[string]any{
								"name":      "fallback_tool",
								"arguments": `{}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["reasoning_content"] != "restored reasoning via json" {
			t.Errorf("reasoning_content = %v, want 'restored reasoning via json'", msg["reasoning_content"])
		}
	})

	t.Run("strict reasoning model sets empty reasoning_content when lookup misses", func(t *testing.T) {
		body := map[string]any{
			"model": "mimo/mimo-v2.5",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{
						map[string]any{
							"id":   "not_cached_id",
							"type": "function",
							"function": map[string]any{
								"name":      "other_tool",
								"arguments": `{}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		rc, ok := msg["reasoning_content"].(string)
		if !ok || rc != "" {
			t.Errorf("reasoning_content = %v (ok=%v), want empty string \"\"", msg["reasoning_content"], ok)
		}
	})
}

func TestNormalizeRequest_MiMoReasoningLadder(t *testing.T) {
	for _, model := range []string{"mimo/mimo-v2.5", "mimo/mimo-v2.5-pro"} {
		for _, reqEffort := range []string{"low", "medium", "high", "max"} {
			body := map[string]any{
				"model":            model,
				"reasoning_effort": reqEffort,
				"messages": []any{
					map[string]any{"role": "user", "content": "hi"},
				},
			}
			out, err := NormalizeRequest(mustJSON(t, body), "")
			if err != nil {
				t.Fatalf("NormalizeRequest(%s, %s): %v", model, reqEffort, err)
			}
			got := decode(t, out)
			if got["reasoning_effort"] != "high" {
				t.Errorf("model %s with requested effort %s got %v, want 'high'", model, reqEffort, got["reasoning_effort"])
			}
		}
	}

	for _, eff := range []string{"low", "medium", "high", "xhigh", "max"} {
		body := map[string]any{
			"model":            "anthropic/claude-fable-5",
			"reasoning_effort": eff,
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest(claude-fable-5, %s): %v", eff, err)
		}
		got := decode(t, out)
		if got["reasoning_effort"] != eff {
			t.Errorf("model anthropic/claude-fable-5 with requested effort %s got %v, want %s", eff, got["reasoning_effort"], eff)
		}
	}
}

func TestAccumulator_ToolCallInsideThinkTagFallback(t *testing.T) {
	a := NewAccumulator()
	chunk := `{"choices":[{"index":0,"delta":{"content":"","reasoning_content":"Thinking about what to do...\n<tool_call>\n<function=bash>\n<parameter=command>ls -la</parameter>\n</function>\n</tool_call>"}}]}`
	if err := a.Add([]byte(chunk)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	raw := a.Finish()
	out := decode(t, raw)
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want 'tool_calls'", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != nil {
		t.Errorf("content = %v, want nil", msg["content"])
	}
	if !bytes.Contains(raw, []byte(`"content":null`)) {
		t.Errorf("raw JSON expected '\"content\":null', got: %s", string(raw))
	}
	rc, _ := msg["reasoning_content"].(string)
	if strings.Contains(rc, "<tool_call>") || strings.Contains(rc, "</tool_call>") {
		t.Errorf("reasoning_content still contains tool_call tags: %q", rc)
	}
	if !strings.Contains(rc, "Thinking about what to do...") {
		t.Errorf("reasoning_content missing thought prefix: %q", rc)
	}
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %v, want 1 tool call", msg["tool_calls"])
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Errorf("fn name = %v, want 'bash'", fn["name"])
	}
	if fn["arguments"] != `{"command":"ls -la"}` {
		t.Errorf("fn arguments = %v, want '{\"command\":\"ls -la\"}'", fn["arguments"])
	}
}

func TestAccumulator_AssistantToolCallContentNull(t *testing.T) {
	a := NewAccumulator()
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_99","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":""}}]}`,
		`{"choices":[{"index":0,"finish_reason":"tool_calls"}]}`,
	}
	for _, chunk := range chunks {
		if err := a.Add([]byte(chunk)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	raw := a.Finish()
	if !bytes.Contains(raw, []byte(`"content":null`)) {
		t.Fatalf("expected output to contain '\"content\":null', got: %s", string(raw))
	}
	out := decode(t, raw)
	choice := out["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != nil {
		t.Errorf("msg.content = %v, want nil", msg["content"])
	}
	calls := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("len(tool_calls) = %d, want 1", len(calls))
	}
}
