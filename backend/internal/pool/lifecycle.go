package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// LeaseRelease decrements the leased run's inflight counter. Call when the
// request completes or fails. Safe on nil leases.
func (p *Pool) LeaseRelease(lease *Lease) {
	if lease == nil || lease.Run == nil {
		return
	}
	if lease.Bridge != nil {
		lease.Bridge.runs.Release(lease.Run)
		return
	}
	if lease.entry == nil {
		return // synthetic lease without a backing entry
	}
	lease.entry.runs.Release(lease.Run)
	// A lease on a removed token (RemoveLastToken swapped the snapshot out
	// from under a concurrent Acquire) releases through its own entry — the
	// bounds-checked index path would no-op and leak the run's inflight, or
	// mis-target a reused index. RemoveLastToken parked the entry undrained
	// when it observed the slip; drain it once its last lease has released.
	p.retiredMu.Lock()
	_, parked := p.retired[lease.entry]
	p.retiredMu.Unlock()
	if parked && lease.entry.runs.InflightCount() == 0 {
		p.drainRemovedToken(lease.entry)
	}
}

// LeaseAbandon releases a lease whose downstream client context was
// cancelled mid-chat (issue #53, CLI DELETE-on-exit parity): when this was
// the LAST in-flight request on the run, the run is dropped from the active
// set and FINISHed through the bounded queue so upstream does not keep an
// abandoned agent run alive until rotation. Concurrent requests on the same
// run keep it alive. The server calls this instead of LeaseRelease when it
// observes a client disconnect.
func (p *Pool) LeaseAbandon(lease *Lease) {
	if lease == nil || lease.Run == nil {
		return
	}
	if lease.Bridge != nil {
		lease.Bridge.runs.ReleaseAbandoned(lease.Run)
		return
	}
	if lease.entry != nil {
		lease.entry.runs.ReleaseAbandoned(lease.Run)
		return
	}
	toks := p.toks.Load()
	if lease.Token < 0 || lease.Token >= len(*toks) {
		return
	}
	(*toks)[lease.Token].runs.ReleaseAbandoned(lease.Run)
}

// RecordRunStep records a completed chat step on the lease's run (issue
// #114): steps are accumulated in memory and sent WITH FINISH — recording
// is local-only and never an upstream call (the CLI has no /steps
// endpoint). The server fires it after a successful chat with the response
// message id ("" when the stream never carried one).
func (p *Pool) RecordRunStep(lease *Lease, messageID string) {
	if lease == nil || lease.Run == nil {
		return
	}
	if lease.Bridge != nil {
		lease.Bridge.runs.RecordStep(lease.Run, messageID)
		return
	}
	if lease.entry != nil {
		lease.entry.runs.RecordStep(lease.Run, messageID)
		return
	}
	toks := p.toks.Load()
	if lease.Token < 0 || lease.Token >= len(*toks) {
		return
	}
	(*toks)[lease.Token].runs.RecordStep(lease.Run, messageID)
}

// MarkRunFailed marks the lease's run as failed for its eventual FINISH
// (issue #114): the server calls it when a chat dies on a terminal upstream
// error so the run does not FINISH as completed (a gateway with zero failed
// runs looks synthetic). The run stays active; only its terminal status is
// recorded. Nil-safe (an acquire failure leaves no lease).
func (p *Pool) MarkRunFailed(lease *Lease) {
	if lease == nil || lease.Run == nil {
		return
	}
	if lease.Bridge != nil {
		lease.Bridge.runs.MarkFailed(lease.Run)
		return
	}
	if lease.entry != nil {
		lease.entry.runs.MarkFailed(lease.Run)
		return
	}
	toks := p.toks.Load()
	if lease.Token < 0 || lease.Token >= len(*toks) {
		return
	}
	(*toks)[lease.Token].runs.MarkFailed(lease.Run)
}

// RecordSpend adds tokens to the lease's backing token spend ledger (issue
// #87): the server reports the usage block of a completed chat. Non-positive
// deltas are ignored. Production caller: chatCore feeds the relay's observed
// usage total once per successful chat completion (#122). The daily $15/$5/
// $0.50 ceilings are server-enforced and cohort-dependent, so this
// token-count ledger is a heuristic proxy, not exact USD accounting — see
// spend.go's package comment.
func (p *Pool) RecordSpend(lease *Lease, tokens int64) {
	if lease == nil || tokens <= 0 {
		return
	}
	if lease.Bridge != nil {
		p.bridgeRecordSpend(lease.Bridge, tokens)
		return
	}
	if lease.entry != nil {
		p.recordSpendEntry(lease.entry, tokens)
		return
	}
	toks := p.toks.Load()
	if lease.Token < 0 || lease.Token >= len(*toks) {
		return
	}
	p.recordSpend(lease.Token, tokens)
}

// InvalidateSession drops the cached free session of token so the next
// Acquire re-creates it (session-invalid recovery). The invalidation is
// guarded to the given instance id (issue #132): after a pre-emptive
// re-admit replaced the cache, a chat that rode the old superseded instance
// failing must not invalidate the fresh one. Out-of-range tokens are
// ignored.
func (p *Pool) InvalidateSession(token int, instanceID string) {
	p.InvalidateSessionWithReason(token, instanceID, "instance_invalidated", 0)
}

// InvalidateSessionWithReason drops the cached session of token only when
// its instance id matches instanceID (issue #132 guard), recording WHY.
// The superseded chat recovery path passes session.ReasonSuperseded
// (#159) so the re-admit storm detector can attribute the invalidation; the
// other session-invalid paths keep the generic instance_invalidated reason.
func (p *Pool) InvalidateSessionWithReason(token int, instanceID, reason string, status int) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return
	}
	(*toks)[token].session.InvalidateInstanceWithReason(instanceID, reason, status)
}

// InvalidateRun drops the current run of token for agentID so the next
// Acquire starts a fresh one (run-invalid recovery). Out-of-range tokens are
// ignored.
func (p *Pool) InvalidateRun(token int, agentID string) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return
	}
	(*toks)[token].runs.Invalidate(agentID)
}

// ClearQueuedCaches drops every token's cached QUEUED session (issue #100):
// the queue-time model fallback calls this before re-acquiring with the
// fallback model, so the fallback acquire creates a fresh session instead of
// re-surfacing the same waiting room. Returns how many queued caches were
// cleared. Other states (active/disabled) are untouched.
func (p *Pool) ClearQueuedCaches() int {
	toks := p.toks.Load()
	cleared := 0
	for _, tok := range *toks {
		if tok.session.ClearQueued() {
			cleared++
		}
	}
	return cleared
}

// InvalidateBridgeSession drops the cached free session of the bridge
// entry so the next AcquireBridge re-creates it (session-invalid recovery).
// Guarded to the lease's instance id (issue #132) — see InvalidateSession.
func (p *Pool) InvalidateBridgeSession(lease *Lease) {
	p.InvalidateBridgeSessionWithReason(lease, "instance_invalidated", 0)
}

// InvalidateBridgeSessionWithReason is the reason-aware form of
// InvalidateBridgeSession (see InvalidateSessionWithReason, #159).
func (p *Pool) InvalidateBridgeSessionWithReason(lease *Lease, reason string, status int) {
	if lease == nil || lease.Bridge == nil {
		return
	}
	lease.Bridge.session.InvalidateInstanceWithReason(lease.SessionInstanceID, reason, status)
}

// InvalidateBridgeRun drops the current run of the bridge entry for agentID
// so the next AcquireBridge starts a fresh one (run-invalid recovery).
func (p *Pool) InvalidateBridgeRun(lease *Lease, agentID string) {
	if lease == nil || lease.Bridge == nil {
		return
	}
	lease.Bridge.runs.Invalidate(agentID)
}

// RemoveLastToken removes the highest-index fixed token (dashboard action).
// Only the last index can be removed safely: removing a middle token would
// shift indices under in-flight leases. Refuses while the token has active
// runs. The removed token's runs are FINISHed and its admitted session
// ended (they used to leak upstream, contrast RemoveAllTokens); a lease
// that slips through the busy-check/swap race is released through the
// retired map and drained once it releases.
func (p *Pool) RemoveLastToken() error {
	toks := p.toks.Load()
	if len(*toks) == 0 {
		return errors.New("pool: no tokens to remove")
	}
	last := (*toks)[len(*toks)-1]
	if last.runs.InflightCount() > 0 {
		return errors.New("pool: token has in-flight requests; wait for them to finish")
	}
	next := append([]*tokenEntry{}, (*toks)[:len(*toks)-1]...)
	p.toks.Store(&next)
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	p.msgsPerToken = p.msgsPerToken[:len(p.msgsPerToken)-1]
	p.spendMu.Lock()
	defer p.spendMu.Unlock()
	p.spendPerToken = p.spendPerToken[:len(p.spendPerToken)-1]

	// The removed slot's 1-based mismatch key is dropped: a later AddToken
	// at the same slot must not inherit a stale escalation window.
	p.mismatchMu.Lock()
	delete(p.mismatch, len(next)+1)
	p.mismatchMu.Unlock()

	// The busy check above and the swap are TOCTOU: an Acquire that loaded
	// the pre-removal snapshot can lease the removed token in between. Park
	// the entry so that lease is still released (LeaseRelease bounds-checks
	// the new snapshot and would otherwise no-op, leaking the run's
	// inflight), then drain now when no lease slipped — finishing the
	// removed token's run and ending its admitted session. A slipped lease
	// keeps the entry parked; LeaseRelease drains it once the last lease
	// releases.
	slip := last.runs.InflightCount() > 0
	p.retiredMu.Lock()
	if p.retired == nil {
		p.retired = make(map[*tokenEntry]time.Time)
	}
	p.retired[last] = time.Now()
	p.retiredMu.Unlock()
	if !slip {
		p.drainRemovedToken(last)
	}
	return nil
}

// RemoveTokenAt removes the fixed token at idx (dashboard action on a
// specific row). A middle removal shifts every higher index, so unlike
// RemoveLastToken it refuses while ANY token has in-flight requests: a
// lease that slipped the check would otherwise re-index against a
// different entry on the chat path (documented hazard in RemoveLastToken).
// The removed entry is parked + drained exactly like RemoveLastToken; the
// usage/spend/mismatch tracks are rebuilt index-aligned.
func (p *Pool) RemoveTokenAt(idx int) error {
	toks := p.toks.Load()
	if idx < 0 || idx >= len(*toks) {
		return errors.New("pool: token index out of range")
	}
	for _, t := range *toks {
		if t.runs.InflightCount() > 0 {
			return errors.New("pool: active requests in flight; retry once they finish")
		}
	}
	target := (*toks)[idx]
	next := make([]*tokenEntry, 0, len(*toks)-1)
	next = append(next, (*toks)[:idx]...)
	next = append(next, (*toks)[idx+1:]...)
	p.toks.Store(&next)
	p.usageMu.Lock()
	p.msgsPerToken = append(p.msgsPerToken[:idx], p.msgsPerToken[idx+1:]...)
	p.usageMu.Unlock()
	p.spendMu.Lock()
	p.spendPerToken = append(p.spendPerToken[:idx], p.spendPerToken[idx+1:]...)
	p.spendMu.Unlock()
	p.mismatchMu.Lock()
	for key, v := range p.mismatch {
		switch {
		case key == idx+1:
			delete(p.mismatch, key)
		case key > idx+1:
			p.mismatch[key-1] = v
			delete(p.mismatch, key)
		}
	}
	p.mismatchMu.Unlock()
	p.retiredMu.Lock()
	if p.retired == nil {
		p.retired = make(map[*tokenEntry]time.Time)
	}
	p.retired[target] = time.Now()
	p.retiredMu.Unlock()
	p.drainRemovedToken(target)
	return nil
}

// SwapTokens swaps the token entries at index i and j in the fixed-token list.
// Active in-flight requests on any token refuse the swap to avoid race hazards.
func (p *Pool) SwapTokens(i, j int) error {
	toks := p.toks.Load()
	if toks == nil {
		return errors.New("pool: no tokens configured")
	}
	if i < 0 || i >= len(*toks) || j < 0 || j >= len(*toks) {
		return errors.New("pool: token index out of range")
	}
	if i == j {
		return nil
	}
	for _, t := range *toks {
		if t.runs.InflightCount() > 0 {
			return errors.New("pool: active requests in flight; retry once they finish")
		}
	}
	next := make([]*tokenEntry, len(*toks))
	copy(next, *toks)
	next[i], next[j] = next[j], next[i]
	p.toks.Store(&next)

	p.usageMu.Lock()
	if i < len(p.msgsPerToken) && j < len(p.msgsPerToken) {
		p.msgsPerToken[i], p.msgsPerToken[j] = p.msgsPerToken[j], p.msgsPerToken[i]
	}
	p.usageMu.Unlock()

	p.spendMu.Lock()
	if i < len(p.spendPerToken) && j < len(p.spendPerToken) {
		p.spendPerToken[i], p.spendPerToken[j] = p.spendPerToken[j], p.spendPerToken[i]
	}
	p.spendMu.Unlock()
	p.mismatchMu.Lock()
	m1, ok1 := p.mismatch[i+1]
	m2, ok2 := p.mismatch[j+1]
	if ok1 {
		p.mismatch[j+1] = m1
	} else {
		delete(p.mismatch, j+1)
	}
	if ok2 {
		p.mismatch[i+1] = m2
	} else {
		delete(p.mismatch, i+1)
	}
	p.mismatchMu.Unlock()

	return nil
}

// session (mirrors RemoveAllTokens' run finish plus the session end that
// removal previously skipped), bounded by the per-token shutdown timeout so
// a hung upstream cannot block the dashboard action. guarded by
// tokenEntry.drained sync.Once to prevent double-drain when both
// LeaseRelease and pruneRetired race on the same retired entry.
func (p *Pool) drainRemovedToken(entry *tokenEntry) {
	entry.drained.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		entry.runs.FinishAllRuns(ctx)
		if err := entry.session.EndSession(ctx); err != nil {
			p.logger.Warn("pool: removed token EndSession failed",
				"err", err, "token_label", tokenEntryLabel(entry))
		}
		p.retiredMu.Lock()
		defer p.retiredMu.Unlock()
		delete(p.retired, entry)
	})
}

// RemoveAllTokens finishes every fixed token's runs and empties the pool
// (bridge-mode switch). In-flight leases on removed tokens no-op on release
// (bounds-checked index access). Config must be updated separately.
func (p *Pool) RemoveAllTokens(ctx context.Context) {
	toks := p.toks.Load()
	for _, t := range *toks {
		t.runs.FinishAllRuns(ctx)
		if err := t.session.EndSession(ctx); err != nil {
			p.logger.Warn("pool: removed token EndSession failed",
				"err", err, "token_label", tokenEntryLabel(t))
		}
	}
	empty := make([]*tokenEntry, 0)
	p.toks.Store(&empty)
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	p.msgsPerToken = nil
	p.spendMu.Lock()
	defer p.spendMu.Unlock()
	p.spendPerToken = nil
	// Drop every pooled mismatch window: the pool is empty, so stale keys
	// would only survive as debris (bridge entries use the shared key 0).
	p.mismatchMu.Lock()
	p.mismatch = make(map[int]mismatchEscalation)
	p.mismatchMu.Unlock()
}

// FinishTokenRuns finishes all active runs of token (dashboard action).
func (p *Pool) FinishTokenRuns(ctx context.Context, token int) error {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	(*toks)[token].runs.FinishAllRuns(ctx)
	return nil
}

// DropTokenSession forcibly ends the active session and finishes all runs for token (dashboard action).
// Forcibly ends the active session so the operator can change model immediately;
// the next request re-admits fresh.
func (p *Pool) DropTokenSession(ctx context.Context, token int) error {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	entry := (*toks)[token]
	snap := entry.session.Snapshot()
	p.logger.Info("pool: dropping session", "token", token, "model", snap.Model, "instance", snap.InstanceID)
	entry.runs.FinishAllRuns(ctx)
	if err := entry.session.EndSession(ctx); err != nil {
		p.logger.Warn("pool: drop session EndSession failed", "token", token, "err", err)
		return err
	}
	p.logger.Info("pool: session dropped", "token", token, "model", snap.Model)
	return nil
}

// Shutdown stops the background jobs and drains every token: FINISH all
// runs, end the sessions, bounded by a 10s force deadline per token. Cached
// bridge entries (bridge mode) are drained best-effort the same way after
// the fixed tokens: FINISH all runs and end each entry's session so no
// upstream activity is left behind.
func (p *Pool) Shutdown(ctx context.Context) {
	// Set BEFORE the drain: an in-flight request still in its acquire phase
	// must not admit a session/run after the drain released the upstream
	// sessions (post-drain re-admission gate; Acquire/AcquireBridge check).
	p.draining.Store(true)
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()

	var errs []string
	toks := p.toks.Load()
	for i, tok := range *toks {
		tokCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		tok.runs.Shutdown(tokCtx)
		cancel()
		// With run persistence the runs are intentionally kept alive for
		// restart-resume — not a drain failure.
		if !tok.runs.KeptForPersistence() {
			if snap := tok.runs.Snapshot(); snap.ActiveRuns > 0 {
				errs = append(errs, fmt.Sprintf("token-%d: %d runs left after shutdown", i+1, snap.ActiveRuns))
			}
		}
	}

	// Drain the cached bridge entries best-effort. The maintain loop is
	// already stopped (wg.Wait above), so the entry list is stable.
	//
	// Snapshot the entries under the lock, then drain each one AFTER
	// releasing bridgeMu: FinishAllRuns + session shutdown are sequential
	// upstream calls bounded by the session-call timeout, so holding
	// bridgeMu across them would stall every other bridge operation
	// (AcquireBridge, bridgeRecordChat/bridgeRecordSpend/BridgeCount) for
	// the whole drain — the same rule bridgeEvictLocked and bridgeMaintain
	// already follow (bridge_cache.go:128-134).
	p.bridgeMu.Lock()
	entries := make([]*bridgeEntry, 0, len(p.bridge))
	for _, entry := range p.bridge {
		entries = append(entries, entry)
	}
	p.bridgeMu.Unlock()

	for _, entry := range entries {
		entryCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		entry.runs.FinishAllRuns(entryCtx)
		if snap := entry.runs.Snapshot(); snap.ActiveRuns > 0 {
			errs = append(errs, fmt.Sprintf("bridge %s: %d runs left after shutdown", bridgeTokenLabel(entry), snap.ActiveRuns))
		}
		if err := entry.session.Shutdown(entryCtx); err != nil {
			errs = append(errs, fmt.Sprintf("bridge %s: shutdown session: %v", bridgeTokenLabel(entry), err))
		}
		cancel()
	}

	if len(errs) > 0 {
		slog.Warn("pool: shutdown incomplete", "errors", strings.Join(errs, "; "))
	}
}

// pruneRetired drops retired tokens that hold no leases and have been
// parked past the drain grace (their runs were already finished at removal
// or on the last release). Entries still carrying a lease stay until
// LeaseRelease drains them.
//
// NOTE: pruneRetired only deletes entries with InflightCount==0 (no active
// leases) and after the drain grace period. The drained sync.Once in
// drainRemovedToken guards against double-drain when LeaseRelease and
// pruneRetired race on the same retired entry.
func (p *Pool) pruneRetired() {
	p.retiredMu.Lock()
	defer p.retiredMu.Unlock()
	for entry, swappedAt := range p.retired {
		if entry.runs.InflightCount() == 0 && time.Since(swappedAt) > retiredDrainGrace {
			delete(p.retired, entry)
		}
	}
}
