package convert

import (
	"encoding/json"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/reasoningcache"
)

// Regression for the review's P2-5 wire path (devdocs/review-2026-08-31.md):
// the toolID reasoning-restore lookup in normalizeMessages presents the
// message's own content and the canonical identity of its tool_calls as a
// binding, so a per-conversation sequential tool_call_id ("call_1") cannot
// restore another conversation's reasoning. The cache rejects bound
// mismatches (reasoningcache.Get); these tests pin that the wire lookup
// actually supplies the binding. The Put side mirrors what the relay Put
// sites record (content plus the raw tool_calls JSON).

var reviewFixTcJSON = `[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]`

func TestReasoningRestoreBindsContent(t *testing.T) {
	cache := reasoningcache.New(100, time.Hour)
	lookup := Options{ReasoningLookup: cache.Get}

	restoreRequest := func(t *testing.T, content string) map[string]any {
		t.Helper()
		payload := map[string]any{
			"model": "test-model",
			"messages": []any{map[string]any{
				"role":    "assistant",
				"content": content,
				"tool_calls": []any{map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "ls",
						"arguments": "{}",
					},
				}},
			}},
		}
		return normalizeEcho(t, payload, lookup)
	}

	t.Run("foreign content under colliding tool_call_id restores nothing", func(t *testing.T) {
		cache.Put([]string{"call_1"}, "conversation A assistant text", reviewFixTcJSON, "reasoning A", "", "m")
		m := restoreRequest(t, "conversation B assistant text")
		if rc, exists := m["reasoning_content"]; exists {
			if s, isStr := rc.(string); isStr && s != "" {
				t.Fatalf("expected no reasoning restore for foreign content, got %q", s)
			}
		}
	})

	t.Run("own content restores reasoning", func(t *testing.T) {
		cache.Put([]string{"call_1"}, "conversation C assistant text", reviewFixTcJSON, "reasoning C", "", "m")
		m := restoreRequest(t, "conversation C assistant text")
		if rc, _ := m["reasoning_content"].(string); rc != "reasoning C" {
			t.Fatalf("expected own-content restore, got %q", rc)
		}
	})
}

var reviewFixBashTcJSON = `[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]`

// The P2-5 canonical-binding upgrade: tool-only turns (assistant content
// empty or null — the Claude Code / aider pattern) are bound by tool-call
// identity alone, so they restore when the echo presents the SAME calls and
// stay silent for foreign arguments, regardless of the echo's wire shape.
func TestReasoningRestoreBindsCanonicalToolCalls(t *testing.T) {
	cache := reasoningcache.New(100, time.Hour)
	lookup := Options{ReasoningLookup: cache.Get}

	echoToolOnly := func(t *testing.T, toolCalls []any) map[string]any {
		t.Helper()
		payload := map[string]any{
			"model": "test-model",
			"messages": []any{map[string]any{
				"role":       "assistant",
				"content":    nil, // tool-only turn: content null
				"tool_calls": toolCalls,
			}},
		}
		return normalizeEcho(t, payload, lookup)
	}
	nestedCall := map[string]any{
		"id":   "call_1",
		"type": "function",
		"function": map[string]any{
			"name":      "bash",
			"arguments": `{"command":"ls"}`,
		},
	}
	flatCall := map[string]any{
		"id":        "call_1",
		"name":      "bash",
		"arguments": `{"command":"ls"}`,
	}

	t.Run("tool-only turn with null content restores on matching identity", func(t *testing.T) {
		cache.Put([]string{"call_1"}, "", reviewFixBashTcJSON, "tool-only reasoning", "", "m")
		m := echoToolOnly(t, []any{nestedCall})
		if rc, _ := m["reasoning_content"].(string); rc != "tool-only reasoning" {
			t.Fatalf("expected tool-only restore, got %q", rc)
		}
	})

	t.Run("foreign arguments under the same ids restore nothing", func(t *testing.T) {
		foreign := map[string]any{
			"id":   "call_1",
			"type": "function",
			"function": map[string]any{
				"name":      "bash",
				"arguments": `{"command":"rm -rf /"}`,
			},
		}
		m := echoToolOnly(t, []any{foreign})
		if rc, exists := m["reasoning_content"]; exists {
			if s, isStr := rc.(string); isStr && s != "" {
				t.Fatalf("expected no restore for foreign arguments, got %q", s)
			}
		}
	})

	t.Run("flat echo shape binds against nested Put shape", func(t *testing.T) {
		// The cache canonicalizes internally: the OpenAI-nested wire shape
		// recorded at Put time and the flat client echo must be equal keys.
		cache.Put([]string{"call_1"}, "", reviewFixBashTcJSON, "tool-only reasoning", "", "m")
		m := echoToolOnly(t, []any{flatCall})
		if rc, _ := m["reasoning_content"].(string); rc != "tool-only reasoning" {
			t.Fatalf("expected shape-independent restore, got %q", rc)
		}
	})
}

func normalizeEcho(t *testing.T, payload map[string]any, opts ...Options) map[string]any {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	out, err := NormalizeRequestOpts(b, "", o)
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
