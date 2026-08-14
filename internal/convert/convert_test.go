package convert

import (
	"encoding/json"
	"fmt"
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
	out, err := NormalizeRequest(mustJSON(t, body))
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
	out, err = NormalizeRequest([]byte(`{"model":"m","messages":[],"temperature":null}`))
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
	out, err := NormalizeRequest(mustJSON(t, body))
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
			if _, err := NormalizeRequest([]byte(body)); err == nil {
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
			name: "ref with siblings not resolved, definitions dropped",
			params: map[string]any{
				"$ref":        "#/definitions/Args",
				"description": "d",
				"definitions": map[string]any{"Args": map[string]any{"type": "object"}},
			},
			want: map[string]any{"$ref": "#/definitions/Args", "description": "d"},
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
			out, err := NormalizeRequest(mustJSON(t, body))
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
	out, err := NormalizeRequest(mustJSON(t, body))
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
	out, err = NormalizeRequest(mustJSON(t, body))
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
	out, err := NormalizeRequest(mustJSON(t, body))
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

	out, err := NormalizeRequest(mustJSON(t, body))
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
