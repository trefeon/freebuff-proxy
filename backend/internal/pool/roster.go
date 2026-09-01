package pool

import (
	"sync"
	"sync/atomic"
	"time"

	"freebuff-proxy/backend/internal/upstream"
)

// tokenRoster owns the fixed-token entry list plus its per-token ledger
// state (usage/spend travel with each entry) and the 1-based mismatch
// escalation map, behind a single mutex (issue #262). Every mutation —
// AddToken, SetConfig slot reconciliation, RemoveLastToken, RemoveTokenAt,
// SwapTokens, RemoveAllTokens — goes through one of the methods below, and
// the per-entry ledger reads/writes (recordChat, usageCount, recordSpend)
// are guarded by the same mu, so the previous usageMu/spendMu/mismatchMu
// lock dances (and the inferred lock-ordering inconsistency where
// RemoveLastToken nested usageMu and spendMu) collapse into one lock.
//
// Readers call Load() for lock-free access to the current entry list; the
// returned pointer snapshot is never mutated in place, so a reader that
// holds it can keep reading without contending on mu. The ledger is touched
// only through these methods, which take mu.
type tokenRoster struct {
	mu       sync.Mutex
	toks     atomic.Pointer[[]*tokenEntry]
	mismatch map[int]mismatchEscalation
}

// newTokenRoster builds a roster over the initial entries (New's fixed
// tokens); the mismatch map starts empty.
func newTokenRoster(entries []*tokenEntry) *tokenRoster {
	r := &tokenRoster{mismatch: make(map[int]mismatchEscalation)}
	r.toks.Store(&entries)
	return r
}

// Load returns the current entry list pointer (nil-safe only until the first
// Store; New always stores a non-nil slice). The returned snapshot is
// immutable: mutations replace the whole slice, so the pointer stays valid
// across a concurrent reindex.
func (r *tokenRoster) Load() *[]*tokenEntry {
	return r.toks.Load()
}

// add appends a caller-built entry and returns its index. The new entry
// carries its own ledger, so no index-aligned slice needs to be extended —
// the publish-order rule (usage before token snapshot) is satisfied by
// construction.
func (r *tokenRoster) add(entry *tokenEntry) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := *r.toks.Load()
	idx := len(cur)
	next := make([]*tokenEntry, 0, idx+1)
	next = append(next, cur...)
	next = append(next, entry)
	r.toks.Store(&next)
	return idx
}

// replaceAll swaps in a fully rebuilt list (SetConfig slot reconciliation).
// Removed/replaced entries are returned in order so the caller can retire
// and drain them; the mismatch map is untouched (pooled keys are reindexed
// by the caller's per-slot rebuild decision).
func (r *tokenRoster) replaceAll(entries []*tokenEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toks.Store(&entries)
}

// removeLast pops the trailing entry, dropping its 1-based mismatch key (a
// later AddToken at the same slot must not inherit a stale escalation
// window). Returns the removed entry and false when the roster is empty.
func (r *tokenRoster) removeLast() (*tokenEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := *r.toks.Load()
	if len(cur) == 0 {
		return nil, false
	}
	last := cur[len(cur)-1]
	next := append([]*tokenEntry{}, cur[:len(cur)-1]...)
	r.toks.Store(&next)
	delete(r.mismatch, len(next)+1)
	return last, true
}

// removeAt removes the entry at idx, reindexing the 1-based mismatch keys
// above it. Returns the removed entry and false when idx is out of range.
func (r *tokenRoster) removeAt(idx int) (*tokenEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := *r.toks.Load()
	if idx < 0 || idx >= len(cur) {
		return nil, false
	}
	target := cur[idx]
	next := make([]*tokenEntry, 0, len(cur)-1)
	next = append(next, cur[:idx]...)
	next = append(next, cur[idx+1:]...)
	r.toks.Store(&next)
	for key, v := range r.mismatch {
		switch {
		case key == idx+1:
			delete(r.mismatch, key)
		case key > idx+1:
			r.mismatch[key-1] = v
			delete(r.mismatch, key)
		}
	}
	return target, true
}

// swap exchanges the entries at i and j and the corresponding 1-based
// mismatch keys. Returns false when indices are out of range or identical.
func (r *tokenRoster) swap(i, j int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := *r.toks.Load()
	if i < 0 || i >= len(cur) || j < 0 || j >= len(cur) || i == j {
		return false
	}
	next := make([]*tokenEntry, len(cur))
	copy(next, cur)
	next[i], next[j] = next[j], next[i]
	r.toks.Store(&next)
	m1, ok1 := r.mismatch[i+1]
	m2, ok2 := r.mismatch[j+1]
	if ok1 {
		r.mismatch[j+1] = m1
	} else {
		delete(r.mismatch, j+1)
	}
	if ok2 {
		r.mismatch[i+1] = m2
	} else {
		delete(r.mismatch, i+1)
	}
	return true
}

// clear empties the roster and resets the mismatch map (RemoveAllTokens).
func (r *tokenRoster) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	empty := make([]*tokenEntry, 0)
	r.toks.Store(&empty)
	r.mismatch = make(map[int]mismatchEscalation)
}

// --- per-entry ledger access (all take roster.mu) ---

// recordChat appends one successful upstream chat for the entry at token.
func (r *tokenRoster) recordChat(token int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := *r.toks.Load()
	if token < 0 || token >= len(cur) {
		return
	}
	cur[token].ledger.recordChat(time.Now())
}

// recordChatEntry appends one successful upstream chat for a lease's backing
// entry by pointer. The entry owns its ledger, so no index lookup is needed;
// a retired (removed) entry's ledger is never read again, so recording on it
// is harmless.
func (r *tokenRoster) recordChatEntry(entry *tokenEntry) {
	if entry == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry.ledger.recordChat(time.Now())
}

// usageCount returns the entry at token's in-window chat count, pruning
// expired timestamps.
func (r *tokenRoster) usageCount(token int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := *r.toks.Load()
	if token < 0 || token >= len(cur) {
		return 0
	}
	return cur[token].ledger.usageCount(time.Now())
}

// usageResetIn returns how long until the entry at token's oldest usage
// timestamp ages out of the window.
func (r *tokenRoster) usageResetIn(token int) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := *r.toks.Load()
	if token < 0 || token >= len(cur) {
		return 0
	}
	return cur[token].ledger.usageResetIn(time.Now())
}

// recordSpend adds tokens to the entry at token's spend ledger.
func (r *tokenRoster) recordSpend(token int, tokens int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := *r.toks.Load()
	if token < 0 || token >= len(cur) {
		return
	}
	cur[token].ledger.recordSpend(tokens, time.Now())
}

// recordSpendEntry adds tokens to a lease's backing entry's ledger by
// pointer (mirrors recordChatEntry).
func (r *tokenRoster) recordSpendEntry(entry *tokenEntry, tokens int64) {
	if entry == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry.ledger.recordSpend(tokens, time.Now())
}

// spendSnapshot returns the entry at token's ledger view.
func (r *tokenRoster) spendSnapshot(token int) spendView {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := *r.toks.Load()
	if token < 0 || token >= len(cur) {
		return spendView{}
	}
	return cur[token].ledger.spendSnapshot()
}

// recordSpendLimited marks one upstream spend_limited refusal on the entry
// at token's ledger.
func (r *tokenRoster) recordSpendLimited(token int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := *r.toks.Load()
	if token < 0 || token >= len(cur) {
		return
	}
	cur[token].ledger.recordSpendLimited()
}

// recordMismatch counts one free_mode_invalid_agent_model hit for tokenIndex
// (1-based pooled index, 0 = the bridge-shared window) and reports whether
// the storm threshold was crossed (crossing window), together with the
// refused model to name. The caller (Pool.recordMismatchEscalation) owns the
// webhook emission so the roster stays a pure ledger.
func (r *tokenRoster) recordMismatch(tokenIndex int, rle *upstream.RateLimitError) (bool, string) {
	if rle == nil || rle.Status != "free_mode_invalid_agent_model" {
		return false, ""
	}
	now := time.Now()
	r.mu.Lock()
	st := r.mismatch[tokenIndex]
	kept := st.hits[:0]
	for _, t := range st.hits {
		if now.Sub(t) < mismatchWindow {
			kept = append(kept, t)
		}
	}
	st.hits = append(kept, now)
	fire := false
	if len(st.hits) >= mismatchThreshold && now.Sub(st.lastStorm) >= mismatchWindow {
		st.lastStorm = now
		fire = true
	}
	r.mismatch[tokenIndex] = st
	r.mu.Unlock()

	if !fire {
		return false, ""
	}
	model := rle.Model
	if model == "" {
		model = rle.Status
	}
	return true, model
}
