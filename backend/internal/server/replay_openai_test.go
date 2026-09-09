package server_test

// Hermetic "real user usage" replays of the OpenAI chat/completions surface
// (devdocs/reviews/user-flow-map.md scenarios replay-opencode-chat-stream,
// replay-opencode-chat-without-usage, replay-qwen-chat-sse,
// replay-qwen-chat-nonstream, replay-pi-chat-thinking,
// replay-aider-chat-toolcalls-ignored, replay-aider-chat-reasoning-effort,
// replay-goose-chat-include-usage). Each test drives the live HTTP surface
// with testutil.MockUpstream as the fake codebuff upstream and asserts the
// byte shape clients parse: delta sequence, terminal finish_reason, the
// final usage chunk, [DONE], and the request translation recorded upstream
// (stream_options passthrough, reasoning_effort clamping, tool-name mapping
// round-trip).

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// --- frame-level SSE helpers -------------------------------------------------

// collectOpenAIFrames parses an OpenAI chat SSE body into ordered data
// frames. Comment lines are skipped; a "data: [DONE]" line sets done.
func collectOpenAIFrames(t *testing.T, body string) (frames []map[string]any, done bool) {
	t.Helper()
	for _, rawLine := range strings.Split(body, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "[DONE]" {
			done = true
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("SSE frame is not a JSON object: %v: %q", err, line)
		}
		frames = append(frames, m)
	}
	return frames, done
}

// openAIChoice returns choice 0's object, or nil when the frame carries none.
func openAIChoice(f map[string]any) map[string]any {
	choices, _ := f["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	ch, _ := choices[0].(map[string]any)
	return ch
}

// openAIDelta returns choice 0's delta, or nil when absent.
func openAIDelta(f map[string]any) map[string]any {
	ch := openAIChoice(f)
	if ch == nil {
		return nil
	}
	d, _ := ch["delta"].(map[string]any)
	return d
}

// openAIFinish returns choice 0's finish_reason when it is a non-empty string.
func openAIFinish(f map[string]any) (string, bool) {
	ch := openAIChoice(f)
	if ch == nil {
		return "", false
	}
	fr, _ := ch["finish_reason"].(string)
	return fr, fr != ""
}

// openAIUsage returns the frame's usage object, or nil.
func openAIUsage(f map[string]any) map[string]any {
	u, _ := f["usage"].(map[string]any)
	return u
}

// findTerminalFinish returns the LAST non-empty finish_reason observed in the
// stream — the terminal one a client assembles its turn from.
func findTerminalFinish(frames []map[string]any) (string, bool) {
	var fr string
	found := false
	for _, f := range frames {
		if s, ok := openAIFinish(f); ok {
			fr, found = s, true
		}
	}
	return fr, found
}

// indexOfFrame returns the index of the first frame pred matches, or -1.
func indexOfFrame(frames []map[string]any, pred func(map[string]any) bool) int {
	for i, f := range frames {
		if pred(f) {
			return i
		}
	}
	return -1
}

// hasUsageFrame reports whether any frame carries a non-nil usage object.
func hasUsageFrame(frames []map[string]any) bool {
	return indexOfFrame(frames, func(f map[string]any) bool { return openAIUsage(f) != nil }) >= 0
}

// joinToolArgs concatenates, in stream order, the function.arguments
// fragments of every delta.tool_calls entry bearing the given index.
func joinToolArgs(frames []map[string]any, index int) string {
	var sb strings.Builder
	for _, f := range frames {
		d := openAIDelta(f)
		if d == nil {
			continue
		}
		tcs, _ := d["tool_calls"].([]any)
		for _, tcRaw := range tcs {
			tc, _ := tcRaw.(map[string]any)
			if tc == nil {
				continue
			}
			idx, _ := tc["index"].(float64)
			if int(idx) != index {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			if fn == nil {
				continue
			}
			if args, ok := fn["arguments"].(string); ok {
				sb.WriteString(args)
			}
		}
	}
	return sb.String()
}

// toolCallName returns the function.name of the first fragment bearing index.
func toolCallName(frames []map[string]any, index int) string {
	for _, f := range frames {
		d := openAIDelta(f)
		if d == nil {
			continue
		}
		tcs, _ := d["tool_calls"].([]any)
		for _, tcRaw := range tcs {
			tc, _ := tcRaw.(map[string]any)
			if tc == nil {
				continue
			}
			idx, _ := tc["index"].(float64)
			if int(idx) != index {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			if fn == nil {
				continue
			}
			if name, _ := fn["name"].(string); name != "" {
				return name
			}
		}
	}
	return ""
}

// assertUsageTotals asserts the prompt/completion/total triple on a usage object.
func assertUsageTotals(t *testing.T, usage map[string]any) {
	t.Helper()
	if usage == nil {
		t.Fatal("usage object missing")
		return
	}
	for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		v, ok := usage[k].(float64)
		if !ok || v <= 0 {
			t.Errorf("usage.%s = %v, want a positive number", k, usage[k])
		}
	}
}

// assertToolArgsComplete asserts the joined arguments parse as complete JSON —
// qwen-code's StreamingToolCallParser throws PROTOCOL_TAG_LEAK on truncated
// tool arguments delivered with finish_reason tool_calls.
func assertToolArgsComplete(t *testing.T, args string) {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		t.Errorf("joined tool arguments are not complete JSON (PROTOCOL_TAG_LEAK risk): %v: %q", err, args)
	}
}

// --- replay-opencode-chat-stream ---------------------------------------------

// TestReplayOpencodeChatStream replays opencode's openai-compatible chat:
// stream_options.include_usage is UNCONDITIONAL, reasoning_effort medium,
// native tools + tool_choice auto, and the stream must deliver the delta
// sequence, a terminal finish_reason, the final usage chunk (with
// completion_tokens_details.reasoning_tokens), and [DONE] — plus the #140
// tool-name round-trip (client "read" ⇄ upstream "read_files") and the
// end_turn pseudo-tool never leaking.
func TestReplayOpencodeChatStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		stream := testutil.SSEEvent(chunk("chatcmpl-oc1", 100,
			`"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Considering the file tree..."},"finish_reason":null}]`)) +
			testutil.SSEEvent(chunk("chatcmpl-oc1", 100,
				`"choices":[{"index":0,"delta":{"content":"Found it."},"finish_reason":null}]`)) +
			testutil.SSEEvent(chunk("chatcmpl-oc1", 100,
				`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_read_10","type":"function","function":{"name":"read_files","arguments":"{\"paths\":[\""}}]},"finish_reason":null}]`)) +
			testutil.SSEEvent(chunk("chatcmpl-oc1", 100,
				`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"src/main.go\"]}"}}]},"finish_reason":null}]`)) +
			testutil.SSEEvent(chunk("chatcmpl-oc1", 100,
				`"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_end_99","type":"function","function":{"name":"end_turn","arguments":"{}"}}]},"finish_reason":null}]`)) +
			testutil.SSEEvent(chunk("chatcmpl-oc1", 100,
				`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`)) +
			testutil.SSEEvent(chunk("chatcmpl-oc1", 100,
				`"usage":{"prompt_tokens":123,"completion_tokens":45,"total_tokens":168,"completion_tokens_details":{"reasoning_tokens":21}}`))
		_, _ = io.WriteString(w, stream)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{
		"model": "` + modelA + `",
		"messages": [
			{"role": "system", "content": "You are opencode."},
			{"role": "user", "content": "read src/main.go"}
		],
		"stream": true,
		"stream_options": {"include_usage": true},
		"reasoning_effort": "medium",
		"tools": [{"type": "function", "function": {"name": "read", "description": "Read a file", "parameters": {"type": "object", "properties": {"paths": {"type": "array", "items": {"type": "string"}}}, "required": ["paths"]}}}],
		"tool_choice": "auto"
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := string(data)
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream must end with [DONE]: %q", truncate(body, 300))
	}
	frames, done := collectOpenAIFrames(t, body)
	if !done {
		t.Error("stream missing the [DONE] terminator")
	}
	if len(frames) < 6 {
		t.Fatalf("frames = %d, want >= 6: %q", len(frames), body)
	}

	// First frame opens with the role and the first reasoning delta.
	firstDelta := openAIDelta(frames[0])
	if firstDelta == nil {
		t.Fatal("first frame has no delta")
		return
	}
	if r, _ := firstDelta["role"].(string); r != "assistant" {
		t.Errorf("first frame delta.role = %q, want assistant", r)
	}
	if rc, _ := firstDelta["reasoning_content"].(string); rc != "Considering the file tree..." {
		t.Errorf("first frame reasoning_content = %q", rc)
	}
	// Every frame identifies the served model.
	for i, f := range frames {
		if m, _ := f["model"].(string); m != modelA {
			t.Errorf("frame %d model = %q, want %q", i, m, modelA)
		}
	}

	// Delta sequence: reasoning → content → tool_calls → terminal finish → usage.
	reasoningIdx := indexOfFrame(frames, func(f map[string]any) bool {
		d := openAIDelta(f)
		return d != nil && d["reasoning_content"] != nil
	})
	contentIdx := indexOfFrame(frames, func(f map[string]any) bool {
		d := openAIDelta(f)
		if d == nil {
			return false
		}
		c, _ := d["content"].(string)
		return c != ""
	})
	toolIdx := indexOfFrame(frames, func(f map[string]any) bool {
		d := openAIDelta(f)
		if d == nil {
			return false
		}
		tcs, _ := d["tool_calls"].([]any)
		for _, tcRaw := range tcs {
			tc, _ := tcRaw.(map[string]any)
			if tc == nil {
				continue
			}
			if id, _ := tc["id"].(string); id == "call_read_10" {
				return true
			}
		}
		return false
	})
	usageIdx := indexOfFrame(frames, func(f map[string]any) bool { return openAIUsage(f) != nil })
	if reasoningIdx < 0 || contentIdx < 0 || toolIdx < 0 || usageIdx < 0 {
		t.Fatalf("delta sequence element missing: reasoning=%d content=%d toolCalls=%d usage=%d", reasoningIdx, contentIdx, toolIdx, usageIdx)
	}
	if reasoningIdx >= contentIdx || contentIdx >= toolIdx || toolIdx >= usageIdx {
		t.Errorf("delta sequence order got reasoning=%d content=%d toolCalls=%d usage=%d, want strictly ascending", reasoningIdx, contentIdx, toolIdx, usageIdx)
	}

	// Terminal finish_reason must be present (opencode hard-errors without it).
	fr, ok := findTerminalFinish(frames)
	if !ok {
		t.Errorf("stream delivered no terminal finish_reason (opencode native runtime errors without it)")
	} else if fr != "tool_calls" {
		t.Errorf("terminal finish_reason = %q, want tool_calls", fr)
	}

	// Final usage chunk: prompt/completion/total + reasoning accounting.
	usage := openAIUsage(frames[usageIdx])
	assertUsageTotals(t, usage)
	details, _ := usage["completion_tokens_details"].(map[string]any)
	if details == nil {
		t.Fatalf("usage missing completion_tokens_details: %v", usage)
		return
	}
	if rt, _ := details["reasoning_tokens"].(float64); rt != 21 {
		t.Errorf("usage.completion_tokens_details.reasoning_tokens = %v, want 21", details["reasoning_tokens"])
	}

	// Native tool call: index/id/name/arguments fragments, complete JSON
	// arguments, and the client name restored (upstream said read_files).
	args0 := joinToolArgs(frames, 0)
	assertToolArgsComplete(t, args0)
	if !strings.Contains(args0, `"src/main.go"`) {
		t.Errorf("joined tool arguments = %q, want the src/main.go path", args0)
	}
	if name := toolCallName(frames, 0); name != "read" {
		t.Errorf("client tool call name = %q, want \"read\" (restored from read_files)", name)
	}

	// The proxy-injected end_turn pseudo-tool must never leak to the client.
	if strings.Contains(body, "end_turn") || strings.Contains(body, "call_end_99") {
		t.Errorf("end_turn pseudo-tool leaked to the client: %q", truncate(body, 400))
	}

	// Request translation, recorded upstream: include_usage unconditional,
	// reasoning_effort clamped medium→high (mediumless DeepSeek ladder),
	// tools renamed to official signature names, tool_choice preserved.
	if n := len(mock.RecordedChatBodies); n != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (no retry storm)", n)
	}
	recorded := mock.RecordedChatBodies[0]
	for _, want := range []string{
		`"stream_options":{"include_usage":true}`,
		`"reasoning_effort":"high"`,
		`"read_files"`,
		`"tool_choice":"auto"`,
	} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s: %s", want, recorded)
		}
	}
}

// --- replay-opencode-chat-without-usage --------------------------------------

// TestReplayOpencodeChatWithoutUsage: when the upstream stream carries no
// usage chunk (even with include_usage requested), the stream must still
// terminate cleanly with a terminal finish_reason + [DONE] — opencode's
// token accounting degrades but the turn must never hang.
func TestReplayOpencodeChatWithoutUsage(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-oc2", 101,
			`"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Hmm..."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-oc2", 101,
			`"choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-oc2", 101,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{
		"model": "` + modelA + `",
		"messages": [{"role": "user", "content": "hi"}],
		"stream": true,
		"stream_options": {"include_usage": true},
		"reasoning_effort": "medium"
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	body := string(data)
	frames, done := collectOpenAIFrames(t, body)
	if !done {
		t.Error("stream must still terminate with [DONE] without a usage chunk")
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream must end with [DONE]: %q", truncate(body, 300))
	}
	fr, ok := findTerminalFinish(frames)
	if !ok {
		t.Errorf("no terminal finish_reason without a usage chunk (opencode hard-errors)")
	} else if fr != "stop" {
		t.Errorf("terminal finish_reason = %q, want stop", fr)
	}
	if hasUsageFrame(frames) {
		t.Error("client stream carries a usage chunk the upstream never sent")
	}
}

// --- replay-qwen-chat-sse -----------------------------------------------------

// TestReplayQwenChatSse replays qwen-code's OpenAI surface: stream_options
// include_usage (absent-tolerant), Content-Type text/event-stream (qwen's
// withResponse rejects non-SSE content types), native delta.tool_calls with
// COMPLETE arguments + finish_reason tool_calls (a truncated chain throws
// PROTOCOL_TAG_LEAK in qwen's StreamingToolCallParser), and the in-band
// error-frame convention (200 + data: {"error":...} + [DONE]) qwen tolerates.
func TestReplayQwenChatSse(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var chatCall atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if chatCall.Add(1) == 1 {
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-qw1", 200,
				`"choices":[{"index":0,"delta":{"reasoning_content":"Parsing request..."},"finish_reason":null}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-qw1", 200,
				`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_qw_1","type":"function","function":{"name":"run_shell","arguments":"{\"command\":\"ls -la\"}"}}]},"finish_reason":null}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-qw1", 200,
				`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-qw1", 200,
				`"usage":{"prompt_tokens":55,"completion_tokens":22,"total_tokens":77}`)))
		} else {
			// qwen tolerates the gateway encoding the failure in-band:
			// 200 + an OpenAI-shaped error frame + [DONE].
			_, _ = io.WriteString(w, `data: {"error":{"message":"upstream stream interrupted: boom","type":"upstream_error","code":"upstream_stream_error"}}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
	}

	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{
		"model": "` + modelA + `",
		"messages": [{"role": "user", "content": "list files"}],
		"stream": true,
		"stream_options": {"include_usage": true},
		"reasoning_effort": "high",
		"tools": [{"type": "function", "function": {"name": "run_shell", "description": "run a shell command", "parameters": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}}]
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream (qwen NonSSEResponseError)", ct)
	}
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	fr, ok := findTerminalFinish(frames)
	if !ok {
		t.Error("stream missing the terminal finish_reason (qwen requires finish_reason tool_calls + completed calls)")
	} else if fr != "tool_calls" {
		t.Errorf("terminal finish_reason = %q, want tool_calls", fr)
	}
	args := joinToolArgs(frames, 0)
	assertToolArgsComplete(t, args)
	if !strings.Contains(args, `"ls -la"`) {
		t.Errorf("joined tool arguments = %q, want the shell command", args)
	}
	if name := toolCallName(frames, 0); name != "run_shell" {
		t.Errorf("tool call name = %q, want run_shell", name)
	}
	usageIdx := indexOfFrame(frames, func(f map[string]any) bool { return openAIUsage(f) != nil })
	if usageIdx < 0 {
		t.Error("missing final usage chunk (merged by qwen's usageMetadata)")
	} else {
		assertUsageTotals(t, openAIUsage(frames[usageIdx]))
	}

	recorded := mock.RecordedChatBodies[0]
	for _, want := range []string{`"stream_options":{"include_usage":true}`, `"run_shell"`} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s: %s", want, recorded)
		}
	}

	// Error convention: in-band OpenAI-shaped error frame + [DONE] on 200.
	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("error-convention status = %d, want 200 (in-band): %s", resp2.StatusCode, truncate(string(data2), 300))
	}
	frames2, done2 := collectOpenAIFrames(t, string(data2))
	if !done2 {
		t.Error("error-convention stream missing [DONE] after the error frame")
	}
	errIdx := indexOfFrame(frames2, func(f map[string]any) bool {
		e, _ := f["error"].(map[string]any)
		return e != nil
	})
	if errIdx < 0 {
		t.Fatalf("error-convention stream missing the in-band error frame: %q", truncate(string(data2), 300))
	}
	errObj, _ := frames2[errIdx]["error"].(map[string]any)
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Errorf("error frame message empty: %v", errObj)
	}
}

// --- replay-qwen-chat-nonstream -----------------------------------------------

// TestReplayQwenChatNonstream: stream:false must produce one JSON
// chat.completion (message + usage), never SSE frames — qwen-code sets
// stream:false explicitly because some gateways default to SSE when absent.
func TestReplayQwenChatNonstream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-qw2", 201,
			`"choices":[{"index":0,"delta":{"reasoning_content":"Working..."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-qw2", 201,
			`"choices":[{"index":0,"delta":{"content":"The answer is 42."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-qw2", 201,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-qw2", 201,
			`"usage":{"prompt_tokens":40,"completion_tokens":10,"total_tokens":50}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{
		"model": "` + modelA + `",
		"messages": [{"role": "user", "content": "the meaning of life?"}],
		"stream": false
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	bb := string(data)
	if strings.Contains(bb, "data: ") {
		t.Fatalf("non-stream response carried SSE frames: %q", truncate(bb, 300))
	}
	var comp struct {
		Object  string `json:"object"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     float64 `json:"prompt_tokens"`
			CompletionTokens float64 `json:"completion_tokens"`
			TotalTokens      float64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &comp); err != nil {
		t.Fatalf("non-stream body is not JSON: %v: %s", err, truncate(bb, 300))
	}
	if comp.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", comp.Object)
	}
	if len(comp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(comp.Choices))
	}
	if comp.Choices[0].Message.Content != "The answer is 42." {
		t.Errorf("message.content = %q", comp.Choices[0].Message.Content)
	}
	if comp.Choices[0].Message.ReasoningContent != "Working..." {
		t.Errorf("message.reasoning_content = %q", comp.Choices[0].Message.ReasoningContent)
	}
	if comp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", comp.Choices[0].FinishReason)
	}
	if comp.Usage.PromptTokens != 40 || comp.Usage.CompletionTokens != 10 || comp.Usage.TotalTokens != 50 {
		t.Errorf("usage = %+v, want 40/10/50", comp.Usage)
	}
}

// --- replay-pi-chat-thinking --------------------------------------------------

// TestReplayPiChatThinking replays pi's openai-completions path with
// reasoning_effort/thinking: the reasoning channel arrives as
// delta.reasoning_content BEFORE the content deltas, the turn ends with
// finish_reason stop, and the final usage chunk carries the reasoning-token
// accounting (13 reasoning tokens in the live run).
func TestReplayPiChatThinking(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-pi1", 300,
			`"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Let me think."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-pi1", 300,
			`"choices":[{"index":0,"delta":{"reasoning_content":" Careful steps."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-pi1", 300,
			`"choices":[{"index":0,"delta":{"content":"The answer is 4."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-pi1", 300,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-pi1", 300,
			`"usage":{"prompt_tokens":99,"completion_tokens":30,"total_tokens":129,"completion_tokens_details":{"reasoning_tokens":13}}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{
		"model": "` + modelA + `",
		"messages": [{"role": "user", "content": "2+2?"}],
		"stream": true,
		"reasoning_effort": "high",
		"thinking": {"type": "enabled", "budget_tokens": 1024}
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	// Reasoning deltas must precede content deltas (thinking block closes
	// before the answer).
	reasoningIdx := indexOfFrame(frames, func(f map[string]any) bool {
		d := openAIDelta(f)
		return d != nil && d["reasoning_content"] != nil
	})
	contentIdx := indexOfFrame(frames, func(f map[string]any) bool {
		d := openAIDelta(f)
		if d == nil {
			return false
		}
		c, _ := d["content"].(string)
		return c != ""
	})
	if reasoningIdx < 0 || contentIdx < 0 {
		t.Fatalf("reasoning/content deltas missing: reasoning=%d content=%d", reasoningIdx, contentIdx)
	}
	if reasoningIdx > contentIdx {
		t.Errorf("content delta at %d arrived before the thinking block at %d", contentIdx, reasoningIdx)
	}
	fr, ok := findTerminalFinish(frames)
	if !ok {
		t.Error("no terminal finish_reason")
	} else if fr != "stop" {
		t.Errorf("terminal finish_reason = %q, want stop", fr)
	}
	usageIdx := indexOfFrame(frames, func(f map[string]any) bool { return openAIUsage(f) != nil })
	if usageIdx < 0 {
		t.Error("missing final usage chunk")
	} else {
		usage := openAIUsage(frames[usageIdx])
		assertUsageTotals(t, usage)
		details, _ := usage["completion_tokens_details"].(map[string]any)
		if details == nil {
			t.Errorf("usage missing completion_tokens_details: %v", usage)
		} else if rt, _ := details["reasoning_tokens"].(float64); rt != 13 {
			t.Errorf("reasoning_tokens = %v, want 13", details["reasoning_tokens"])
		}
	}
}

// --- replay-aider-chat-toolcalls-ignored --------------------------------------

// TestReplayAiderChatToolcallsIgnored: aider (litellm stream path) ignores
// native delta.tool_calls entirely. The gateway must still relay them
// cleanly and terminate — empty-content chunks tolerated, no retry storm,
// and errors surfaced as OpenAI-shaped bodies, never a bare 400 (aider
// fail-fasts on BadRequestError).
func TestReplayAiderChatToolcallsIgnored(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Empty-content chunk (empty delta) must be tolerated.
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-ad1", 400,
			`"choices":[{"index":0,"delta":{},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-ad1", 400,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ad_1","type":"function","function":{"name":"run_shell","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-ad1", 400,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{
		"model": "` + modelA + `",
		"messages": [{"role": "user", "content": "list files"}],
		"stream": true,
		"temperature": 0,
		"max_tokens": 8192
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	args := joinToolArgs(frames, 0)
	assertToolArgsComplete(t, args)
	fr, ok := findTerminalFinish(frames)
	if !ok {
		t.Error("no terminal finish_reason")
	} else if fr != "tool_calls" {
		t.Errorf("terminal finish_reason = %q, want tool_calls", fr)
	}
	if n := len(mock.RecordedChatBodies); n != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (no retry storm on a tool-call stream)", n)
	}
	recorded := mock.RecordedChatBodies[0]
	if strings.Contains(recorded, "stream_options") {
		t.Errorf("aider never sends stream_options; proxy must not invent one: %s", recorded)
	}

	// Error convention: upstream 500 → OpenAI-shaped error body, status 502
	// (retryable class), never a bare 400.
	errMock := testutil.NewMock()
	defer errMock.Close()
	errMock.ChatStatus = http.StatusInternalServerError
	errMock.ChatErrorBody = `{"error":{"message":"internal error","type":"server_error"}}`
	errTS, _ := newTestServer(t, nil, errMock)

	respE, dataE := doJSON(t, http.MethodPost, errTS.URL+"/v1/chat/completions", []byte(reqBody), nil)
	if respE.StatusCode == http.StatusBadRequest {
		t.Fatalf("upstream error surfaced as 400 (aider fail-fasts): %s", truncate(string(dataE), 300))
	}
	if respE.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", respE.StatusCode)
	}
	var errBody struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(dataE, &errBody); err != nil {
		t.Fatalf("error body is not the OpenAI shape: %v: %s", err, truncate(string(dataE), 300))
	}
	if errBody.Error.Message == "" || errBody.Error.Code == "" {
		t.Errorf("error body incomplete: %s", truncate(string(dataE), 300))
	}
	// chatAttempt retries once on the generic upstream error; a storm (the
	// pre-fix shape) would show 4+ chat calls, so bound the assertion.
	if n := len(errMock.RecordedChatBodies); n > 2 {
		t.Errorf("upstream chat calls = %d, want <= 2 (single retry-once, no storm)", n)
	}
}

// --- replay-aider-chat-reasoning-effort ---------------------------------------

// TestReplayAiderChatReasoningEffort: aider sends --reasoning-effort and
// --thinking-tokens as TOP-LEVEL body keys. The proxy must accept both
// (never 400), map the effort onto the wire, and deliver
// delta.reasoning_content to litellm's stream loop.
func TestReplayAiderChatReasoningEffort(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-ad2", 401,
			`"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Reasoning through the edit..."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-ad2", 401,
			`"choices":[{"index":0,"delta":{"content":"Fixed."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-ad2", 401,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-ad2", 401,
			`"usage":{"prompt_tokens":88,"completion_tokens":35,"total_tokens":123,"completion_tokens_details":{"reasoning_tokens":17}}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{
		"model": "` + modelA + `",
		"messages": [{"role": "user", "content": "fix the bug"}],
		"stream": true,
		"reasoning_effort": "high",
		"thinking": {"type": "enabled", "budget_tokens": 2048}
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("top-level reasoning_effort/thinking rejected with %d: %s (must be accepted)", resp.StatusCode, truncate(string(data), 300))
	}
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	reasoningIdx := indexOfFrame(frames, func(f map[string]any) bool {
		d := openAIDelta(f)
		return d != nil && d["reasoning_content"] != nil
	})
	if reasoningIdx < 0 {
		t.Error("delta.reasoning_content missing (litellm falls back to delta.reasoning)")
	} else if rc, _ := openAIDelta(frames[reasoningIdx])["reasoning_content"].(string); rc != "Reasoning through the edit..." {
		t.Errorf("reasoning_content = %q", rc)
	}
	fr, ok := findTerminalFinish(frames)
	if !ok {
		t.Error("no terminal finish_reason")
	} else if fr != "stop" {
		t.Errorf("terminal finish_reason = %q, want stop", fr)
	}
	usageIdx := indexOfFrame(frames, func(f map[string]any) bool { return openAIUsage(f) != nil })
	if usageIdx < 0 {
		t.Error("missing final usage chunk")
	} else {
		assertUsageTotals(t, openAIUsage(frames[usageIdx]))
	}
	recorded := mock.RecordedChatBodies[0]
	if !strings.Contains(recorded, `"reasoning_effort":"high"`) {
		t.Errorf("upstream body missing reasoning_effort: %s", recorded)
	}
	// #111: the DeepSeek thinking translation is server-side — the proxy
	// never emits a thinking block on the wire.
	if strings.Contains(recorded, `"thinking":`) {
		t.Errorf("upstream body carries a thinking block (proxy must map to reasoning_effort): %s", recorded)
	}
}

// --- replay-goose-chat-include-usage -------------------------------------------

// TestReplayGooseChatIncludeUsage replays goose's OpenAI engine: stream
// + stream_options include_usage (always), a developer-role system message
// (rewritten to system upstream), native tool_calls whose tool-call loop
// ends on finish_reason tool_calls, and the final usage chunk + [DONE]
// (goose's SSE parser breaks on [DONE]; without it the tool-call inner loop
// only ends on finish_reason).
func TestReplayGooseChatIncludeUsage(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-gs1", 500,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_gs_1","type":"function","function":{"name":"run_shell","arguments":"{\"command\":\""}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-gs1", 500,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"goose run\"}"}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-gs1", 500,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-gs1", 500,
			`"usage":{"prompt_tokens":120,"completion_tokens":38,"total_tokens":158}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{
		"model": "` + modelA + `",
		"messages": [
			{"role": "developer", "content": "You are a helpful goose."},
			{"role": "user", "content": "run goose"}
		],
		"stream": true,
		"stream_options": {"include_usage": true},
		"tools": [{"type": "function", "function": {"name": "run_shell", "description": "run a command", "parameters": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}}]
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE] (goose breaks on it)")
	}
	fr, ok := findTerminalFinish(frames)
	if !ok {
		t.Error("no terminal finish_reason (goose tool-call loop needs it)")
	} else if fr != "tool_calls" {
		t.Errorf("terminal finish_reason = %q, want tool_calls", fr)
	}
	args := joinToolArgs(frames, 0)
	assertToolArgsComplete(t, args)
	if !strings.Contains(args, `"goose run"`) {
		t.Errorf("joined tool arguments = %q, want the goose command", args)
	}
	if name := toolCallName(frames, 0); name != "run_shell" {
		t.Errorf("tool call name = %q, want run_shell", name)
	}
	usageIdx := indexOfFrame(frames, func(f map[string]any) bool { return openAIUsage(f) != nil })
	if usageIdx < 0 {
		t.Error("missing final usage chunk (goose reads it from any chunk)")
	} else {
		assertUsageTotals(t, openAIUsage(frames[usageIdx]))
	}
	if n := len(mock.RecordedChatBodies); n != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (proxy must not loop the tool call)", n)
	}
	recorded := mock.RecordedChatBodies[0]
	for _, want := range []string{`"stream_options":{"include_usage":true}`, `"role":"system"`} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s: %s", want, recorded)
		}
	}
	if strings.Contains(recorded, `"role":"developer"`) {
		t.Errorf("developer role leaked to the upstream (must rewrite to system): %s", recorded)
	}
}
