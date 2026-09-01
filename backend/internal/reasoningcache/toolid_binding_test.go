package reasoningcache

import (
	"testing"
	"time"
)

// Tests for the review fix that binds cache entries to the
// (content, toolCallsJSON) recorded at Put time: Get verifies that binding on
// toolID hits so a colliding per-conversation sequential tool_call_id cannot
// restore another conversation's reasoning.

func TestGetToolIDContentBinding(t *testing.T) {
	t.Run("mismatch is a miss and correct content still hits", func(t *testing.T) {
		c := New(100, time.Hour)
		contentA := "conversation A content"
		tcA := `[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]`
		c.Put([]string{"call_1"}, contentA, tcA, "reasoning A", "", "modelA")

		// A different conversation replays its own content under the same
		// sequential tool_call_id: the toolID hit must be rejected.
		if r, s, ok := c.Get("call_1", "conversation B content", ""); ok {
			t.Fatalf("expected cross-conversation toolID hit to be rejected, got (%q, %q)", r, s)
		}

		// The owning conversation still gets its reasoning back.
		if r, s, ok := c.Get("call_1", contentA, tcA); !ok || r != "reasoning A" || s != "" {
			t.Fatalf("expected matching content to hit, got (%q, %q, %v)", r, s, ok)
		}
	})

	t.Run("same toolID and content across conversations still hits", func(t *testing.T) {
		c := New(100, time.Hour)
		sharedContent := "identical content"
		sharedTC := `[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]`
		c.Put([]string{"call_1"}, sharedContent, sharedTC, "reasoning A", "", "modelA")
		// Conversation B records the same tool call shape; Put overwrites the slot.
		c.Put([]string{"call_1"}, sharedContent, sharedTC, "reasoning B", "", "modelB")

		if r, _, ok := c.Get("call_1", sharedContent, sharedTC); !ok || r != "reasoning B" {
			t.Fatalf("expected legitimate reuse to hit with latest entry, got (%q, %v)", r, ok)
		}

		// Callers without binding info keep the plain toolID-only behavior.
		if r, _, ok := c.Get("call_1", "", ""); !ok || r != "reasoning B" {
			t.Fatalf("expected no-binding toolID hit, got (%q, %v)", r, ok)
		}
	})

	t.Run("binding mismatch falls through to the hash index", func(t *testing.T) {
		c := New(100, time.Hour)
		tcA := `[{"id":"call_1","type":"function","function":{"name":"a","arguments":"{}"}}]`
		tcB := `[{"id":"call_1","type":"function","function":{"name":"b","arguments":"{}"}}]`
		c.Put([]string{"call_1"}, "content A", tcA, "reasoning A", "", "modelA")
		c.Put([]string{"call_1"}, "content B", tcB, "reasoning B", "", "modelB")
		if c.Len() != 2 {
			t.Fatalf("expected Len() == 2, got %d", c.Len())
		}

		// byToolID["call_1"] points at conversation B; conversation A's replay
		// must fall through to the hash index and get its own reasoning back.
		if r, _, ok := c.Get("call_1", "content A", tcA); !ok || r != "reasoning A" {
			t.Fatalf("expected hash fallback to restore conversation A reasoning, got (%q, %v)", r, ok)
		}
		if r, _, ok := c.Get("call_1", "content B", tcB); !ok || r != "reasoning B" {
			t.Fatalf("expected conversation B toolID hit, got (%q, %v)", r, ok)
		}
	})

	t.Run("hash fallback path is unaffected", func(t *testing.T) {
		c := New(100, time.Hour)
		content := "fallback content"
		tc := `[{"id":"call_9","type":"function","function":{"name":"f","arguments":"{}"}}]`
		c.Put([]string{"call_9"}, content, tc, "fallback reasoning", "", "model")

		if r, _, ok := c.Get("", content, tc); !ok || r != "fallback reasoning" {
			t.Fatalf("expected hash lookup by content, got (%q, %v)", r, ok)
		}
		if r, _, ok := c.Get("call_unknown", content, tc); !ok || r != "fallback reasoning" {
			t.Fatalf("expected unknown toolID to fall through to hash, got (%q, %v)", r, ok)
		}
		if _, _, ok := c.Get("", "other content", tc); ok {
			t.Fatalf("expected mismatching hash lookup to miss")
		}
		if _, _, ok := c.Get("", "", ""); ok {
			t.Fatalf("expected empty lookup to miss")
		}
	})
}
