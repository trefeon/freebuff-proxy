package server

// Regression tests for Responses -> Chat input translation.
//
// responsesInputToMessages used to skip "function_call" items outright while
// still translating their matching "function_call_output" into a role:"tool"
// message. That left the tool reply orphaned: a tool_call_id with no preceding
// assistant tool_calls entry. Chat backends drop such replies, so the model
// never saw the tool result and re-issued the identical call on every turn,
// which made agentic clients (Codex, which can only speak the Responses wire
// format) loop forever against this proxy.
//
// The contract these tests pin down: every function_call item must emerge as
// an assistant message carrying tool_calls, immediately before the tool
// message that answers it.

import (
	"testing"
)

// toolCallsOf returns the tool_calls slice of a translated message, or nil.
func toolCallsOf(t *testing.T, msg any) []any {
	t.Helper()
	m, ok := msg.(map[string]any)
	if !ok {
		return nil
	}
	tc, ok := m["tool_calls"].([]any)
	if !ok {
		return nil
	}
	return tc
}

func roleOf(t *testing.T, msg any) string {
	t.Helper()
	m, ok := msg.(map[string]any)
	if !ok {
		return ""
	}
	r, _ := m["role"].(string)
	return r
}

// TestResponsesInputReplaysFunctionCallBeforeItsOutput is the core regression:
// the assistant tool call must survive translation so its result is not
// orphaned.
func TestResponsesInputReplaysFunctionCallBeforeItsOutput(t *testing.T) {
	input := []any{
		map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "What is the weather in Berlin?"},
			},
		},
		map[string]any{
			"type":      "function_call",
			"call_id":   "call_w1",
			"name":      "get_weather",
			"arguments": `{"city":"Berlin"}`,
		},
		map[string]any{
			"type":    "function_call_output",
			"call_id": "call_w1",
			"output":  `{"temp_c":19}`,
		},
	}

	msgs := responsesInputToMessages(input, nil)

	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (user, assistant tool_calls, tool), got %d: %#v", len(msgs), msgs)
	}

	if got := roleOf(t, msgs[0]); got != "user" {
		t.Errorf("msgs[0] role = %q, want %q", got, "user")
	}

	// The assistant replay must sit directly before the tool reply.
	if got := roleOf(t, msgs[1]); got != "assistant" {
		t.Fatalf("msgs[1] role = %q, want %q (function_call was dropped)", got, "assistant")
	}
	calls := toolCallsOf(t, msgs[1])
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls length = %d, want 1", len(calls))
	}
	call, ok := calls[0].(map[string]any)
	if !ok {
		t.Fatalf("tool_calls[0] is %T, want map", calls[0])
	}
	if call["id"] != "call_w1" {
		t.Errorf("tool_calls[0].id = %v, want call_w1", call["id"])
	}
	if call["type"] != "function" {
		t.Errorf("tool_calls[0].type = %v, want function", call["type"])
	}
	fn, ok := call["function"].(map[string]any)
	if !ok {
		t.Fatalf("tool_calls[0].function is %T, want map", call["function"])
	}
	if fn["name"] != "get_weather" {
		t.Errorf("function.name = %v, want get_weather", fn["name"])
	}
	if fn["arguments"] != `{"city":"Berlin"}` {
		t.Errorf("function.arguments = %v, want the original arguments", fn["arguments"])
	}

	// And the tool reply must still reference it, so the pair is intact.
	toolMsg, ok := msgs[2].(map[string]any)
	if !ok {
		t.Fatalf("msgs[2] is %T, want map", msgs[2])
	}
	if toolMsg["role"] != "tool" {
		t.Errorf("msgs[2] role = %v, want tool", toolMsg["role"])
	}
	if toolMsg["tool_call_id"] != "call_w1" {
		t.Errorf("tool_call_id = %v, want call_w1 (orphaned tool reply)", toolMsg["tool_call_id"])
	}
}

// TestResponsesInputFunctionCallEmptyArguments: the Chat wire format requires
// arguments to be a JSON string, so an omitted one becomes "{}" rather than "".
func TestResponsesInputFunctionCallEmptyArguments(t *testing.T) {
	input := []any{
		map[string]any{"type": "function_call", "call_id": "c1", "name": "ping"},
	}

	msgs := responsesInputToMessages(input, nil)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	calls := toolCallsOf(t, msgs[0])
	if len(calls) != 1 {
		t.Fatalf("tool_calls length = %d, want 1", len(calls))
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != "{}" {
		t.Errorf("arguments = %q, want %q", fn["arguments"], "{}")
	}
}

// TestResponsesInputSkipsUnusableItems: a function_call missing the fields
// needed to build a valid tool_calls entry is dropped rather than emitted
// malformed, and genuinely non-replayable item types stay skipped.
func TestResponsesInputSkipsUnusableItems(t *testing.T) {
	cases := []struct {
		name string
		item map[string]any
	}{
		{"missing call_id", map[string]any{"type": "function_call", "name": "ping"}},
		{"missing name", map[string]any{"type": "function_call", "call_id": "c1"}},
		{"reasoning", map[string]any{"type": "reasoning", "id": "r1"}},
		{"item_reference", map[string]any{"type": "item_reference", "id": "i1"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := responsesInputToMessages([]any{tc.item}, nil)
			if len(msgs) != 0 {
				t.Fatalf("expected item to be skipped, got %d messages: %#v", len(msgs), msgs)
			}
		})
	}
}

// TestResponsesInputMultipleToolCallsKeepOrder covers a parallel tool-call
// turn: each call must be replayed before its own result, in order.
func TestResponsesInputMultipleToolCallsKeepOrder(t *testing.T) {
	input := []any{
		map[string]any{"type": "function_call", "call_id": "a", "name": "one", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "a", "output": "1"},
		map[string]any{"type": "function_call", "call_id": "b", "name": "two", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "b", "output": "2"},
	}

	msgs := responsesInputToMessages(input, nil)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d: %#v", len(msgs), msgs)
	}

	wantRoles := []string{"assistant", "tool", "assistant", "tool"}
	for i, want := range wantRoles {
		if got := roleOf(t, msgs[i]); got != want {
			t.Errorf("msgs[%d] role = %q, want %q", i, got, want)
		}
	}

	wantIDs := []string{"a", "b"}
	for i, idx := range []int{0, 2} {
		calls := toolCallsOf(t, msgs[idx])
		if len(calls) != 1 {
			t.Fatalf("msgs[%d] tool_calls length = %d, want 1", idx, len(calls))
		}
		if got := calls[0].(map[string]any)["id"]; got != wantIDs[i] {
			t.Errorf("msgs[%d] tool_calls[0].id = %v, want %v", idx, got, wantIDs[i])
		}
	}
}
