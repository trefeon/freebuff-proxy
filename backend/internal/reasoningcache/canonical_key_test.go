package reasoningcache

import (
	"encoding/json"
	"testing"
	"time"
)

// Tests for the P2-5 canonical tool-calls binding: entries are bound to
// content PLUS the canonical identity of the tool calls, so tool-only turns
// (content empty or null — the Claude Code / aider pattern) are protected
// too, and the binding is shape-independent (flat / OpenAI-nested /
// Anthropic-input echoes of the same calls all bind).

func TestCanonicalizeToolCallsJSON(t *testing.T) {
	t.Run("flat and OpenAI-nested shapes produce the same key", func(t *testing.T) {
		flat := CanonicalizeToolCallsJSON(`[{"id":"call_1","name":"ls","arguments":"{}"}]`)
		nested := CanonicalizeToolCallsJSON(`[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]`)
		if flat == "" || flat != nested {
			t.Fatalf("canonical keys diverged: flat=%q nested=%q", flat, nested)
		}
		want := "call_1\x1fls\x1f{}"
		if flat != want {
			t.Fatalf("canonical key = %q, want %q", flat, want)
		}
	})

	t.Run("anthropic tool_use input shape also canonicalizes", func(t *testing.T) {
		anthropic := CanonicalizeToolCallsJSON(`[{"id":"toolu_1","name":"bash","input":{"command":"ls"}}]`)
		if anthropic == "" {
			t.Fatal("anthropic input shape failed to canonicalize")
		}
	})

	t.Run("elements without ids are skipped", func(t *testing.T) {
		key := CanonicalizeToolCallsJSON(`[{"name":"no_id","arguments":"{}"},{"id":"call_2","name":"ok","arguments":"{}"}]`)
		if key != "call_2\x1fok\x1f{}" {
			t.Fatalf("canonical key = %q, want only the id-bearing element", key)
		}
	})

	t.Run("degenerate inputs return empty", func(t *testing.T) {
		for name, raw := range map[string]string{
			"empty":       "",
			"not json":    "not json",
			"not array":   `{"id":"call_1"}`,
			"no elements": `[{"name":"x","arguments":"{}"}]`,
		} {
			if got := CanonicalizeToolCallsJSON(raw); got != "" {
				t.Errorf("%s: CanonicalizeToolCallsJSON = %q, want empty", name, got)
			}
		}
	})
}

func TestCanonicalToolKeyArgsNormalization(t *testing.T) {
	// The OpenAI surface echoes arguments verbatim; the Anthropic surface
	// round-trips them through a parsed input object (compact re-marshal).
	// Both must converge on the same canonical key.
	verbatim := CanonicalToolKey([][3]string{{"call_1", "f", `{"a": 1, "b":"two"}`}})
	compact := CanonicalToolKey([][3]string{{"call_1", "f", `{"a":1,"b":"two"}`}})
	if verbatim == "" || verbatim != compact {
		t.Fatalf("arguments normalization diverged: verbatim=%q compact=%q", verbatim, compact)
	}
	// Non-JSON argument strings pass through untouched (still bind).
	if got := CanonicalToolKey([][3]string{{"call_1", "f", "raw text"}}); got != "call_1\x1ff\x1fraw text" {
		t.Fatalf("non-JSON args = %q, want verbatim", got)
	}
}

func TestPutCanonicalToolIdentityBinding(t *testing.T) {
	t.Run("tool-only turn with matching identity hits", func(t *testing.T) {
		c := New(100, time.Hour)
		// The streaming pattern: content empty, identity via canonical key.
		c.PutCanonical([]string{"call_1"}, "", CanonicalToolKey([][3]string{{"call_1", "bash", `{"command":"ls"}`}}), "tool-only reasoning", "", "m")

		// Client echo: content null, OpenAI-shaped tool_calls — must hit on
		// tool-call identity alone.
		echo := `[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]`
		if r, _, ok := c.Get("call_1", "", echo); !ok || r != "tool-only reasoning" {
			t.Fatalf("expected tool-only bound hit, got (%q, %v)", r, ok)
		}
	})

	t.Run("same ids with different arguments is a foreign conversation", func(t *testing.T) {
		c := New(100, time.Hour)
		c.PutCanonical([]string{"call_1"}, "", CanonicalToolKey([][3]string{{"call_1", "bash", `{"command":"ls"}`}}), "reasoning A", "", "m")
		foreign := `[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"rm -rf /\"}"}}]`
		if r, _, ok := c.Get("call_1", "", foreign); ok {
			t.Fatalf("expected foreign-args echo to be rejected, got %q", r)
		}
	})

	t.Run("shape-independent Put vs Get binding", func(t *testing.T) {
		c := New(100, time.Hour)
		// Put with the flat shape...
		c.Put([]string{"call_9"}, "some content", `[{"id":"call_9","name":"f","arguments":"{}"}]`, "shape-free reasoning", "", "m")
		// ...Get with the OpenAI-nested echo (extra type/function nesting).
		nested := `[{"id":"call_9","type":"function","function":{"name":"f","arguments":"{}"}}]`
		if r, _, ok := c.Get("call_9", "some content", nested); !ok || r != "shape-free reasoning" {
			t.Fatalf("expected canonical equality across shapes, got (%q, %v)", r, ok)
		}
	})
	t.Run("entries without recorded identity reject tool identity they never had", func(t *testing.T) {
		c := New(100, time.Hour)
		c.Put([]string{"call_3"}, "content only", "", "reasoning C", "", "m")
		echo := `[{"id":"call_3","type":"function","function":{"name":"f","arguments":"{}"}}]`
		// The caller presents tool identity the entry never bound: the
		// binding check is fail-closed (mismatch → hash fallback, which
		// also misses). No production Put site stores tool ids without
		// identity anymore, so this only fires for foreign/legacy shapes.
		if _, _, ok := c.Get("call_3", "content only", echo); ok {
			t.Fatal("expected unverified tool identity to be rejected (fail-closed)")
		}
	})

	t.Run("zero-binding entries keep legacy toolID-only behavior", func(t *testing.T) {
		c := New(100, time.Hour)
		c.Put([]string{"call_z"}, "", "", "legacy reasoning", "", "m")
		if r, _, ok := c.Get("call_z", "", ""); !ok || r != "legacy reasoning" {
			t.Fatalf("expected zero-binding toolID hit, got (%q, %v)", r, ok)
		}
		// Even with tool identity presented: nothing to verify against.
		echo := `[{"id":"call_z","type":"function","function":{"name":"f","arguments":"{}"}}]`
		if r, _, ok := c.Get("call_z", "", echo); !ok || r != "legacy reasoning" {
			t.Fatalf("expected zero-binding entry to hit regardless of echo, got (%q, %v)", r, ok)
		}
	})

	t.Run("PutCanonical without identity binds on content like Put", func(t *testing.T) {
		c := New(100, time.Hour)
		c.PutCanonical([]string{"call_5"}, "content five", "", "reasoning five", "", "m")
		if r, _, ok := c.Get("call_5", "content five", ""); !ok || r != "reasoning five" {
			t.Fatalf("expected content-bound hit, got (%q, %v)", r, ok)
		}
		if _, _, ok := c.Get("call_5", "other content", ""); ok {
			t.Fatal("expected foreign content to be rejected")
		}
	})
}

func TestCanonicalKeyOrderingMatters(t *testing.T) {
	a := CanonicalToolKey([][3]string{{"call_1", "a", "{}"}, {"call_2", "b", "{}"}})
	b := CanonicalToolKey([][3]string{{"call_2", "b", "{}"}, {"call_1", "a", "{}"}})
	if a == b {
		t.Fatal("canonical key must be order-sensitive (wire order is part of identity)")
	}
}

// jsonMarshalOrderStable documents (by failing loudly if violated) the
// assumption behind args normalization: encoding/json marshals maps with
// sorted keys, so the compact re-marshal is deterministic.
func TestJSONMarshalOrderStable(t *testing.T) {
	b1, _ := json.Marshal(map[string]any{"z": 1, "a": 2})
	b2, _ := json.Marshal(map[string]any{"a": 2, "z": 1})
	if string(b1) != string(b2) {
		t.Fatalf("map marshal not key-sorted: %q vs %q", b1, b2)
	}
}
