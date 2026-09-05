package server_test

// Hermetic "real user usage" replay tests for the Anthropic /v1/messages and
// /v1/messages/count_tokens surfaces. Each test drives the proxy's live HTTP
// surface exactly the way one harness client does, with testutil.MockUpstream
// as the fake codebuff.com upstream. Wire shapes mirror the
// devdocs/reviews/user-flow-map.md replay-<client>-<scenario> scenarios:
// claude-code (Bearer + anthropic-beta, 2-turn tool loop), goose (x-api-key,
// system[].cache_control, always-present max_tokens, streaming tool use),
// qwen-code (Bearer identity bundle on a non-anthropic host), and the error
// envelopes the map pins (banned, per-IP 429, count_tokens for paused ids).
// NEW TEST FILE ONLY — sibling files must not be touched.
// FINDING RECORDED + FIXED during this replay session (no prod change from
// this file): intOf (anthropic_stream.go) lacked an int64 case, so
// openAIUsageToAnthropic's int64 values were dropped in the streamed
// message_delta usage (always output_tokens 0). The fix landed while this
// slice ran; the strict usage assertions below verify the fixed contract.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// replayEventTypes returns the ordered event type names of an Anthropic SSE
// body (message_start, content_block_*, message_delta, message_stop).
func replayEventTypes(events []map[string]any) []string {
	types := make([]string, 0, len(events))
	for _, ev := range events {
		if t, ok := ev["type"].(string); ok {
			types = append(types, t)
		}
	}
	return types
}

// replayEventIndex returns the event's content block index (-1 when absent).
func replayEventIndex(ev map[string]any) int {
	if f, ok := ev["index"].(float64); ok {
		return int(f)
	}
	return -1
}

// replayInputFragments joins the input_json_delta partial_json fragments
// delivered against one content block index — the client-side rule for
// assembling a tool_use input from a stream.
func replayInputFragments(events []map[string]any, blockIdx int) string {
	var sb strings.Builder
	for _, ev := range events {
		if ev["type"] != "content_block_delta" {
			continue
		}
		if replayEventIndex(ev) != blockIdx {
			continue
		}
		delta, _ := ev["delta"].(map[string]any)
		if delta == nil || delta["type"] != "input_json_delta" {
			continue
		}
		if frag, ok := delta["partial_json"].(string); ok {
			sb.WriteString(frag)
		}
	}
	return sb.String()
}

// replayTextJoin joins every text_delta fragment — the client-side rule for
// assembling streamed text content.
func replayTextJoin(events []map[string]any) string {
	var sb strings.Builder
	for _, ev := range events {
		if ev["type"] != "content_block_delta" {
			continue
		}
		delta, _ := ev["delta"].(map[string]any)
		if delta == nil || delta["type"] != "text_delta" {
			continue
		}
		if text, ok := delta["text"].(string); ok {
			sb.WriteString(text)
		}
	}
	return sb.String()
}

// replayMessageDelta returns the terminal message_delta's stop_reason and
// usage map ("" / nil when the stream never finalized).
func replayMessageDelta(events []map[string]any) (string, map[string]any) {
	for _, ev := range events {
		if ev["type"] != "message_delta" {
			continue
		}
		stopReason := ""
		if delta, ok := ev["delta"].(map[string]any); ok {
			stopReason, _ = delta["stop_reason"].(string)
		}
		usage, _ := ev["usage"].(map[string]any)
		return stopReason, usage
	}
	return "", nil
}

// TestReplayClaudeCodeAnthropicToolLoop replays the claude-code 2.x wire for
// a 2-turn tool loop (user → tool_use → tool_result → final answer) against
// /v1/messages. Claude Code headers: Authorization: Bearer, anthropic-version,
// anthropic-beta "interleaved-thinking-2025-05-14, effort-2025-05-13". The
// num_turns semantics asserted: the tool_use block round-trips, the
// tool_result is accepted and translated, and the second turn produces final
// text content with stop_reason end_turn — plus the streamed content_block
// lifecycle (thinking closed before tool_use opens; input assembled from
// input_json_delta fragments) and unknown beta values tolerated.
func TestReplayClaudeCodeAnthropicToolLoop(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var mu sync.Mutex
	turn := 0
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		turn++
		cur := turn
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		switch cur {
		case 1: // upstream answers the tool call: thinking + Bash + end_turn pseudo-tool
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-cc1", 1, `"choices":[{"index":0,"delta":{"reasoning_content":"I need to inspect the directory first.","role":"assistant"},"finish_reason":null}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-cc1", 1, `"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_bash_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}},{"index":1,"id":"call_end_turn_999","type":"function","function":{"name":"end_turn","arguments":"{}"}}]},"index":0}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-cc1", 1, `"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls -la\"}"}}]},"index":0}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-cc1", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":120,"completion_tokens":32,"total_tokens":152,"completion_tokens_details":{"reasoning_tokens":18}}`)))
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default: // turn 2: final answer
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-cc2", 1, `"choices":[{"index":0,"delta":{"content":"Done. The directory contains:\nfile1.txt\ndir1\n","role":"assistant"},"finish_reason":null}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-cc2", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":90,"completion_tokens":24,"total_tokens":114}`)))
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
	}

	ts, _ := newTestServerCfg(t, []string{"claude-code-key"}, nil, mock)
	baseHeaders := map[string]string{
		"Content-Type":      "application/json",
		"Authorization":     "Bearer claude-code-key",
		"anthropic-version": "2023-06-01",
		"anthropic-beta":    "interleaved-thinking-2025-05-14, effort-2025-05-13",
	}

	// --- Turn 1: request with tools + thinking, expect a streamed tool_use ---
	turn1Body := `{
		"model": "deepseek/deepseek-v4-flash",
		"max_tokens": 4096,
		"system": "You are Claude Code, a coding agent.",
		"messages": [{"role": "user", "content": "List the files in the current directory"}],
		"tools": [{"name": "Bash", "description": "Execute a shell command and return its output", "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}],
		"thinking": {"type": "enabled", "budget_tokens": 2048},
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
	// Exact sequential lifecycle: message_start → thinking block open →
	// thinking_delta → signature_delta → thinking block stop → tool_use block
	// open → input_json_delta ×2 → tool_use stop → message_delta → message_stop.
	wantTypes1 := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop"}
	if got := replayEventTypes(events1); strings.Join(got, ",") != strings.Join(wantTypes1, ",") {
		t.Fatalf("turn 1 event sequence = %v, want %v", got, wantTypes1)
	}

	var thinkingIdx, toolIdx = -1, -1
	var toolName, toolID string
	var toolBlocks int
	var sawEndTurnBlock bool
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
				toolBlocks++
				toolIdx = replayEventIndex(ev)
				toolName, _ = cb["name"].(string)
				toolID, _ = cb["id"].(string)
				if cb["name"] == "end_turn" {
					sawEndTurnBlock = true
				}
				if input, ok := cb["input"].(map[string]any); !ok || len(input) != 0 {
					t.Errorf("tool_use content_block_start input = %v, want empty object (deltas carry the input)", cb["input"])
				}
			}
		case "content_block_delta":
			delta, _ := ev["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			switch delta["type"] {
			case "thinking_delta":
				if replayEventIndex(ev) != thinkingIdx {
					t.Errorf("thinking_delta index = %d, want %d (open thinking block)", replayEventIndex(ev), thinkingIdx)
				}
			case "input_json_delta":
				if replayEventIndex(ev) != toolIdx {
					t.Errorf("input_json_delta index = %d, want %d (open tool_use block)", replayEventIndex(ev), toolIdx)
				}
			}
		}
	}
	if toolBlocks != 1 {
		t.Errorf("tool_use content_block_start count = %d, want exactly 1 (end_turn pseudo-tool must be stripped)", toolBlocks)
	}
	if sawEndTurnBlock {
		t.Error("end_turn pseudo-tool leaked as a client-visible tool_use block")
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
	if msg, ok := events1[0]["message"].(map[string]any); ok {
		if usage, ok := msg["usage"].(map[string]any); ok {
			if in, _ := usage["input_tokens"].(float64); in <= 0 {
				t.Errorf("message_start usage.input_tokens = %v, want > 0", usage["input_tokens"])
			}
		}
		if model, _ := msg["model"].(string); model != modelA {
			t.Errorf("message_start model = %q, want %q", model, modelA)
		}
	} else {
		t.Error("message_start missing message object")
	}

	// --- Turn 2: feed tool_use + tool_result back, expect final text ---
	turn2Body := `{
		"model": "deepseek/deepseek-v4-flash",
		"max_tokens": 4096,
		"system": "You are Claude Code, a coding agent.",
		"messages": [
			{"role": "user", "content": "List the files in the current directory"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "I need to inspect the directory first.", "signature": ""},
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
		t.Fatalf("turn 2 status = %d, want 200: %s", resp2.StatusCode, truncate(string(data2), 300))
	}
	events2 := collectAnthropicEvents(t, string(data2))
	wantTypes2 := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := replayEventTypes(events2); strings.Join(got, ",") != strings.Join(wantTypes2, ",") {
		t.Fatalf("turn 2 event sequence = %v, want %v", got, wantTypes2)
	}
	stop2, usage2 := replayMessageDelta(events2)
	if stop2 != "end_turn" {
		t.Errorf("turn 2 stop_reason = %q, want end_turn", stop2)
	}
	if out, _ := usage2["output_tokens"].(float64); out != 24 {
		t.Errorf("turn 2 usage.output_tokens = %v, want 24", usage2["output_tokens"])
	}
	if text := replayTextJoin(events2); text != "Done. The directory contains:\nfile1.txt\ndir1\n" {
		t.Errorf("turn 2 assembled text = %q, want the final answer", text)
	}

	// The tool_result must reach the upstream as a chat role "tool" message
	// with the echoed tool_use id (num_turns semantics: accepted).
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

// TestReplayGooseAnthropicCacheControl replays the goose Anthropic-engine wire:
// x-api-key auth, anthropic-version, system as an array of text parts carrying
// cache_control:{type:"ephemeral"} (prompt caching on by default), max_tokens
// always present (4096), tools, unconditional stream. Asserts the
// cache_control shape is tolerated (never 400), max_tokens reaches the
// upstream, input_json_delta fragments assemble into the tool_use input, and
// message_delta carries usage + stop_reason tool_use before message_stop.
func TestReplayGooseAnthropicCacheControl(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-goose1", 1, `"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_exec_1","type":"function","function":{"name":"execute_shell","arguments":"{\"cmd\":\"ls"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-goose1", 1, `"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" -la\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-goose1", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":60,"completion_tokens":44,"total_tokens":104}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, []string{"goose-key"}, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "goose-key",
		"anthropic-version": "2023-06-01",
	}
	gooseBody := `{
		"model": "deepseek/deepseek-v4-flash",
		"max_tokens": 4096,
		"system": [{"type": "text", "text": "You are Goose, a coding agent.", "cache_control": {"type": "ephemeral"}}],
		"messages": [{"role": "user", "content": "List the contents of the current directory"}],
		"tools": [{"name": "execute_shell", "description": "Run a shell command", "input_schema": {"type": "object", "properties": {"cmd": {"type": "string"}}, "required": ["cmd"]}}],
		"stream": true
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(gooseBody), headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (cache_control in system parts must be tolerated): %s", resp.StatusCode, truncate(string(data), 300))
	}
	events := collectAnthropicEvents(t, string(data))
	wantTypes := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := replayEventTypes(events); strings.Join(got, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event sequence = %v, want %v", got, wantTypes)
	}
	var toolIdx = -1
	var toolName, toolID string
	for _, ev := range events {
		if ev["type"] != "content_block_start" {
			continue
		}
		cb, _ := ev["content_block"].(map[string]any)
		if cb == nil || cb["type"] != "tool_use" {
			continue
		}
		toolIdx = replayEventIndex(ev)
		toolName, _ = cb["name"].(string)
		toolID, _ = cb["id"].(string)
	}
	if toolName != "execute_shell" || toolID != "call_exec_1" {
		t.Errorf("tool_use name/id = %q/%q, want execute_shell/call_exec_1", toolName, toolID)
	}
	if args := replayInputFragments(events, toolIdx); args != `{"cmd":"ls -la"}` {
		t.Errorf("assembled tool_use input = %q, want {\"cmd\":\"ls -la\"}", args)
	}
	stop, usage := replayMessageDelta(events)
	if stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stop)
	}
	if out, _ := usage["output_tokens"].(float64); out != 44 {
		t.Errorf("usage.output_tokens = %v, want 44", usage["output_tokens"])
	}

	// Wire facts: the client's max_tokens must reach the upstream, the system
	// text must be forwarded, and cache_control must not be leaked upstream.
	if !mock.BodyContains(`"max_tokens":4096`) {
		t.Error("upstream chat body missing max_tokens:4096")
	}
	if !mock.BodyContains("You are Goose") {
		t.Error("upstream chat body missing the system prompt text")
	}
	if mock.BodyContains("cache_control") {
		t.Error("cache_control leaked into the upstream chat body (no prompt-cache marker upstream)")
	}

	// max_tokens is ALWAYS present: when a client omits it, the proxy must
	// still forward a concrete value rather than the raw body.
	noMaxBody := `{"model":"deepseek/deepseek-v4-flash","system":[{"type":"text","text":"You are Goose, a coding agent.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}],"stream":true}`
	respNoMax, dataNoMax := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(noMaxBody), headers)
	if respNoMax.StatusCode != http.StatusOK {
		t.Fatalf("no-max_tokens status = %d, want 200: %s", respNoMax.StatusCode, truncate(string(dataNoMax), 300))
	}
	if evs := collectAnthropicEvents(t, string(dataNoMax)); replayEventTypes(evs)[len(evs)-1] != "message_stop" {
		t.Error("no-max_tokens stream did not terminate with message_stop")
	}
	if last := mock.LastChatBody(); !strings.Contains(last, `"max_tokens":`) {
		t.Errorf("no-max_tokens upstream body = %s, want an injected max_tokens", last)
	}
}

// TestReplayMessagesBannedEnvelope pins the Anthropic error-envelope shape
// for refused requests: a banned upstream account is a 403
// {"type":"error","error":{"type":"permission_error","code":"account_banned"}}
// and a withdrawn (paused/capped-away) model is a 400 invalid_request_error
// naming the replacement — never the OpenAI-shaped body writeJSONError
// produces. is_error-style parsing of the envelope must succeed.
func TestReplayMessagesBannedEnvelope(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true // every route 403 {"status":"banned", "resumes_at": ...}
	ts, _ := newTestServer(t, []string{"replay-key"}, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "replay-key",
		"anthropic-version": "2023-06-01",
	}
	body := `{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("banned response is not parseable as an error object: %v: %s", err, data)
	}
	if envelope.Type != "error" {
		t.Errorf("top-level type = %q, want error (Anthropic envelope, not OpenAI shape)", envelope.Type)
	}
	if envelope.Error.Type != "permission_error" {
		t.Errorf("error.type = %q, want permission_error", envelope.Error.Type)
	}
	if envelope.Error.Code != "account_banned" {
		t.Errorf("error.code = %q, want account_banned", envelope.Error.Code)
	}
	if !strings.Contains(envelope.Error.Message, "banned") {
		t.Errorf("error.message = %q, want the ban copy", envelope.Error.Message)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Error("banned response missing Retry-After")
	}
	if ver := resp.Header.Get("anthropic-version"); ver != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", ver)
	}
}

// TestReplayMessagesUnavailableModelEnvelope is the capped/unavailable-model
// half of the envelope contract: a recognized-but-withdrawn id on /v1/messages
// yields a 400 Anthropic invalid_request_error naming the replacement, never
// a refusal in OpenAI shape.
func TestReplayMessagesUnavailableModelEnvelope(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, []string{"replay-key"}, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "replay-key",
		"anthropic-version": "2023-06-01",
	}
	body := `{"model":"minimax/minimax-m3","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("response is not parseable: %v: %s", err, data)
	}
	if envelope.Type != "error" {
		t.Errorf("top-level type = %q, want error", envelope.Type)
	}
	if envelope.Error.Type != "invalid_request_error" || envelope.Error.Code != "invalid_request_error" {
		t.Errorf("error.type/code = %q/%q, want invalid_request_error", envelope.Error.Type, envelope.Error.Code)
	}
	if !strings.Contains(envelope.Error.Message, "MiniMax M3 is no longer available") {
		t.Errorf("error.message = %q, want the withdrawn-model copy naming the replacement", envelope.Error.Message)
	}
	if mock.RequestCount() != 0 {
		t.Errorf("withdrawn model reached the upstream (%d requests); it must be refused locally", mock.RequestCount())
	}
}

// TestReplayMessages429Envelope replays the per-IP limiter tripping on the
// Anthropic surface: the third request from one client IP gets a 429 with the
// Anthropic error envelope (rate_limit_error / rate_limit_exceeded) plus
// Retry-After, not an OpenAI-shaped body.
func TestReplayMessages429Envelope(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("cmpl-replay429", 1, `"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("cmpl-replay429", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
	ts, _ := newTestServerCfg(t, []string{"replay-key"}, func(c *config.Config) {
		c.RateLimitPerIP = 1.0
		c.RateLimitBurst = 2
	}, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "replay-key",
		"anthropic-version": "2023-06-01",
	}
	body := `{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	for i := range 2 {
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d should succeed, got %d: %s", i+1, resp.StatusCode, truncate(string(data), 200))
		}
	}
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("3rd request status = %d, want 429: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("429 response is not parseable: %v: %s", err, data)
	}
	if envelope.Type != "error" {
		t.Errorf("top-level type = %q, want error (Anthropic envelope)", envelope.Type)
	}
	if envelope.Error.Type != "rate_limit_error" {
		t.Errorf("error.type = %q, want rate_limit_error", envelope.Error.Type)
	}
	if envelope.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error.code = %q, want rate_limit_exceeded", envelope.Error.Code)
	}
	if !strings.Contains(envelope.Error.Message, "rate limit exceeded") {
		t.Errorf("error.message = %q, want rate limit exceeded copy", envelope.Error.Message)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" || ra == "0" {
		t.Errorf("Retry-After = %q, want a positive retry hint", ra)
	}
	if ver := resp.Header.Get("anthropic-version"); ver != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", ver)
	}
}

// TestReplayMessagesTurnSpendLimited replays an upstream turn_spend_limit
// refusal on the Anthropic surface: /v1/messages shares chatCore/writeError
// with the OpenAI surface, so it must get the same terminal framing — 429
// + Anthropic envelope (rate_limit_error / turn_spend_limited) with the
// upstream loop warning and NO Retry-After — never the retry drumbeat.
func TestReplayMessagesTurnSpendLimited(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var mu sync.Mutex
	chatCalls := 0
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		chatCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"turn_spend_limit","message":"Something went wrong with this turn.","retryAfterMs":60000}`)
	}
	ts, _ := newTestServerCfg(t, []string{"replay-key"}, nil, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "replay-key",
		"anthropic-version": "2023-06-01",
	}
	body := `{"model":"deepseek/deepseek-v4-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("429 response is not parseable: %v: %s", err, data)
	}
	if envelope.Type != "error" {
		t.Errorf("top-level type = %q, want error (Anthropic envelope)", envelope.Type)
	}
	if envelope.Error.Type != "rate_limit_error" {
		t.Errorf("error.type = %q, want rate_limit_error", envelope.Error.Type)
	}
	if envelope.Error.Code != "turn_spend_limited" {
		t.Errorf("error.code = %q, want turn_spend_limited", envelope.Error.Code)
	}
	if !strings.Contains(envelope.Error.Message, "Something went wrong with this turn") {
		t.Errorf("error.message missing the upstream loop warning: %q", envelope.Error.Message)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none (no retry drumbeat for a killed turn)", ra)
	}
	if ver := resp.Header.Get("anthropic-version"); ver != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", ver)
	}
	mu.Lock()
	defer mu.Unlock()
	if chatCalls != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (never re-POST into a turn-spend refusal)", chatCalls)
	}
}

// TestReplayMessagesCountTokensPaused replays the issue #140 count_tokens
// contract: a paused (withdrawn) model id fed to /v1/messages/count_tokens
// returns a NUMBER — {"input_tokens": N} with zero upstream contact — not a
// refusal, because released clients still list the id in their pickers and
// count_tokens is a purely local estimate.
func TestReplayMessagesCountTokensPaused(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, []string{"replay-key"}, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "replay-key",
		"anthropic-version": "2023-06-01",
	}
	body := `{"model":"minimax/minimax-m3","messages":[{"role":"user","content":"hello"}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages/count_tokens", []byte(body), headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (paused ids stay recognized for count_tokens): %s", resp.StatusCode, truncate(string(data), 300))
	}
	if ver := resp.Header.Get("anthropic-version"); ver != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", ver)
	}
	var result struct {
		InputTokens json.RawMessage `json:"input_tokens"`
		Type        string          `json:"type"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("count_tokens response is not parseable: %v: %s", err, data)
	}
	if len(result.InputTokens) == 0 {
		t.Fatalf("input_tokens missing: %s", data)
	}
	if result.Type == "error" {
		t.Fatalf("count_tokens returned an error envelope for a paused id: %s", data)
	}
	var n int
	if err := json.Unmarshal(result.InputTokens, &n); err != nil || n <= 0 {
		t.Fatalf("input_tokens = %s, want a positive integer (issue #140: number, not refusal)", result.InputTokens)
	}
	if rc := mock.RequestCount(); rc != 0 {
		t.Errorf("count_tokens made %d upstream request(s); it must be purely local", rc)
	}
}

// TestReplayQwenAnthropicIdentityBundle replays qwen-code's Anthropic-surface
// identity against the proxy: Authorization: Bearer together with the proxy
// identity bundle (x-app: cli, User-Agent: claude-cli) on a non-anthropic
// host must be tolerated, and the x-api-key alternative must also work.
func TestReplayQwenAnthropicIdentityBundle(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-qwen1", 1, `"choices":[{"index":0,"delta":{"content":"hello from qwen","role":"assistant"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-qwen1", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, []string{"qwen-key"}, mock)
	body := `{"model":"deepseek/deepseek-v4-flash","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],"stream":true}`

	// Identity bundle: Bearer + x-app + claude-cli UA.
	bundleHeaders := map[string]string{
		"Content-Type":      "application/json",
		"Authorization":     "Bearer qwen-key",
		"x-app":             "cli",
		"User-Agent":        "claude-cli/2.1.229 (external, cli)",
		"anthropic-version": "2023-06-01",
	}
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), bundleHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Bearer bundle status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	events := collectAnthropicEvents(t, string(data))
	if typ := replayEventTypes(events); typ[len(typ)-1] != "message_stop" {
		t.Errorf("Bearer bundle stream did not terminate with message_stop: %v", typ)
	}
	if text := replayTextJoin(events); text != "hello from qwen" {
		t.Errorf("assembled text = %q, want hello from qwen", text)
	}

	// Alternative identity: x-api-key alone.
	xkeyHeaders := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "qwen-key",
		"anthropic-version": "2023-06-01",
	}
	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), xkeyHeaders)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("x-api-key status = %d, want 200: %s", resp2.StatusCode, truncate(string(data2), 300))
	}

	// CORS preflight on the same surface: 204 with the credential/identity
	// headers allowed.
	preflight, _ := doJSON(t, http.MethodOptions, ts.URL+"/v1/messages", nil, map[string]string{
		"Origin":                         "http://example.com",
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "content-type,authorization,x-api-key,anthropic-version",
	})
	if preflight.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d, want 204", preflight.StatusCode)
	}
	allowHeaders := preflight.Header.Get("Access-Control-Allow-Headers")
	for _, want := range []string{"Content-Type", "Authorization", "x-api-key", "anthropic-version"} {
		if !strings.Contains(allowHeaders, want) {
			t.Errorf("Access-Control-Allow-Headers = %q, missing %q", allowHeaders, want)
		}
	}
	if allow := preflight.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(allow, "POST") {
		t.Errorf("Access-Control-Allow-Methods = %q, want POST", allow)
	}
}

// TestReplayClaudeCodeBearerWithoutBeta is the claude-code baseline: the same
// Bearer-authenticated /v1/messages wire WITHOUT the anthropic-beta header
// still works — betas are optional, not required.
func TestReplayClaudeCodeBearerWithoutBeta(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-baseline", 1, `"choices":[{"index":0,"delta":{"content":"Baseline answer","role":"assistant"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-baseline", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":9,"total_tokens":39}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, []string{"claude-code-key"}, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"Authorization":     "Bearer claude-code-key",
		"anthropic-version": "2023-06-01",
	}
	if got := headers["anthropic-beta"]; got != "" {
		t.Fatalf("test precondition failed: anthropic-beta present (%q)", got)
	}
	body := `{"model":"deepseek/deepseek-v4-flash","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	events := collectAnthropicEvents(t, string(data))
	wantTypes := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := replayEventTypes(events); strings.Join(got, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event sequence = %v, want %v", got, wantTypes)
	}
	stop, usage := replayMessageDelta(events)
	if stop != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", stop)
	}
	if out, _ := usage["output_tokens"].(float64); out != 9 {
		t.Errorf("usage.output_tokens = %v, want 9", usage["output_tokens"])
	}
	if text := replayTextJoin(events); text != "Baseline answer" {
		t.Errorf("assembled text = %q, want Baseline answer", text)
	}
}
