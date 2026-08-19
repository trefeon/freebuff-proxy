package reasoningcache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestBasicPutAndGet(t *testing.T) {
	c := New(100, time.Hour)

	toolIDs := []string{"call_1", "call_2"}
	content := "Let me check the weather."
	toolCallsJSON := `[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]`
	reasoning := "Thinking about San Francisco weather..."
	signature := "sig_abc123"
	model := "deepseek-r1"

	c.Put(toolIDs, content, toolCallsJSON, reasoning, signature, model)

	if c.Len() != 1 {
		t.Fatalf("expected Len() == 1, got %d", c.Len())
	}

	// Lookup by tool ID 1
	r, s, ok := c.GetByToolID("call_1")
	if !ok {
		t.Fatalf("expected GetByToolID(call_1) to be ok")
	}
	if r != reasoning || s != signature {
		t.Fatalf("expected (%q, %q), got (%q, %q)", reasoning, signature, r, s)
	}

	// Lookup by tool ID 2
	r, s, ok = c.GetByToolID("call_2")
	if !ok {
		t.Fatalf("expected GetByToolID(call_2) to be ok")
	}
	if r != reasoning || s != signature {
		t.Fatalf("expected (%q, %q), got (%q, %q)", reasoning, signature, r, s)
	}

	// Lookup by hash
	r, s, ok = c.GetByHash(content, toolCallsJSON)
	if !ok {
		t.Fatalf("expected GetByHash to be ok")
	}
	if r != reasoning || s != signature {
		t.Fatalf("expected (%q, %q), got (%q, %q)", reasoning, signature, r, s)
	}

	// Lookup by unified Get (toolID match)
	r, s, ok = c.Get("call_1", "", "")
	if !ok || r != reasoning || s != signature {
		t.Fatalf("expected Get(call_1) to be ok, got (%q, %q, %v)", r, s, ok)
	}

	// Lookup by unified Get (fallback to hash match when toolID empty)
	r, s, ok = c.Get("", content, toolCallsJSON)
	if !ok || r != reasoning || s != signature {
		t.Fatalf("expected Get(hash) to be ok, got (%q, %q, %v)", r, s, ok)
	}

	// Lookup by unified Get with unknown toolID falling back to valid hash
	r, s, ok = c.Get("call_unknown", content, toolCallsJSON)
	if !ok || r != reasoning || s != signature {
		t.Fatalf("expected Get(unknownID, hash) to fall back and succeed, got (%q, %q, %v)", r, s, ok)
	}

	// GetEntryByToolID
	entry, ok := c.GetEntryByToolID("call_1")
	if !ok || entry == nil {
		t.Fatalf("expected GetEntryByToolID to return entry")
	}
	if entry.Model != model || entry.ReasoningContent != reasoning || entry.Signature != signature {
		t.Fatalf("unexpected entry: %+v", entry)
	}

	// Non-existent lookups
	if _, _, ok := c.GetByToolID("call_unknown"); ok {
		t.Fatalf("expected unknown tool ID to return ok=false")
	}
	if _, _, ok := c.GetByHash("different content", toolCallsJSON); ok {
		t.Fatalf("expected mismatching hash to return ok=false")
	}
}

func TestEmptyReasoningAndSignature(t *testing.T) {
	c := New(100, time.Hour)

	// When reasoning == "" && signature == "", Put does nothing.
	c.Put([]string{"call_empty"}, "content", "toolCalls", "", "", "model")
	if c.Len() != 0 {
		t.Fatalf("expected cache to be empty, got %d entries", c.Len())
	}
	if _, _, ok := c.GetByToolID("call_empty"); ok {
		t.Fatalf("expected call_empty to not be found")
	}

	// If reasoning != "" but signature == "", entry is saved.
	c.Put([]string{"call_reasoning_only"}, "c", "t", "reasoning text", "", "model")
	if c.Len() != 1 {
		t.Fatalf("expected Len() == 1, got %d", c.Len())
	}
	r, s, ok := c.GetByToolID("call_reasoning_only")
	if !ok || r != "reasoning text" || s != "" {
		t.Fatalf("unexpected result: (%q, %q, %v)", r, s, ok)
	}

	// If reasoning == "" but signature != "", entry is saved.
	c.Put([]string{"call_sig_only"}, "c2", "t2", "", "sig_only", "model")
	if c.Len() != 2 {
		t.Fatalf("expected Len() == 2, got %d", c.Len())
	}
	r, s, ok = c.GetByToolID("call_sig_only")
	if !ok || r != "" || s != "sig_only" {
		t.Fatalf("unexpected result: (%q, %q, %v)", r, s, ok)
	}
}

func TestEmptyToolIDsAndEdgeCases(t *testing.T) {
	c := New(100, time.Hour)

	// Put with nil/empty tool IDs, but with content
	c.Put(nil, "some content", "some tools", "reasoning", "sig", "model")
	if c.Len() != 1 {
		t.Fatalf("expected Len() == 1, got %d", c.Len())
	}
	r, s, ok := c.GetByHash("some content", "some tools")
	if !ok || r != "reasoning" || s != "sig" {
		t.Fatalf("unexpected result: (%q, %q, %v)", r, s, ok)
	}

	// Empty and whitespace IDs should be ignored
	c.Put([]string{"", "  ", "call_valid", ""}, "c", "t", "r2", "s2", "m")
	if _, _, ok := c.GetByToolID(""); ok {
		t.Fatalf("GetByToolID(\"\") should return false")
	}
	if _, _, ok := c.GetByToolID("   "); ok {
		t.Fatalf("GetByToolID(\"   \") should return false")
	}
	r, s, ok = c.GetByToolID("call_valid")
	if !ok || r != "r2" || s != "s2" {
		t.Fatalf("expected call_valid to be found")
	}

	// GetByHash with empty content and toolCallsJSON should return false
	if _, _, ok := c.GetByHash("", ""); ok {
		t.Fatalf("GetByHash(\"\", \"\") should return false")
	}
}

func TestTTLExpiration(t *testing.T) {
	ttl := 20 * time.Millisecond
	c := New(100, ttl)

	c.Put([]string{"call_ttl"}, "content_ttl", "tools_ttl", "reasoning", "sig", "model")

	// Immediate lookup succeeds
	if _, _, ok := c.GetByToolID("call_ttl"); !ok {
		t.Fatalf("expected immediate lookup to succeed")
	}
	if _, _, ok := c.GetByHash("content_ttl", "tools_ttl"); !ok {
		t.Fatalf("expected immediate hash lookup to succeed")
	}

	// Wait for TTL to pass
	time.Sleep(35 * time.Millisecond)

	// Lookup by tool ID triggers expiration eviction
	if _, _, ok := c.GetByToolID("call_ttl"); ok {
		t.Fatalf("expected expired tool ID lookup to return ok=false")
	}

	// Hash lookup should also be gone
	if _, _, ok := c.GetByHash("content_ttl", "tools_ttl"); ok {
		t.Fatalf("expected expired hash lookup to return ok=false")
	}

	if c.Len() != 0 {
		t.Fatalf("expected Len() == 0 after expiration, got %d", c.Len())
	}
}

func TestPrune(t *testing.T) {
	ttl := 20 * time.Millisecond
	c := New(100, ttl)

	c.Put([]string{"c1"}, "cnt1", "t1", "r1", "s1", "m")
	c.Put([]string{"c2"}, "cnt2", "t2", "r2", "s2", "m")

	time.Sleep(35 * time.Millisecond)

	// Add a fresh entry
	c.Put([]string{"c3"}, "cnt3", "t3", "r3", "s3", "m")

	if c.Len() < 1 {
		t.Fatalf("expected at least 1 entry")
	}

	c.Prune()

	if c.Len() != 1 {
		t.Fatalf("expected Len() == 1 after Prune(), got %d", c.Len())
	}

	if _, _, ok := c.GetByToolID("c1"); ok {
		t.Fatalf("expected c1 to be pruned")
	}
	if _, _, ok := c.GetByToolID("c2"); ok {
		t.Fatalf("expected c2 to be pruned")
	}
	if _, _, ok := c.GetByToolID("c3"); !ok {
		t.Fatalf("expected c3 to remain")
	}
}

func TestMaxEntriesEviction(t *testing.T) {
	maxEntries := 3
	c := New(maxEntries, time.Hour)

	c.Put([]string{"c1"}, "cnt1", "t1", "r1", "s1", "m")
	c.Put([]string{"c2"}, "cnt2", "t2", "r2", "s2", "m")
	c.Put([]string{"c3"}, "cnt3", "t3", "r3", "s3", "m")

	if c.Len() != 3 {
		t.Fatalf("expected Len() == 3, got %d", c.Len())
	}

	// Adding a 4th entry evicts the oldest (c1)
	c.Put([]string{"c4"}, "cnt4", "t4", "r4", "s4", "m")

	if c.Len() != 3 {
		t.Fatalf("expected Len() == 3 after eviction, got %d", c.Len())
	}

	if _, _, ok := c.GetByToolID("c1"); ok {
		t.Fatalf("expected c1 to be evicted")
	}
	if _, _, ok := c.GetByHash("cnt1", "t1"); ok {
		t.Fatalf("expected cnt1/t1 to be evicted")
	}

	for _, id := range []string{"c2", "c3", "c4"} {
		if _, _, ok := c.GetByToolID(id); !ok {
			t.Fatalf("expected %s to still be in cache", id)
		}
	}
}

func TestOverwriteKeys(t *testing.T) {
	c := New(2, time.Hour)

	// Entry 1 has shared key "call_shared"
	c.Put([]string{"call_shared", "call_other1"}, "cnt1", "", "r1", "s1", "m")

	// Entry 2 overwrites "call_shared"
	c.Put([]string{"call_shared", "call_other2"}, "cnt2", "", "r2", "s2", "m")

	r, s, ok := c.GetByToolID("call_shared")
	if !ok || r != "r2" || s != "s2" {
		t.Fatalf("expected call_shared to return Entry 2, got (%q, %q, %v)", r, s, ok)
	}

	// Adding Entry 3 evicts Entry 1 (oldest)
	c.Put([]string{"call_other3"}, "cnt3", "", "r3", "s3", "m")

	// Entry 1's eviction should NOT delete "call_shared", because "call_shared" now points to Entry 2
	r, s, ok = c.GetByToolID("call_shared")
	if !ok || r != "r2" || s != "s2" {
		t.Fatalf("expected call_shared to still point to Entry 2 after Entry 1 eviction, got (%q, %q, %v)", r, s, ok)
	}
}

func TestConcurrentPutAndGet(t *testing.T) {
	c := New(50, 100*time.Millisecond)

	var wg sync.WaitGroup
	workers := 16
	iterations := 200

	for w := range workers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := range iterations {
				callID := fmt.Sprintf("call_%d_%d", workerID, i%10)
				content := fmt.Sprintf("content_%d_%d", workerID, i%10)
				tools := fmt.Sprintf("tools_%d_%d", workerID, i%10)
				reasoning := fmt.Sprintf("reasoning_%d", i)
				sig := fmt.Sprintf("sig_%d", i)

				c.Put([]string{callID}, content, tools, reasoning, sig, "model")

				_, _, _ = c.GetByToolID(callID)
				_, _, _ = c.GetByHash(content, tools)
				_, _ = c.GetEntryByToolID(callID)
				_, _ = c.GetEntryByHash(content, tools)
				_ = c.Len()

				if i%20 == 0 {
					c.Prune()
				}
			}
		}(w)
	}

	wg.Wait()
}

func TestDefaultConstants(t *testing.T) {
	c := New(0, 0)
	if c.maxEntries != DefaultMaxEntries {
		t.Fatalf("expected maxEntries == %d, got %d", DefaultMaxEntries, c.maxEntries)
	}
	if c.ttl != DefaultTTL {
		t.Fatalf("expected ttl == %v, got %v", DefaultTTL, c.ttl)
	}
}
