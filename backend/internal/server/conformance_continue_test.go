package server_test

// Hermetic "real user usage" conformance tests for the Continue harness
// (reference/agents/continue/WIRE-NOTES.md) against the proxy's OpenAI
// chat and Anthropic /v1/messages surfaces. Continue's OpenAI adapter sends
// BOTH x-api-key AND Authorization: Bearer with the SAME key
// (openai-adapters/src/apis/OpenAI.ts:159-169), forces
// stream_options.include_usage on streaming (OpenAI.ts:52-54), and its
// Anthropic path always sends anthropic-version plus
// anthropic-beta "prompt-caching-2024-07-31" (cache enabled) with
// system[].cache_control:{type:"ephemeral"} (AnthropicUtils.ts:66-88).
// Each test drives the live HTTP surface with testutil.MockUpstream as the
// fake codebuff.com upstream and asserts the byte shape clients parse.

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// continueToolUseBlock returns the first tool_use content_block's index, name,
// and id from an Anthropic event list, or (-1, "", "") when the stream never
// produced one. Continue's Anthropic stream surfaces each tool_use via
// content_block_start (id/name) and input_json_delta fragments
// (WIRE-NOTES.md §4), so the assembled turn is only usable when these are
// intact.
func continueToolUseBlock(events []map[string]any) (int, string, string) {
	for _, ev := range events {
		if ev["type"] != "content_block_start" {
			continue
		}
		cb, _ := ev["content_block"].(map[string]any)
		if cb == nil || cb["type"] != "tool_use" {
			continue
		}
		idx := -1
		if f, ok := ev["index"].(float64); ok {
			idx = int(f)
		}
		name, _ := cb["name"].(string)
		id, _ := cb["id"].(string)
		return idx, name, id
	}
	return -1, "", ""
}

// TestConformanceContinueDualAuthIncludeUsage replays Continue's OpenAI-
// compatible chat wire: BOTH x-api-key and Authorization: Bearer (same key)
// with stream_options.include_usage:true and a Continue-shaped native tool
// {type:function, function:{name, description, parameters, strict}}. The
// proxy must accept the dual auth (never 401), keep stream_options.
// include_usage in the forwarded upstream body (Continue's OpenAI adapter
// forces it and a proxy dropping it loses token accounting downstream), pass
// the tool shape through untouched, and relay a stream with terminal
// finish_reason, the final usage chunk, and [DONE].
func TestConformanceContinueDualAuthIncludeUsage(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cont1", 1,
			`"choices":[{"index":0,"delta":{"role":"assistant","content":"Hello!"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cont1", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":25,"completion_tokens":11,"total_tokens":36}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, []string{"continue-key"}, mock)
	headers := map[string]string{
		"Content-Type":  "application/json",
		"x-api-key":     "continue-key",
		"Authorization": "Bearer continue-key",
	}
	// execute_shell is NOT in the proxy's clientToOfficial tool map
	// (internal/convert/toolmap.go), so it round-trips verbatim — keeping the
	// recorded-body assertion about the tool shape unambiguous.
	reqBody := `{
		"model": "` + modelA + `",
		"messages": [{"role": "user", "content": "Hi"}],
		"stream": true,
		"stream_options": {"include_usage": true},
		"tools": [{"type": "function", "function": {"name": "execute_shell", "description": "Run a shell command", "parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}, "required": ["cmd"]}, "strict": false}}]
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (dual x-api-key + Bearer must be accepted): %s", resp.StatusCode, truncate(string(data), 300))
	}
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE] (Continue's OpenAI SDK iterates to it)")
	}
	if fr, ok := findTerminalFinish(frames); !ok {
		t.Error("no terminal finish_reason (Continue needs it to close the turn)")
	} else if fr != "stop" {
		t.Errorf("terminal finish_reason = %q, want stop", fr)
	}
	usageIdx := indexOfFrame(frames, func(f map[string]any) bool { return openAIUsage(f) != nil })
	if usageIdx < 0 {
		t.Error("missing final usage chunk (Continue forces stream_options.include_usage)")
	} else {
		assertUsageTotals(t, openAIUsage(frames[usageIdx]))
	}

	// Upstream record: stream_options survives the whitelist
	// (internal/convert/convert.go upstreamKeys), and the Continue tool shape
	// round-trips. json.Marshal sorts object keys, so assert the parameters
	// sub-keys individually rather than a whole-object substring.
	recorded := mock.RecordedChatBodies[0]
	for _, want := range []string{
		`"stream_options":{"include_usage":true}`,
		`"type":"function"`,
		`"name":"execute_shell"`,
		`"strict":false`,
		`"type":"object"`,
	} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s: %s", want, recorded)
		}
	}
}

// TestConformanceContinueAnthropicCacheControlBeta replays Continue's
// Anthropic-surface wire: x-api-key + anthropic-version + anthropic-beta
// "prompt-caching-2024-07-31" (emitted when caching is on), system as an
// array of text parts carrying cache_control:{type:"ephemeral"}, max_tokens,
// tools, and unconditional stream. Asserts the beta header and cache_control
// shape are tolerated (never 400), max_tokens reaches the upstream, the
// system prompt is forwarded, cache_control is NOT leaked into the translated
// OpenAI upstream body, and the stream yields the standard Anthropic block
// lifecycle ending in message_stop.
func TestConformanceContinueAnthropicCacheControlBeta(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-cont2", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_exec_1","type":"function","function":{"name":"execute_shell","arguments":"{\"cmd\":\"ls"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-cont2", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" -la\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-cont2", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":60,"completion_tokens":44,"total_tokens":104}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, []string{"continue-key"}, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "continue-key",
		"anthropic-version": "2023-06-01",
		"anthropic-beta":    "prompt-caching-2024-07-31",
	}
	continueBody := `{
		"model": "deepseek/deepseek-v4-flash",
		"max_tokens": 4096,
		"system": [{"type": "text", "text": "You are Continue, a coding assistant.", "cache_control": {"type": "ephemeral"}}],
		"messages": [{"role": "user", "content": "List the contents of the current directory"}],
		"tools": [{"name": "execute_shell", "description": "Run a shell command", "input_schema": {"type": "object", "properties": {"cmd": {"type": "string"}}, "required": ["cmd"]}}],
		"stream": true
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(continueBody), headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (anthropic-beta + cache_control must be tolerated): %s", resp.StatusCode, truncate(string(data), 300))
	}
	events := collectAnthropicEvents(t, string(data))
	wantTypes := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := replayEventTypes(events); strings.Join(got, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event sequence = %v, want %v", got, wantTypes)
	}
	idx, name, id := continueToolUseBlock(events)
	if name != "execute_shell" || id != "call_exec_1" {
		t.Errorf("tool_use name/id = %q/%q, want execute_shell/call_exec_1", name, id)
	}
	if args := replayInputFragments(events, idx); args != `{"cmd":"ls -la"}` {
		t.Errorf("assembled tool_use input = %q, want {\"cmd\":\"ls -la\"}", args)
	}

	// Wire facts (WIRE-NOTES.md §3/§4): the client's max_tokens must reach the
	// upstream and the system text must be forwarded, while the Anthropic-only
	// cache_control marker must NOT leak into the translated OpenAI body.
	if !mock.BodyContains(`"max_tokens":4096`) {
		t.Error("upstream chat body missing max_tokens:4096")
	}
	if !mock.BodyContains("You are Continue") {
		t.Error("upstream chat body missing the system prompt text")
	}
	if mock.BodyContains("cache_control") {
		t.Error("cache_control leaked into the upstream chat body (Anthropic-only marker)")
	}
}

// TestConformanceContinueAnthropicToolUseStopReason pins the terminal
// stop-reason contract Continue's Anthropic streaming loop depends on
// (WIRE-NOTES.md §4): an upstream OpenAI turn that ends with
// finish_reason "tool_calls" must translate to an Anthropic tool_use content
// block whose assembled input is complete JSON, a message_delta carrying
// stop_reason "tool_use", and the stream must end with message_stop — never
// a bare hang or a truncated terminal.
func TestConformanceContinueAnthropicToolUseStopReason(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-cont3", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_tool_7","type":"function","function":{"name":"execute_shell","arguments":"{\"cmd\":\"ls"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-cont3", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" -la\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-cont3", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":60,"completion_tokens":44,"total_tokens":104}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, []string{"continue-key"}, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "continue-key",
		"anthropic-version": "2023-06-01",
	}
	body := `{"model":"deepseek/deepseek-v4-flash","max_tokens":4096,"messages":[{"role":"user","content":"List the files"}],"tools":[{"name":"execute_shell","description":"Run a shell command","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	events := collectAnthropicEvents(t, string(data))
	idx, name, id := continueToolUseBlock(events)
	if name != "execute_shell" || id != "call_tool_7" {
		t.Errorf("tool_use name/id = %q/%q, want execute_shell/call_tool_7", name, id)
	}
	if args := replayInputFragments(events, idx); args != `{"cmd":"ls -la"}` {
		t.Errorf("assembled tool_use input = %q, want {\"cmd\":\"ls -la\"}", args)
	}
	stop, _ := replayMessageDelta(events)
	if stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use (Continue drives the tool loop on it)", stop)
	}
	if last := events[len(events)-1]; last["type"] != "message_stop" {
		t.Errorf("last event type = %v, want message_stop", last["type"])
	}
}
