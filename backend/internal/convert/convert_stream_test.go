package convert

import (
	"strings"
	"testing"
)

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
			return
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
			return
		}
		if !strings.Contains(err.Error(), "token rate limit reached") {
			t.Fatalf("error %v does not contain message", err)
		}

		a2 := NewAccumulator()
		err2 := a2.Add([]byte(`{"error":"context window exceeded"}`))
		if err2 == nil {
			t.Fatal("expected error for string error chunk")
			return
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
		return
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
		return
	}

	// A chunk needing defaults still takes the sanitize path.
	out, drop = SanitizeChunk([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`))
	if drop || out == nil {
		t.Fatal("chunk dropped")
		return
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
		return
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
			return
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
			return
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
			return
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
			return
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
			return
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
