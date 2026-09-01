package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/convert"
	"freebuff-proxy/backend/internal/reasoningcache"
)

// Round-trip regression for the P2-5 canonical tool-calls binding: the
// reasoning captured by the streaming relays under (toolIDs, canonical
// tool-call identity) must restore on the next request's normalizeMessages
// echo of the SAME calls even when the assistant turn carried no content at
// all (content null — the Claude Code / aider tool-only turn), and must NOT
// restore when a foreign conversation replays the same sequential
// tool_call_ids with different arguments or a different tool name.

func newRelayBindingServer(t *testing.T) *Server {
	t.Helper()
	s := testRelayServer()
	// The reasoning lookup is threaded per-request through
	// Server.convertOptions (issue #251): a populated per-Server cache is
	// the hook, no process-global installation.
	s.reasoningCache = reasoningcache.New(16, time.Hour)
	return s
}

func normalizeAssistantEcho(t *testing.T, content any, toolCalls []map[string]any, lookup func(string, string, string) (string, string, bool)) map[string]any {
	t.Helper()
	payload := map[string]any{
		"model": "test-model",
		"messages": []any{map[string]any{
			"role":       "assistant",
			"content":    content,
			"tool_calls": toolCalls,
		}},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	out, err := convert.NormalizeRequestOpts(b, "", convert.Options{ReasoningLookup: lookup})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m, _ := msgs[0].(map[string]any)
	if m == nil {
		t.Fatal("expected assistant message object")
	}
	return m
}

func bindingToolEcho(name, arguments string) map[string]any {
	return map[string]any{
		"id":   "call_1",
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
}

// (a)+(b): the openai streaming relay accumulates (id, name, arguments)
// per upstream index and stores the canonical identity via PutCanonical;
// the wire lookup in normalizeMessages presents the echo's tool_calls, so
// the binding holds for the owning conversation and rejects foreign ones.
func TestRelayBindingOpenAIStreamRoundTrip(t *testing.T) {
	s := newRelayBindingServer(t)

	// Tool-only stream: reasoning fragments + a tool call whose arguments
	// arrive split across two fragments, no content at all.
	ss := strings.Join([]string{
		testutilSSE(`{"id":"cmpl-rb","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"reasoning_content":"streamed thinking"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-rb","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-rb","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-rb","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")
	rec := httptest.NewRecorder()
	s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())

	t.Run("echo of the same calls with content null restores", func(t *testing.T) {
		m := normalizeAssistantEcho(t, nil, []map[string]any{bindingToolEcho("get_weather", `{"city":"SF"}`)}, s.reasoningCache.Get)
		if rc, _ := m["reasoning_content"].(string); rc != "streamed thinking" {
			t.Fatalf("expected streamed reasoning restored on tool-only echo, got %q", rc)
		}
	})

	t.Run("same ids with different arguments restore nothing", func(t *testing.T) {
		m := normalizeAssistantEcho(t, nil, []map[string]any{bindingToolEcho("get_weather", `{"city":"NYC"}`)}, s.reasoningCache.Get)
		if rc, exists := m["reasoning_content"]; exists {
			if str, isStr := rc.(string); isStr && str != "" {
				t.Fatalf("expected no restore for foreign arguments, got %q", str)
			}
		}
	})

	t.Run("same ids with a different tool name restore nothing", func(t *testing.T) {
		m := normalizeAssistantEcho(t, nil, []map[string]any{bindingToolEcho("other_tool", `{"city":"SF"}`)}, s.reasoningCache.Get)
		if rc, exists := m["reasoning_content"]; exists {
			if str, isStr := rc.(string); isStr && str != "" {
				t.Fatalf("expected no restore for foreign tool name, got %q", str)
			}
		}
	})
}

// (c): the anthropic streaming finalize stores (toolIDs, canonical identity
// of st's ordered tool states) via PutCanonical; the same two cases apply.
func TestRelayBindingAnthropicStreamRoundTrip(t *testing.T) {
	s := newRelayBindingServer(t)

	ss := strings.Join([]string{
		testutilSSE(`{"id":"cmpl-rb","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"deep thought"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-rb","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-rb","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")
	rec := httptest.NewRecorder()
	s.relayAnthropicStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", 0)

	t.Run("echo of the same calls with content null restores", func(t *testing.T) {
		m := normalizeAssistantEcho(t, nil, []map[string]any{bindingToolEcho("get_weather", `{"city":"SF"}`)}, s.reasoningCache.Get)
		if rc, _ := m["reasoning_content"].(string); rc != "deep thought" {
			t.Fatalf("expected finalize reasoning restored on tool-only echo, got %q", rc)
		}
	})
	t.Run("same ids with different arguments restore nothing", func(t *testing.T) {
		m := normalizeAssistantEcho(t, nil, []map[string]any{bindingToolEcho("get_weather", `{"city":"NYC"}`)}, s.reasoningCache.Get)
		if rc, exists := m["reasoning_content"]; exists {
			if str, isStr := rc.(string); isStr && str != "" {
				t.Fatalf("expected no restore for foreign arguments, got %q", str)
			}
		}
	})
}

// (d) at wire level: the non-streaming openai relay Put carries the raw
// tool_calls JSON; the client echo (same calls re-marshaled by the client,
// here with key order shuffled inside arguments) must still bind — the
// canonical key normalizes arguments, so shape/whitespace is irrelevant.
func TestRelayBindingNonStreamJSONRoundTrip(t *testing.T) {
	s := newRelayBindingServer(t)

	// Non-streaming Put with raw JSON (what relayJSON records), arguments
	// in non-compact form to prove normalization.
	s.reasoningCache.Put([]string{"call_1"}, "text before tool",
		`[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{ \"city\" : \"SF\" }"}}]`,
		"nonstream reasoning", "", "m")

	t.Run("client echo with whitespace-variant arguments binds", func(t *testing.T) {
		m := normalizeAssistantEcho(t, "text before tool", []map[string]any{bindingToolEcho("get_weather", `{"city":"SF"}`)}, s.reasoningCache.Get)
		if rc, _ := m["reasoning_content"].(string); rc != "nonstream reasoning" {
			t.Fatalf("expected non-streaming restore, got %q", rc)
		}
	})

	t.Run("foreign arguments still rejected", func(t *testing.T) {
		m := normalizeAssistantEcho(t, "text before tool", []map[string]any{bindingToolEcho("get_weather", `{"city":"NYC"}`)}, s.reasoningCache.Get)
		if rc, exists := m["reasoning_content"]; exists {
			if str, isStr := rc.(string); isStr && str != "" {
				t.Fatalf("expected no restore for foreign arguments, got %q", str)
			}
		}
	})
}
