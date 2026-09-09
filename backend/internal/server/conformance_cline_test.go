package server_test

// Conformance replays for Cline (cline/cline) — a Vercel AI SDK v7 client,
// so the proxy must speak the real vendor wire verbatim. There are three
// distinct surfaces (reference/harnesses/cline/WIRE-NOTES.md §1, §10):
//
//   - openai-compatible: POST {baseUrl}/v1/chat/completions with
//     `includeUsage: true` (vendors/openai-compatible.ts:243-249) and, for
//     reasoning-era model IDs, `max_completion_tokens` — never `max_tokens`
//     (§10 withMaxCompletionTokensForReasoningModels). The proxy MUST accept
//     `max_completion_tokens` or cline's chat requests 400.
//   - openai (native provider.responses()): POST {baseUrl}/v1/responses with
//     `max_output_tokens` (§10, vendors/openai.ts:53-60), flat function
//     tools, and a stream that terminates on response.completed.
//   - anthropic (@ai-sdk/anthropic): POST {baseUrl}/v1/messages with
//     x-api-key + anthropic-version, extended thinking
//     `thinking:{enabled,max_tokens}` (§6) and the thinking signature
//     replayed UNMODIFIED on the tool-result turn (§6 / routing:
//     thinking history passed forward with signature).

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// clineResponsesBody builds a cline-shaped /v1/responses body: input_text
// user turn, flat function tool, max_output_tokens (never max_tokens), and
// stream:true — the @ai-sdk/openai Responses wire.
func clineResponsesBody(model string) string {
	input := `[{"role":"user","content":[{"type":"input_text","text":"What is 2+2?"}]}]`
	return `{"model":"` + model + `",` +
		`"input":` + input + `,` +
		`"max_output_tokens":1024,` +
		`"tools":[{"type":"function","name":"calculate","description":"Do math","parameters":{"type":"object","properties":{"expr":{"type":"string"}},"required":["expr"]}}],` +
		`"stream":true}`
}

// clineThinkingJoin joins every thinking_delta fragment in stream order —
// the client-side rule for assembling an assistant thinking block, which
// cline then replays back (signature included) on the tool-result turn.
func clineThinkingJoin(events []map[string]any) string {
	var sb strings.Builder
	for _, ev := range events {
		if ev["type"] != "content_block_delta" {
			continue
		}
		delta, _ := ev["delta"].(map[string]any)
		if delta == nil || delta["type"] != "thinking_delta" {
			continue
		}
		if text, ok := delta["thinking"].(string); ok {
			sb.WriteString(text)
		}
	}
	return sb.String()
}

// --- cline openai-compatible: max_completion_tokens + include_usage ----------

// TestConformanceClineChatCompatMaxCompletionTokens replays cline's
// openai-compatible chat: `max_completion_tokens` (never `max_tokens` for a
// reasoning-era model) plus `stream_options.include_usage` (the provider is
// built with `includeUsage: true`). Assert the convert whitelist passes the
// token cap through UNCHANGED end-to-end and include_usage survives — a 400
// or a dropped cap would break cline's reasoning-model chat requests.
func TestConformanceClineChatCompatMaxCompletionTokens(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-clineA1", 600,
			`"choices":[{"index":0,"delta":{"content":"Hello from cline."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-clineA1", 600,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-clineA1", 600,
			`"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServerCfg(t, []string{"cline-key"}, nil, mock)
	reqBody := `{
		"model": "` + modelA + `",
		"messages": [
			{"role": "system", "content": "You are Cline."},
			{"role": "user", "content": "hi"}
		],
		"max_completion_tokens": 2048,
		"stream": true,
		"stream_options": {"include_usage": true}
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody),
		map[string]string{"Authorization": "Bearer cline-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (max_completion_tokens must be accepted): %s",
			resp.StatusCode, truncate(string(data), 300))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Request translation, recorded upstream: the convert whitelist pin —
	// max_completion_tokens passed through UNCHANGED (cline never rewrites it
	// to max_tokens), and stream_options.include_usage preserved.
	recorded := mock.LastChatBody()
	if !strings.Contains(recorded, `"max_completion_tokens":2048`) {
		t.Errorf("upstream body missing max_completion_tokens 2048 (convert dropped/rewrote it): %s", truncate(recorded, 400))
	}
	if strings.Contains(recorded, `"max_tokens":`) {
		t.Errorf("upstream body rewrote max_completion_tokens to max_tokens: %s", truncate(recorded, 400))
	}
	if !strings.Contains(recorded, `"stream_options":{"include_usage":true}`) {
		t.Errorf("upstream body missing stream_options.include_usage: %s", truncate(recorded, 400))
	}
}

// --- cline openai (responses): max_output_tokens + tools ---------------------

// TestConformanceClineResponsesMaxOutputTokens replays cline's native OpenAI
// provider (provider.responses()) against /v1/responses: `max_output_tokens`
// (never `max_tokens`), flat function tools, and a stream that must terminate
// on response.completed with the usage triple — no 400 on the unknown-to-chat
// token cap, no [DONE] sentinel (Responses clients terminate on
// response.completed).
func TestConformanceClineResponsesMaxOutputTokens(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-clineR1", 700,
			`"choices":[{"index":0,"delta":{"content":"4"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-clineR1", 700,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-clineR1", 700,
			`"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}`)))
		// No upstream [DONE]: Responses clients terminate on response.completed.
	}

	ts, _ := newTestServer(t, nil, mock)
	body := clineResponsesBody(modelA)
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (max_output_tokens must be accepted): %s",
			resp.StatusCode, truncate(string(data), 300))
	}
	sse := string(data)
	if !strings.Contains(sse, `"type":"response.completed"`) {
		t.Fatalf("stream missing terminal response.completed: %q", truncate(sse, 400))
	}
	events := collectResponsesEvents(t, sse)
	if len(events) == 0 {
		t.Fatalf("no SSE events parsed from body: %q", truncate(sse, 400))
	}
	if last := eventTypes(events)[len(events)-1]; last != "response.completed" {
		t.Errorf("last event = %q, want response.completed", last)
	}
	if indexOfType(events, "response.failed") != -1 {
		t.Error("stream emitted response.failed for a healthy upstream")
	}

	// response.completed must carry the usage triple in Responses shape.
	var completedResp map[string]any
	for _, ev := range events {
		if t, _ := ev["type"].(string); t == "response.completed" {
			completedResp, _ = ev["response"].(map[string]any)
		}
	}
	if completedResp == nil {
		t.Fatal("response.completed missing response object")
		return
	}
	usage, _ := completedResp["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("response.completed missing usage")
		return
	}
	for _, k := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if v, ok := usage[k].(float64); !ok || v <= 0 {
			t.Errorf("usage.%s = %v, want a positive number", k, usage[k])
		}
	}
	if usage["input_tokens"] != float64(20) || usage["output_tokens"] != float64(5) || usage["total_tokens"] != float64(25) {
		t.Errorf("usage = %v, want input 20 output 5 total 25", usage)
	}

	// Request translation: max_output_tokens → max_completion_tokens upstream.
	if !mock.BodyContains(`"max_completion_tokens":1024`) {
		t.Error("upstream chat body missing max_completion_tokens mapped from max_output_tokens")
	}
}

// --- cline anthropic: thinking + signature replay ---------------------------

// TestConformanceClineAnthropicThinkingSignatureReplay replays cline's
// @ai-sdk/anthropic surface against /v1/messages: x-api-key +
// anthropic-version, extended thinking `thinking:{enabled,max_tokens}`,
// a tool_use turn, then the NEXT request re-sends the assistant thinking
// block + its signature UNMODIFIED along with the tool_result — the proxy
// must accept that replay (returned-200) rather than rejecting the
// replayed thinking history, and the tool_result must reach the upstream as
// a role:tool message with the echoed tool_use id.
func TestConformanceClineAnthropicThinkingSignatureReplay(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var calls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if calls.Add(1) == 1 {
			// Turn 1: thinking + a Bash tool use (no end_turn pseudo-tool on
			// cline's raw wire).
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-clineC1", 1,
				`"choices":[{"index":0,"delta":{"reasoning_content":"I need to inspect the directory first.","role":"assistant"},"finish_reason":null}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-clineC1", 1,
				`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_bash_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]},"index":0}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-clineC1", 1,
				`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls -la\"}"}}]},"index":0}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-clineC1", 1,
				`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":120,"completion_tokens":32,"total_tokens":152,"completion_tokens_details":{"reasoning_tokens":18}}`)))
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		// Turn 2: final answer after the tool_result.
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-clineC2", 1,
			`"choices":[{"index":0,"delta":{"content":"Done. file1.txt\ndir1\n","role":"assistant"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-clineC2", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":90,"completion_tokens":24,"total_tokens":114}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServerCfg(t, []string{"cline-key"}, nil, mock)
	baseHeaders := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "cline-key",
		"anthropic-version": "2023-06-01",
	}

	// --- Turn 1: tools + extended thinking, expect a streamed tool_use ---
	turn1Body := `{
		"model": "` + modelA + `",
		"max_tokens": 4096,
		"system": "You are Cline, a coding agent.",
		"messages": [{"role": "user", "content": "List the files in the current directory"}],
		"tools": [{"name": "Bash", "description": "Execute a shell command and return its output", "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}],
		"thinking": {"enabled": true, "max_tokens": 2048},
		"stream": true
	}`
	resp1, data1 := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(turn1Body), baseHeaders)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("turn 1 status = %d, want 200: %s", resp1.StatusCode, truncate(string(data1), 300))
	}
	if ct := resp1.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("turn 1 Content-Type = %q, want text/event-stream", ct)
	}
	if ver := resp1.Header.Get("anthropic-version"); ver != "2023-06-01" {
		t.Errorf("turn 1 anthropic-version = %q, want 2023-06-01", ver)
	}
	if strings.Contains(string(data1), "[DONE]") {
		t.Error("turn 1 leaked an OpenAI [DONE] terminator into the Anthropic stream")
	}

	events1 := collectAnthropicEvents(t, string(data1))
	wantTypes1 := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop"}
	if got := replayEventTypes(events1); strings.Join(got, ",") != strings.Join(wantTypes1, ",") {
		t.Fatalf("turn 1 event sequence = %v, want %v", got, wantTypes1)
	}

	var thinkingIdx, toolIdx = -1, -1
	var toolName, toolID string
	var sawThinkingDelta, sawSignatureDelta bool
	for _, ev := range events1 {
		switch ev["type"] {
		case "content_block_start":
			cb, _ := ev["content_block"].(map[string]any)
			if cb == nil {
				continue
			}
			switch cb["type"] {
			case "thinking":
				thinkingIdx = replayEventIndex(ev)
			case "tool_use":
				toolIdx = replayEventIndex(ev)
				toolName, _ = cb["name"].(string)
				toolID, _ = cb["id"].(string)
			}
		case "content_block_delta":
			delta, _ := ev["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			switch delta["type"] {
			case "thinking_delta":
				sawThinkingDelta = true
				if replayEventIndex(ev) != thinkingIdx {
					t.Errorf("thinking_delta index = %d, want %d (open thinking block)", replayEventIndex(ev), thinkingIdx)
				}
			case "signature_delta":
				sawSignatureDelta = true
				if replayEventIndex(ev) != thinkingIdx {
					t.Errorf("signature_delta index = %d, want %d (thinking block)", replayEventIndex(ev), thinkingIdx)
				}
			case "input_json_delta":
				if replayEventIndex(ev) != toolIdx {
					t.Errorf("input_json_delta index = %d, want %d (open tool_use block)", replayEventIndex(ev), toolIdx)
				}
			}
		}
	}
	if !sawThinkingDelta {
		t.Error("turn 1 missing a thinking_delta (extended thinking not relayed)")
	}
	if !sawSignatureDelta {
		t.Error("turn 1 missing a signature_delta (thinking block never closed with a signature)")
	}
	if toolName != "Bash" || toolID != "call_bash_1" {
		t.Errorf("tool_use name/id = %q/%q, want Bash/call_bash_1", toolName, toolID)
	}
	if args1 := replayInputFragments(events1, toolIdx); args1 != `{"command":"ls -la"}` {
		t.Errorf("assembled tool_use input = %q, want {\"command\":\"ls -la\"}", args1)
	}
	stop1, usage1 := replayMessageDelta(events1)
	if stop1 != "tool_use" {
		t.Errorf("turn 1 stop_reason = %q, want tool_use", stop1)
	}
	if out, _ := usage1["output_tokens"].(float64); out != 32 {
		t.Errorf("turn 1 usage.output_tokens = %v, want 32", usage1["output_tokens"])
	}

	// The thinking text the client received, to replay UNMODIFIED next turn.
	thinkingText := clineThinkingJoin(events1)
	if thinkingText == "" {
		t.Fatal("turn 1 delivered no thinking text to replay")
	}

	// --- Turn 2: re-send the assistant thinking + signature UNMODIFIED with
	// the tool_result — the proxy must accept the replay, not reject it. ---
	thinkingJSON, _ := json.Marshal(thinkingText)
	turn2Body := `{
		"model": "` + modelA + `",
		"max_tokens": 4096,
		"system": "You are Cline, a coding agent.",
		"messages": [
			{"role": "user", "content": "List the files in the current directory"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": ` + string(thinkingJSON) + `, "signature": ""},
				{"type": "tool_use", "id": "call_bash_1", "name": "Bash", "input": {"command": "ls -la"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "call_bash_1", "content": "file1.txt\ndir1\n"}
			]}
		],
		"tools": [{"name": "Bash", "description": "Execute a shell command and return its output", "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}],
		"stream": true
	}`
	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(turn2Body), baseHeaders)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("turn 2 status = %d, want 200 (replayed thinking + signature must be tolerated): %s",
			resp2.StatusCode, truncate(string(data2), 300))
	}
	events2 := collectAnthropicEvents(t, string(data2))
	if stop2, _ := replayMessageDelta(events2); stop2 != "end_turn" {
		t.Errorf("turn 2 stop_reason = %q, want end_turn", stop2)
	}
	if text := replayTextJoin(events2); text != "Done. file1.txt\ndir1\n" {
		t.Errorf("turn 2 assembled text = %q, want the final answer", text)
	}

	// The replayed thinking text must reach the upstream as reasoning_content.
	if !mock.BodyContains("I need to inspect the directory first.") {
		t.Error("upstream chat body missing the replayed thinking text")
	}
	// The tool_result must reach the upstream as a role:tool message with the
	// echoed tool_use id.
	if !mock.BodyContains(`"tool_call_id":"call_bash_1"`) {
		t.Error("upstream chat body missing tool_call_id echoed from the tool_use id")
	}
	if !mock.BodyContains(`"role":"tool"`) || !mock.BodyContains("file1.txt") {
		t.Error("upstream chat body missing the role tool message with the tool output")
	}
	if bodies := mock.RecordedChatBodiesSnapshot(); len(bodies) != 2 {
		t.Errorf("upstream chat request count = %d, want 2 (one per turn)", len(bodies))
	}
}
