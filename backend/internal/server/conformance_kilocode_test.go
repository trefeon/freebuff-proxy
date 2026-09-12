package server_test

// Conformance replay for Kilo Code (Kilo-Org/kilocode, an opencode-derived
// fork that reuses the same packages/llm wire layer) against the proxy's
// Anthropic Messages surface, in the exact kilocode wire shape (WIRE-NOTES:
// reference/agents/kilocode/WIRE-NOTES.md):
//
//   - x-api-key auth (runner/model.ts:168-175, providers/anthropic.ts:26-30)
//     + anthropic-version 2023-06-01 (anthropic-messages.ts:849) +
//     anthropic-beta "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14"
//     (packages/core/src/plugin/provider/anthropic.ts:12-15) — the gateway
//     must never 400 on that beta bundle.
//   - system as an array of text parts carrying cache_control:{type:"ephemeral"}
//     (prompt caching on by default) and tools with cache_control —
//     tolerated, not forwarded (no prompt-cache marker upstream).
//   - thinking {type:"enabled", budget_tokens} (anthropic-messages.ts:646-660)
//     — reasoning surfaces as a thinking content block + thinking_delta +
//     signature_delta.
//   - stream:true + max_tokens hard-required fields
//     (anthropicBodyFields, anthropic-messages.ts:151-165).
//
// CRITICAL kilocode transport fact (shared.ts:261-267): [DONE] and empty
// data: lines are FILTERED by the client but are NOT stream terminators —
// the response stream ends only when the HTTP body closes. The relay must
// finalize on upstream EOF with message_stop, and every SSE frame must be
// pure JSON (Sse.decode drops malformed data lines silently, so a single
// non-JSON frame would desync the client's lifecycle with no error).
// Retry contract (executor.ts:96, 353-364): kilocode retries only
// 429/503/504/529/>=500 — a clean 200 with a correct stream is what keeps
// it from churning.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// kiloFramesOf parses every SSE data: line of a kilocode /v1/messages body
// and FAILS on any non-JSON frame. kilocode's Framing.sse → Sse.decode drops
// malformed data lines silently, so the gateway must never emit one
// (comments like the ": connecting" grace line are not data lines and are
// excluded here; "[DONE]" is filtered client-side but a separate assertion
// pins its absence).
func kiloFramesOf(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rawLine := range strings.Split(body, "\n") {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var dm map[string]any
		if err := json.Unmarshal([]byte(payload), &dm); err != nil {
			t.Fatalf("SSE frame is not valid JSON (kilocode drops it silently): %v — %q", err, payload)
		}
		out = append(out, dm)
	}
	return out
}

// TestConformanceKilocodeAnthropicWire replays kilocode's Anthropic turn:
// the exact header bundle (x-api-key + anthropic-version + the
// interleaved-thinking/fine-grained-tool-streaming betas), system as an
// array of text parts with cache_control, thinking with budget_tokens,
// tools with cache_control, max_tokens, stream:true. Asserts: 200 (never a
// 400 on the beta bundle or the cache_control shapes), the block sequence
// thinking → text → tool_use with thinking_delta/signature_delta closing the
// thinking block, the tool_use input assembled from input_json_delta
// fragments, the stream terminating on BODY CLOSE (frames end after
// message_stop — no [DONE] sentinel required or emitted), and that every
// frame is valid JSON.
func TestConformanceKilocodeAnthropicWire(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-kilo1", 303,
			`"choices":[{"index":0,"delta":{"reasoning_content":"I should inspect the directory first."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-kilo1", 303,
			`"choices":[{"index":0,"delta":{"content":"Let me look at the repo."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-kilo1", 303,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_kilo_1","type":"function","function":{"name":"kilo_shell","arguments":"{\"cmd\":\"ls"}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-kilo1", 303,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" -la\"}"}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-kilo1", 303,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":60,"completion_tokens":44,"total_tokens":104}`)))
		// No upstream [DONE] is required: kilocode ends the stream on HTTP
		// body close — the relay finalizes on this EOF.
	}
	ts, _ := newTestServer(t, nil, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "kilo-key",
		"anthropic-version": "2023-06-01",
		"anthropic-beta":    "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14",
	}
	body := `{
		"model": "deepseek/deepseek-v4-flash",
		"max_tokens": 4096,
		"system": [{"type": "text", "text": "You are Kilo, a coding agent.", "cache_control": {"type": "ephemeral"}}],
		"thinking": {"type": "enabled", "budget_tokens": 4096},
		"messages": [{"role": "user", "content": "List the current directory"}],
		"tools": [{"name": "kilo_shell", "description": "Run a shell command", "cache_control": {"type": "ephemeral"}, "input_schema": {"type": "object", "properties": {"cmd": {"type": "string"}}, "required": ["cmd"]}}],
		"stream": true
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (kilocode header bundle + cache_control shapes must never 400): %s", resp.StatusCode, truncate(string(data), 300))
	}
	if ver := resp.Header.Get("anthropic-version"); ver != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", ver)
	}
	sse := string(data)
	// The client filters [DONE] but does NOT treat it as a terminator: the
	// stream ends on body close. Assert no sentinel is emitted (finalize is
	// message_stop on upstream EOF) and that every frame is pure JSON.
	if strings.Contains(sse, "[DONE]") {
		t.Error("stream must not emit a [DONE] sentinel (kilocode does not treat it as a terminator)")
	}
	events := kiloFramesOf(t, sse)
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "content_block_start", "content_block_delta", "content_block_stop", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := replayEventTypes(events); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}

	// Thinking block (index 0) opens first and closes with the
	// signature_delta before the text/tool blocks open — the client closes
	// reasoning on signature_delta (anthropic-messages.ts:714-736).
	var thinkingIdx = -1
	for _, ev := range events {
		if ev["type"] != "content_block_start" {
			continue
		}
		cb, _ := ev["content_block"].(map[string]any)
		if cb == nil || cb["type"] != "thinking" {
			continue
		}
		thinkingIdx = replayEventIndex(ev)
	}
	if thinkingIdx != 0 {
		t.Fatalf("thinking block index = %d, want 0", thinkingIdx)
	}
	var sawThinkingDelta, sawSignatureDelta bool
	for _, ev := range events {
		if ev["type"] != "content_block_delta" || replayEventIndex(ev) != thinkingIdx {
			continue
		}
		d, _ := ev["delta"].(map[string]any)
		switch d["type"] {
		case "thinking_delta":
			if d["thinking"] != "I should inspect the directory first." {
				t.Errorf("thinking_delta = %v, want the full reasoning text", d["thinking"])
			}
			sawThinkingDelta = true
		case "signature_delta":
			sawSignatureDelta = true
		}
	}
	if !sawThinkingDelta || !sawSignatureDelta {
		t.Errorf("thinking lifecycle incomplete: thinking_delta=%v signature_delta=%v", sawThinkingDelta, sawSignatureDelta)
	}

	// The tool_use block: client name/id restored, input assembled from the
	// input_json_delta fragments against the block index.
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
	if toolIdx != 2 || toolName != "kilo_shell" || toolID != "call_kilo_1" {
		t.Errorf("tool_use = index %d name %q id %q, want index 2 kilo_shell/call_kilo_1", toolIdx, toolName, toolID)
	}
	if args := replayInputFragments(events, toolIdx); args != `{"cmd":"ls -la"}` {
		t.Errorf("assembled tool_use input = %q, want {\"cmd\":\"ls -la\"}", args)
	}

	// Terminal contract: message_delta carries the tool_use stop reason and
	// authoritative usage; the frames END on message_stop (body close).
	stop, usage := replayMessageDelta(events)
	if stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stop)
	}
	if out, _ := usage["output_tokens"].(float64); out != 44 {
		t.Errorf("usage.output_tokens = %v, want 44", usage["output_tokens"])
	}

	// The translated upstream chat body: system text forwarded, max_tokens
	// forwarded (hard-required by kilocode), reasoning_effort derived from
	// the thinking budget (4096 → medium, clamped to high for
	// deepseek-v4-flash — #112 ladder rule), wrapped tool, and cache_control
	// never leaked (no prompt-cache marker upstream — parity with
	// TestReplayGooseAnthropicCacheControl).
	if !mock.BodyContains("You are Kilo, a coding agent.") {
		t.Error("upstream body missing the system text")
	}
	if !mock.BodyContains(`"max_tokens":4096`) {
		t.Error("upstream body missing max_tokens:4096")
	}
	if !mock.BodyContains(`"reasoning_effort":"high"`) {
		t.Error("upstream body missing reasoning_effort (want clamped high)")
	}
	if !mock.BodyContains(`"kilo_shell"`) {
		t.Error("upstream body missing the wrapped tool")
	}
	if mock.BodyContains("cache_control") {
		t.Error("cache_control leaked into the upstream chat body (no prompt-cache marker upstream)")
	}
}
