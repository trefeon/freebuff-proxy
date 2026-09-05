package convert

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

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

	// Issue #144: the upstream's own canonical XML tag (codebuff_tool_call,
	// common/src/tools/constants.ts) must be extracted like the model-native
	// Hermes/Qwen/MiMo formats — models stream it when they ignore the stop
	// sequences, and the CLI parses it with util/stream-xml-parser.ts.
	t.Run("codebuff_tool_call xml format", func(t *testing.T) {
		raw := "Plan:\n<codebuff_tool_call>\n<function=bash>\n<parameter=command>pwd</parameter>\n</function>\n</codebuff_tool_call>"
		cleaned, calls := extractXMLToolCalls(raw)
		if cleaned != "Plan:" {
			t.Errorf("cleaned = %q, want 'Plan:'", cleaned)
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
		if argsMap["command"] != "pwd" {
			t.Errorf("command = %q, want 'pwd'", argsMap["command"])
		}
	})

	// Pip/wrap-wrapped MiMo format exercises the regex's 4th capture group
	// (the fallback chain after codebuff_tool_call was added); a group-shift
	// regression would return the raw text with no call.
	t.Run("pipe-wrapped mimo format", func(t *testing.T) {
		raw := "I will help:\n<|tool_call_start|>\n<function=bash>\n<parameter=command>echo hi</parameter>\n</function>\n<|tool_call_end|>"
		cleaned, calls := extractXMLToolCalls(raw)
		if cleaned != "I will help:" {
			t.Errorf("cleaned = %q, want 'I will help:'", cleaned)
		}
		if len(calls) != 1 {
			t.Fatalf("calls len = %d, want 1", len(calls))
		}
		if calls[0].Function.Name != "bash" {
			t.Errorf("name = %q, want 'bash'", calls[0].Function.Name)
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

// feedAll runs fragments through one XMLToolCallExtractor (Feed in order,
// Flush at end), concatenating safe text and calls in stream order.
func feedAll(t *testing.T, frags ...string) (string, []*toolCall) {
	t.Helper()
	var x XMLToolCallExtractor
	var text strings.Builder
	var calls []*toolCall
	for _, f := range frags {
		tt, cc := x.Feed(f)
		text.WriteString(tt)
		calls = append(calls, cc...)
	}
	ft, fc := x.Flush()
	text.WriteString(ft)
	calls = append(calls, fc...)
	return text.String(), calls
}

func TestXMLStreamExtractor(t *testing.T) {
	t.Run("complete block in one fragment", func(t *testing.T) {
		text, calls := feedAll(t, "Let me run the command:\n<tool_call>\n<function=bash>\n<parameter=command>rtk --version 2>&1</parameter>\n</function>\n</tool_call>")
		if text != "Let me run the command:\n" {
			t.Errorf("text = %q, want 'Let me run the command:\\n'", text)
		}
		if len(calls) != 1 {
			t.Fatalf("calls len = %d, want 1", len(calls))
		}
		if calls[0].Function.Name != "bash" {
			t.Errorf("name = %q, want 'bash'", calls[0].Function.Name)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
			t.Fatalf("arguments not JSON: %v", err)
		}
		if args["command"] != "rtk --version 2>&1" {
			t.Errorf("command arg = %v, want 'rtk --version 2>&1'", args["command"])
		}
	})

	t.Run("block split across fragments", func(t *testing.T) {
		text, calls := feedAll(t,
			"Let me check:\n<tool_call>",
			"\n<function=bash>",
			"\n<parameter=command>pwd</parameter>",
			"\n</function>\n</tool_call>",
			"\nDone.",
		)
		if text != "Let me check:\n\nDone." {
			t.Errorf("text = %q, want 'Let me check:\\n\\nDone.'", text)
		}
		if len(calls) != 1 {
			t.Fatalf("calls len = %d, want 1", len(calls))
		}
		if calls[0].Function.Name != "bash" {
			t.Errorf("name = %q, want 'bash'", calls[0].Function.Name)
		}
	})

	t.Run("text before block emitted immediately while block held", func(t *testing.T) {
		var x XMLToolCallExtractor
		tt, cc := x.Feed("intro <tool_call>")
		if tt != "intro " {
			t.Errorf("first Feed text = %q, want 'intro '", tt)
		}
		if len(cc) != 0 {
			t.Errorf("first Feed calls = %d, want 0", len(cc))
		}
		tt, cc = x.Feed("<function=bash><parameter=cmd>ls</parameter></function></tool_call>")
		if tt != "" {
			t.Errorf("second Feed text = %q, want ''", tt)
		}
		if len(cc) != 1 {
			t.Fatalf("second Feed calls = %d, want 1", len(cc))
		}
	})

	t.Run("two blocks in one fragment", func(t *testing.T) {
		text, calls := feedAll(t, "first:\n<tool_call>{\"name\":\"a\",\"arguments\":{}}</tool_call>\nsecond:\n<codebuff_tool_call>{\"name\":\"b\",\"arguments\":{}}</codebuff_tool_call>")
		if text != "first:\n\nsecond:\n" {
			t.Errorf("text = %q, want 'first:\\n\\nsecond:\\n'", text)
		}
		if len(calls) != 2 {
			t.Fatalf("calls len = %d, want 2", len(calls))
		}
		if calls[0].Function.Name != "a" || calls[1].Function.Name != "b" {
			t.Errorf("names = %q,%q want a,b", calls[0].Function.Name, calls[1].Function.Name)
		}
	})

	t.Run("pipe form", func(t *testing.T) {
		text, calls := feedAll(t, "I will help:\n<|tool_call_start|>\n<function=bash>\n<parameter=command>echo hi</parameter>\n</function>\n<|tool_call_end|>")
		if text != "I will help:\n" {
			t.Errorf("text = %q, want 'I will help:\\n'", text)
		}
		if len(calls) != 1 {
			t.Fatalf("calls len = %d, want 1", len(calls))
		}
		if calls[0].Function.Name != "bash" {
			t.Errorf("name = %q, want 'bash'", calls[0].Function.Name)
		}
	})

	t.Run("fenced json block", func(t *testing.T) {
		text, calls := feedAll(t, "I will list the directory:\n```tool_call\n{\"name\": \"bash\", \"arguments\": {\"command\": \"ls -la\"}}\n```")
		if text != "I will list the directory:\n" {
			t.Errorf("text = %q, want 'I will list the directory:\\n'", text)
		}
		if len(calls) != 1 {
			t.Fatalf("calls len = %d, want 1", len(calls))
		}
		if calls[0].Function.Name != "bash" {
			t.Errorf("name = %q, want 'bash'", calls[0].Function.Name)
		}
	})

	t.Run("plain code fence without brace passes through", func(t *testing.T) {
		text, calls := feedAll(t, "example:\n```go\nfunc main() {}\n```")
		if text != "example:\n```go\nfunc main() {}\n```" {
			t.Errorf("text = %q, want unchanged", text)
		}
		if len(calls) != 0 {
			t.Errorf("calls = %d, want 0", len(calls))
		}
	})

	t.Run("fenced opener split mid-delta does not panic", func(t *testing.T) {
		// The '{' lands in the same fragment as the fence but AFTER
		// non-fence prefix text; the old code stored a fragment-absolute
		// fenceBrace and sliced buffered (relative) with it.
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("extractor panicked: %v", r)
			}
		}()
		text, calls := feedAll(t,
			"note: ```json\n{",
			"\"name\": \"bash\", \"arguments\": {\"command\": \"ls -la\"}}\n```",
			"\nDone.",
		)
		if text != "note: \nDone." {
			t.Errorf("text = %q, want 'note: \\nDone.'", text)
		}
		if len(calls) != 1 {
			t.Fatalf("calls len = %d, want 1", len(calls))
		}
		if calls[0].Function.Name != "bash" {
			t.Errorf("name = %q, want 'bash'", calls[0].Function.Name)
		}
	})

	t.Run("fence and opener split across fragments", func(t *testing.T) {
		// The fragment ends on "```json\n" with no '{' visible; the '{'
		// arrives in the next fragment and must complete the opener.
		text, calls := feedAll(t,
			"Here:\n```json\n",
			"{\"name\": \"bash\", \"arguments\": {\"command\": \"pwd\"}}\n```",
		)
		if text != "Here:\n" {
			t.Errorf("text = %q, want 'Here:\\n'", text)
		}
		if len(calls) != 1 {
			t.Fatalf("calls len = %d, want 1", len(calls))
		}
		if calls[0].Function.Name != "bash" {
			t.Errorf("name = %q, want 'bash'", calls[0].Function.Name)
		}
	})

	t.Run("closer inside json string value does not close early", func(t *testing.T) {
		// "```" inside a JSON string value must not close the block; the
		// real closing fence after the JSON does.
		text, calls := feedAll(t,
			"```json\n{\"name\": \"bash\", \"arguments\": {\"command\": \"echo ``` hi\"}}\n```",
			"\nnote",
		)
		if text != "\nnote" {
			t.Errorf("text = %q, want '\\nnote'", text)
		}
		if len(calls) != 1 {
			t.Fatalf("calls len = %d, want 1", len(calls))
		}
		if calls[0].Function.Name != "bash" {
			t.Errorf("name = %q, want 'bash'", calls[0].Function.Name)
		}
	})

	t.Run("plain code fence split across fragments stays text", func(t *testing.T) {
		text, calls := feedAll(t, "```go\n", "func main() {\n}\n```")
		if text != "```go\nfunc main() {\n}\n```" {
			t.Errorf("text = %q, want unchanged", text)
		}
		if len(calls) != 0 {
			t.Errorf("calls = %d, want 0", len(calls))
		}
	})

	t.Run("plain code fence then brace stays text", func(t *testing.T) {
		text, calls := feedAll(t, "```go\n", "{\"name\": \"bash\", \"arguments\": {}}\n```")
		if text != "```go\n{\"name\": \"bash\", \"arguments\": {}}\n```" {
			t.Errorf("text = %q, want unchanged", text)
		}
		if len(calls) != 0 {
			t.Errorf("calls = %d, want 0", len(calls))
		}
	})

	t.Run("false positive block kept as text", func(t *testing.T) {
		text, calls := feedAll(t, "The tag <function_call>not a tool call</function_call> is literal.")
		if text != "The tag <function_call>not a tool call</function_call> is literal." {
			t.Errorf("text = %q, want unchanged", text)
		}
		if len(calls) != 0 {
			t.Errorf("calls = %d, want 0", len(calls))
		}
	})

	t.Run("unclosed block flushed scrubbed at stream end", func(t *testing.T) {
		text, calls := feedAll(t, "cut off mid:\n<tool_call>\n<function=bash>")
		if text != "cut off mid:\n\n" {
			t.Errorf("text = %q, want 'cut off mid:\\n\\n' with dangling tags scrubbed", text)
		}
		if strings.Contains(text, "<tool_call>") || strings.Contains(text, "<function") {
			t.Errorf("text = %q, dangling tags not scrubbed", text)
		}
		if len(calls) != 0 {
			t.Errorf("calls = %d, want 0", len(calls))
		}
	})

	t.Run("buffer bound flushes false positive", func(t *testing.T) {
		old := maxStreamXMLBuffer
		maxStreamXMLBuffer = 64
		t.Cleanup(func() { maxStreamXMLBuffer = old })
		var x XMLToolCallExtractor
		text, calls := x.Feed("prose <tool_call> " + strings.Repeat("x", 100))
		if len(calls) != 0 {
			t.Fatalf("calls = %d, want 0", len(calls))
		}
		if text != "prose <tool_call> "+strings.Repeat("x", 100) {
			t.Errorf("text = %q, want full prose flushed at bound", text)
		}
	})

	t.Run("flush of open block yields no calls and scrubbed text", func(t *testing.T) {
		var x XMLToolCallExtractor
		tt, cc := x.Feed("<tool_call>")
		if tt != "" || len(cc) != 0 {
			t.Fatalf("Feed = (%q, %d), want ('', 0)", tt, len(cc))
		}
		text, calls := x.Flush()
		if len(calls) != 0 {
			t.Fatalf("Flush calls = %d, want 0", len(calls))
		}
		if text != "" {
			t.Errorf("text = %q, want ''", text)
		}
	})
}

func TestToolCallDeltaFragment(t *testing.T) {
	tc := &toolCall{ID: "call_x", Type: "function", Function: toolFunction{Name: "bash", Arguments: `{"command":"pwd"}`}}
	got := ToolCallDeltaFragment(3, tc)
	want := map[string]any{
		"index": 3,
		"id":    "call_x",
		"type":  "function",
		"function": map[string]any{
			"name":      "bash",
			"arguments": `{"command":"pwd"}`,
		},
	}
	assertJSONEq(t, mustJSON(t, got), want)
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

// TestExtractXMLToolCallsPluralDialect pins the <tool_calls> (plural) XML
// dialect: some models emit the plural wrapper around the same
// <function=...>/<parameter=...> payload, and it must extract like the
// singular form instead of leaking literal "<tool_calls>" into content.
func TestExtractXMLToolCallsPluralDialect(t *testing.T) {
	raw := "Reading VansRouter executor first half\n<tool_calls>\n<function=read_file>\n<parameter=path>VansRouter executor first half</parameter>\n</function>\n</tool_calls>"
	cleaned, calls := extractXMLToolCalls(raw)
	if len(calls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "read_file" {
		t.Errorf("name = %q, want read_file", calls[0].Function.Name)
	}
	if strings.Contains(cleaned, "<tool_calls>") || strings.Contains(cleaned, "</tool_calls>") {
		t.Errorf("cleaned content still carries the plural tags: %q", cleaned)
	}
	if !strings.Contains(cleaned, "Reading VansRouter executor first half") {
		t.Errorf("cleaned content lost the prose: %q", cleaned)
	}

	// JSON payload inside the plural wrapper.
	rawJSON := "x <tool_calls>{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}</tool_calls> y"
	cleanedJSON, callsJSON := extractXMLToolCalls(rawJSON)
	if len(callsJSON) != 1 || callsJSON[0].Function.Name != "read_file" {
		t.Fatalf("plural JSON payload not extracted: %d calls, first %+v", len(callsJSON), callsJSON)
	}
	if strings.Contains(cleanedJSON, "<tool_calls>") {
		t.Errorf("cleaned JSON content still carries plural tags: %q", cleanedJSON)
	}
}

// TestXMLStreamExtractorPluralDialect streams a <tool_calls> block split
// across fragments: the opener lands mid-tag, the payload and closer in a
// later fragment. Regression: the plural opener was not a stream shape, so
// the block leaked as literal "<tool_calls>" text to the harness.
func TestXMLStreamExtractorPluralDialect(t *testing.T) {
	var x XMLToolCallExtractor
	text, _ := x.Feed("Let me check:\n<tool_cal")
	if text != "Let me check:\n" {
		t.Errorf("pre-opener text = %q, want %q", text, "Let me check:\n")
	}
	text, calls := x.Feed("ls>\n<function=read_file>\n<parameter=path>a.go</parameter>\n</function>\n</tool_calls>")
	if len(calls) != 1 || calls[0].Function.Name != "read_file" {
		t.Fatalf("want read_file call, got %d calls: %+v", len(calls), calls)
	}
	if text != "" {
		t.Errorf("block fragment leaked as text: %q", text)
	}
	text, calls = x.Feed("done")
	if text != "done" || len(calls) != 0 {
		t.Errorf("post-block text = %q calls = %d", text, len(calls))
	}
}

// TestXMLStreamExtractorPluralDanglingFlush pins Flush scrubbing of an
// unclosed <tool_calls> block: no call parses, and the dangling tags are
// removed instead of reaching the client as literal text.
func TestXMLStreamExtractorPluralDanglingFlush(t *testing.T) {
	var x XMLToolCallExtractor
	text, _ := x.Feed("intro <tool_calls>\n<function=read_file>")
	if text != "intro " {
		t.Errorf("pre-opener text = %q, want %q", text, "intro ")
	}
	text, calls := x.Flush()
	if len(calls) != 0 {
		t.Fatalf("unclosed block parsed %d calls, want 0", len(calls))
	}
	if strings.Contains(text, "<tool_calls>") || strings.Contains(text, "<function") {
		t.Errorf("dangling tags survived Flush: %q", text)
	}
}
