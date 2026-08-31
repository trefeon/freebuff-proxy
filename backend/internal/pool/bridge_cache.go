// bridge_cache.go — bridge-mode LRU cache: lazily-created per-client-token
// entries (upstream client + session manager + run manager), LRU eviction
// when the cache is full, and the maintain/evict loops (bridgeMaintain,
// bridgeSessionPollTick, bridgeEvictToken).
package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/runs"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
)

// maxBridgeEntries caps the in-memory bridge cache: one entry (upstream
// client + session manager + run manager) per distinct client token. LRU
// eviction makes room when the cap is exceeded.
const maxBridgeEntries = 32

// defaultBridgeIdleEvict is the sliding-TTL default for idle bridge-entry
// eviction when BRIDGE_IDLE_EVICT is unset (72h). An idle client's upstream
// session row expires on its own, so a longer TTL mostly saves re-create
// churn — fewer admission requests, fewer fingerprints — while dead tokens
// are still evicted immediately by B6.
const defaultBridgeIdleEvict = 72 * time.Hour

// maxBridgeSurvivors bounds the survivor list: evictions are rare relative
// to the maintain passes that prune it, and each survivor is a few bytes.
const maxBridgeSurvivors = 256

// bridgeSurvivor is one evicted bridge entry's 24h-window chat count,
// carried until it ages out of the BRIDGE_DAILY_LIMIT window (review
// 2026-08-31 P3): a recompute that only sums live entries would let an
// eviction reset an active client's contribution to the global cap.
type bridgeSurvivor struct {
	count   int
	evicted time.Time // eviction time; the survivor expires one window later
}

// tokenKey returns a 32-char hex string derived from the SHA-256 hash of the
// raw client token. Bridge map keys use this non-reversible form so raw tokens
// are never stored as map keys in memory. The raw token is still held in
// bridgeEntry.token for upstream client creation.
func tokenKey(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:16])[:32]
}

// BridgeCount returns the number of cached bridge entries (healthz).
func (p *Pool) BridgeCount() int {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return len(p.bridge)
}

// bridgeEntryFor returns the cached bridge entry for clientToken, creating
// it on first use (upstream client + session manager + run manager) and
// recording the use for LRU order. A token that cannot build an upstream
// client yields an error and is never cached.
//
// The upstream client is created OUTSIDE bridgeMu to avoid blocking
// other bridge operations during the network-heavy New call. A buffered
// creation-rate gate (bridgeCreateGate, capacity 4) limits concurrent
// client creations to prevent thundering-herd creation. A double-check
// after acquiring the gate ensures a concurrent creator did not already
// populate the cache.
//
// Map keys use tokenKey (SHA-256 truncated to 32 hex chars) so raw
// client tokens are never stored as map keys in memory.
func (p *Pool) bridgeEntryFor(clientToken string) (*bridgeEntry, error) {
	key := tokenKey(clientToken)

	// Fast path: entry already cached.
	p.bridgeMu.Lock()
	if entry, ok := p.bridge[key]; ok {
		entry.lastUsed = time.Now()
		p.bridgeTouch(key)
		p.bridgeMu.Unlock()
		return entry, nil
	}
	p.bridgeMu.Unlock()

	// Slow path: create client OUTSIDE bridgeMu. upstream.New may
	// involve DNS + TLS handshake; holding bridgeMu here would block every
	// other bridge operation for the full creation duration.
	//
	// Creation-rate gate slot: cap concurrent New calls.

	// Use a stoppable timer: time.After would leave a 5s timer pending on
	// the acquired path (per creation under gate pressure).
	timer := time.NewTimer(5 * time.Second)
	select {
	case p.bridgeCreateGate <- struct{}{}:
		if !timer.Stop() {
			<-timer.C
		}
	case <-timer.C:
		return nil, fmt.Errorf("bridge: creation rate limit exceeded (too many concurrent client builds)")
	}
	defer func() { <-p.bridgeCreateGate }()

	// Double-check: another goroutine may have created the entry while we
	// were waiting for the gate. Release bridgeMu before calling upstream.New
	// (DNS + TLS handshake can be slow; holding bridgeMu blocks every other
	// bridge operation).
	p.bridgeMu.Lock()
	if entry, ok := p.bridge[key]; ok {
		entry.lastUsed = time.Now()
		p.bridgeTouch(key)
		p.bridgeMu.Unlock()
		return entry, nil
	}
	p.bridgeMu.Unlock()

	// Create the client outside bridgeMu: upstream.New may involve DNS + TLS
	// handshakes; holding bridgeMu would block every other bridge operation
	// for the full creation duration.
	//
	// The token probe (zero-cost GET, no session claimed — catches invalid
	// or revoked tokens before an entry is cached) also runs OUTSIDE
	// bridgeMu: it is an upstream call bounded by SessionCallTimeout, and
	// one slow or hung probe must never stall every other bridge operation.
	// The cache is re-checked after the probe so a concurrent creator that
	// won the race is reused instead of producing two entries for the same
	// token.
	client, err := upstream.New(clientToken, p.cfg.Load())
	if err != nil {
		return nil, fmt.Errorf("bridge: %w", err)
	}

	probeCfg := p.cfg.Load()
	probeCtx, probeCancel := context.WithTimeout(context.Background(), probeCfg.SessionCallTimeout)
	_, probeErr := client.ProbeAccount(probeCtx)
	probeCancel()
	if probeErr != nil {
		return nil, fmt.Errorf("bridge: token validation failed: %w", probeErr)
	}

	// Re-check after the probe (a concurrent creator may have populated
	// the cache while this probe was in flight), then insert.
	p.bridgeMu.Lock()
	if entry, ok := p.bridge[key]; ok {
		p.bridgeMu.Unlock()
		return entry, nil
	}

	entry := &bridgeEntry{token: clientToken, client: client, spend: newSpendLedger(), admissionGate: make(chan struct{})}
	cfg := p.cfg.Load()
	entry.session = session.NewManagerWithStore(client, p.store)
	entry.session.SetReAdmitLead(cfg.SessionReAdmitLead)
	entry.session.SetAdmissionProbeTTL(cfg.SessionProbeCacheTTL)
	entry.session.SetModelUnavailableCacheTTL(cfg.ModelUnavailableCacheTTL)
	entry.runs = runs.NewRunManagerOpts(client, entry.session, runOptions(cfg))
	entry.lastUsed = time.Now()

	p.bridge[key] = entry
	p.bridgeOrder = append(p.bridgeOrder, key)
	// Drop the LRU victims under the lock, then FINISH their runs and end
	// their sessions after releasing it: the calls are sequential upstream
	// calls bounded by the session-call timeout, so running them under
	// bridgeMu would stall every other bridge operation (AcquireBridge,
	// bridgeRecordChat, BridgeCount, bridgeMaintain) for the full eviction
	// duration.
	victims := p.bridgeEvictLocked(entry)
	p.bridgeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, victim := range victims {
		victim.runs.FinishAllRuns(ctx)
		// Mirror the idle-eviction and dead-token paths: the victim may
		// hold an admitted upstream session — end it so its row does not
		// linger while nothing owns the entry (a later request from the
		// same client would otherwise admit a second concurrent session).
		if endErr := victim.session.EndSession(ctx); endErr != nil {
			p.logger.Warn("pool: bridge EndSession failed during cache-full eviction",
				"err", endErr, "token_label", bridgeTokenLabel(victim))
		}
	}
	return entry, nil
}

// bridgeTouch moves clientToken to the newest end of the LRU order.
func (p *Pool) bridgeTouch(clientToken string) {
	for i, tok := range p.bridgeOrder {
		if tok == clientToken {
			if i < len(p.bridgeOrder)-1 {
				copy(p.bridgeOrder[i:], p.bridgeOrder[i+1:])
				p.bridgeOrder[len(p.bridgeOrder)-1] = clientToken
			}
			return
		}
	}
	p.bridgeOrder = append(p.bridgeOrder, clientToken)
}

// bridgeRecordSurvivorLocked folds an evicted entry's in-window usage into
// the survivor list so its contribution to the global BRIDGE_DAILY_LIMIT
// survives the eviction (review 2026-08-31 P3). Timestamps already outside
// the window are not carried — the prune at capture mirrors the recompute.
// Caller holds bridgeMu.
func (p *Pool) bridgeRecordSurvivorLocked(entry *bridgeEntry, now time.Time) {
	if entry == nil {
		return
	}
	count := 0
	cutoff := now.Add(-usageWindow)
	for _, at := range entry.usage {
		if !at.Before(cutoff) {
			count++
		}
	}
	if count == 0 {
		return
	}
	if len(p.bridgeSurvivors) >= maxBridgeSurvivors {
		// Drop the oldest survivors to stay bounded.
		p.bridgeSurvivors = p.bridgeSurvivors[len(p.bridgeSurvivors)-maxBridgeSurvivors+1:]
	}
	p.bridgeSurvivors = append(p.bridgeSurvivors, bridgeSurvivor{count: count, evicted: now})
}

// bridgeEvictLocked evicts the oldest bridge entries while the cache is
// over maxBridgeEntries (LRU): the victims are removed from the cache and
// LRU order and returned so the caller can FINISH their runs best-effort
// (bounded by the client's session-call timeout) AFTER releasing bridgeMu —
// the upstream FINISH calls must not run under the lock, or a full cache
// would stall every other bridge operation for the whole eviction. keep is
// the entry that was just created by the caller; it is excluded from the
// victim scan (like busy entries) because bridgeEntryFor hands it back for
// immediate use — evicting it here would leave its run and admitted session
// outside the cache, where neither bridgeMaintain nor Pool.Shutdown would
// ever sweep them. Caller holds bridgeMu.
func (p *Pool) bridgeEvictLocked(keep *bridgeEntry) []*bridgeEntry {
	var victims []*bridgeEntry
	for len(p.bridgeOrder) > maxBridgeEntries {
		// Scan from the LRU end for an entry WITHOUT outstanding leases:
		// FINISHing the run of an entry that still serves a request would
		// kill the in-flight chat. Busy entries are left in the cache for
		// the idle sweep (bridgeMaintain) once their leases drain; when
		// every entry is busy, nothing is evicted this pass.
		evicted := false
		for i := 0; i < len(p.bridgeOrder); {
			oldest := p.bridgeOrder[i]
			entry, ok := p.bridge[oldest]
			if !ok {
				p.bridgeOrder = removeBridgeOrder(p.bridgeOrder, oldest)
				continue
			}
			if entry == keep || entry.runs.InflightCount() > 0 {
				i++
				continue
			}
			victims = append(victims, entry)
			p.bridgeRecordSurvivorLocked(entry, time.Now())
			delete(p.bridge, oldest)
			p.bridgeOrder = removeBridgeOrder(p.bridgeOrder, oldest)
			p.logger.Debug("pool: bridge entry evicted (cache full)", "bridge_entries", len(p.bridge))
			evicted = true
			break
		}
		if !evicted {
			break
		}
	}
	return victims
}

// bridgeLen returns the number of cached bridge entries (test accessor).
func (p *Pool) bridgeLen() int {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return len(p.bridge)
}

// bridgeToken returns the cached entry for clientToken (test accessor).
// Accepts the raw client token and hashes it for map lookup.
func (p *Pool) bridgeToken(clientToken string) *bridgeEntry {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return p.bridge[tokenKey(clientToken)]
}

// bridgeTokenLabel returns a short non-reversible label for a bridge
// entry's client token, safe for logs: the sha256 of the token, hex,
// truncated to 8 chars. The raw client token must never reach logs (logring
// retains them for /admin/logs), so shutdown and diagnostics use the label,
// not the token.
func bridgeTokenLabel(entry *bridgeEntry) string {
	if entry == nil || entry.client == nil {
		return "bridge"
	}
	return "token-" + entry.client.TokenKey()[:8]
}

// BridgeSnapshot returns a snapshot of all active bridge entries for the
// dashboard. The snapshot is taken under bridgeMu but returned for external
// reads (#187).
func (p *Pool) BridgeSnapshot() []BridgeTokenSnapshot {
	p.bridgeMu.Lock()
	type keyEntry struct {
		key      string
		entry    *bridgeEntry
		lastUsed time.Time
		spend    spendView
	}
	entries := make([]keyEntry, 0, len(p.bridge))
	for k, e := range p.bridge {
		// Copy lastUsed AND the spend view while the lock is held:
		// bridgeEntryFor mutates lastUsed and bridgeRecordSpend mutates the
		// spend ledger under bridgeMu, so unlocked reads here would race
		// (torn values under -race). ledgerView rolls the ledger window —
		// a write — so it must run under bridgeMu like the recorders do.
		entries = append(entries, keyEntry{key: k, entry: e, lastUsed: e.lastUsed, spend: ledgerView(e.spend)})
	}
	p.bridgeMu.Unlock()

	snaps := make([]BridgeTokenSnapshot, 0, len(entries))
	for _, ke := range entries {
		e := ke.entry
		eRuns := e.runs.Snapshot()
		var cooldownUntil time.Time
		if eRuns.CooldownUntil.After(time.Now()) {
			cooldownUntil = eRuns.CooldownUntil
		}
		// Build quota map from session snapshot.
		var quotaByModel map[string]session.QuotaSnapshot
		if sess := e.session.Snapshot(); sess.QuotaByModel != nil {
			quotaByModel = make(map[string]session.QuotaSnapshot)
			for model, rl := range sess.QuotaByModel {
				quotaByModel[model] = session.QuotaSnapshot{
					Model:       rl.Model,
					Limit:       rl.Limit,
					RecentCount: rl.RecentCount,
					Period:      rl.Period,
					ResetAt:     rl.ResetAt,
					Pool:        rl.Pool,
					PoolLabel:   rl.PoolLabel,
					Entitlement: rl.Entitlement,
				}
			}
		}
		sess := e.session.Snapshot()
		var model string
		if sess.Model != "" {
			model = sess.Model
		}
		spend := ke.spend
		spendLimit := p.cfg.Load().MaxSpendPerDay
		spendPct := 0
		if spendLimit > 0 {
			spendPct = int((spend.Day * 100) / spendLimit)
			if spendPct > 100 {
				spendPct = 100
			}
		}
		banType, bannedUntil := banView(eRuns.BanError, eRuns.BannedUntil)
		premium := premiumSnapshotFromQuotaMap(quotaByModel)
		snaps = append(snaps, BridgeTokenSnapshot{
			Key:           ke.key,
			LastUsed:      ke.lastUsed,
			ActiveRuns:    eRuns.ActiveRuns,
			Requests:      eRuns.Requests,
			Locked:        e.locked.Load(),
			CooldownUntil: cooldownUntil,
			SessionActive: sess.Status == "active",
			AccessTier:    sess.AccessTier,
			Model:         model,
			QuotaByModel:  quotaByModel,
			SpendDay:      float64(spend.Day),
			SpendPct:      spendPct,
			PremiumQuota:  premium,
			BanType:       banType,
			BannedUntil:   bannedUntil,
		})
	}
	return snaps
}

// bridgeSessionPollTick polls the bridge cache's active sessions on the same
// jittered schedule as the fixed tokens. The sweep/eviction half
// stays in bridgeMaintain; only the per-entry session poll runs here so its
// timing is not quantized onto the 60s rotation grid.
func (p *Pool) bridgeSessionPollTick(ctx context.Context, cfg *config.Config) {
	p.bridgeMu.Lock()
	entries := make([]*bridgeEntry, 0, len(p.bridge))
	for _, entry := range p.bridge {
		entries = append(entries, entry)
	}
	p.bridgeMu.Unlock()

	for _, entry := range entries {
		if time.Now().Before(entry.runs.CooldownUntil()) || entry.runs.BanError() != nil {
			// Cooldown or live ban: no session poll (same rule as the
			// fixed-token loop) — a ban must not keep re-contacting
			// upstream at the poll cadence.
			continue
		}
		if entry.runs.InflightCount() > 0 {
			continue
		}
		now := time.Now()
		if !entry.nextPollAt.IsZero() && now.Before(entry.nextPollAt) {
			continue
		}
		mCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		err := entry.session.Poll(mCtx)
		cancel()
		var delay time.Duration
		if err != nil {
			entry.pollFailures++
			delay = sessionPollBackoffDelay(entry.pollFailures, sessionPollRetryAfter(err))
			p.logger.Debug("pool: bridge session poll failed", "err", err, "retry_in", delay)
		} else {
			entry.pollFailures = 0
			snap := entry.session.Snapshot()
			delay = sessionPollSuccessDelay(snap)
		}
		entry.nextPollAt = time.Now().Add(delay)
	}
}

// bridgeMaintain sweeps the bridge cache: entries idle past the idle-eviction TTL
// are dropped (runs FINISHed and the upstream session ended, best-effort);
// entries with in-flight leases are NEVER evicted — FINISHing their runs
// would kill the in-flight chat, so busy entries always get the per-token
// maintain work below and are only swept once their leases drain and they
// stay idle. The remaining entries get the per-token maintain work — rotate
// aged runs and advance queued sessions, bounded by the same RequestTimeout
// ctx as the fixed-token loop. On idle passes (idle=true) only the sweep
// runs: the per-entry queued-advance pauses with the fixed tokens, and the
// idle-sweep keeps bridge entries from staying admitted upstream past
// the idle-eviction TTL while the pool stays idle. Active-session liveness polls
// are NOT part of this pass — they run on the jittered
// bridgeSessionPollTick schedule.
func (p *Pool) bridgeMaintain(ctx context.Context, idle bool) {
	cfg := p.cfg.Load()
	var toEvict []*bridgeEntry
	var toMaintain []*bridgeEntry
	idleEvict := defaultBridgeIdleEvict
	if cfg.BridgeIdleEvict > 0 {
		idleEvict = cfg.BridgeIdleEvict
	}

	p.bridgeMu.Lock()
	now := time.Now()
	for token, entry := range p.bridge {
		// Busy entry: leave it for the maintain pass (same rule as
		// bridgeEvictLocked's busy skip — the idle sweep only handles
		// entries once their leases drain).
		if entry.runs.InflightCount() > 0 {
			toMaintain = append(toMaintain, entry)
			continue
		}
		if now.Sub(entry.lastUsed) > idleEvict {
			toEvict = append(toEvict, entry)
			p.bridgeRecordSurvivorLocked(entry, now)
			delete(p.bridge, token)
			p.bridgeOrder = removeBridgeOrder(p.bridgeOrder, token)
			p.logger.Debug("pool: bridge entry evicted (idle)", "bridge_entries", len(p.bridge))
		} else {
			toMaintain = append(toMaintain, entry)
		}
	}

	// Recompute bridgeDailyUsage from live entries: each entry's usage
	// slice is pruned to the 24h window, so summing their lengths gives
	// the correct rolling total (mirrors bridgeUsageCount per-entry).
	// Evicted entries' in-window usage is folded back in from the bounded
	// survivor list (review 2026-08-31 P3): without it, evicting an active
	// client's entry would reset its contribution to the global cap.
	total := 0
	for _, entry := range p.bridge {
		cutoff := now.Add(-usageWindow)
		history := entry.usage
		first := 0
		for first < len(history) && history[first].Before(cutoff) {
			first++
		}
		entry.usage = history[first:]
		total += len(entry.usage)
	}
	// Fold in unexpired survivors and prune the expired ones in the same
	// pass (each survivor ages out one usage window after its eviction).
	kept := p.bridgeSurvivors[:0]
	for _, s := range p.bridgeSurvivors {
		if now.Sub(s.evicted) < usageWindow {
			kept = append(kept, s)
			total += s.count
		}
	}
	p.bridgeSurvivors = kept
	p.bridgeDailyUsage = total

	p.bridgeMu.Unlock()

	for _, entry := range toEvict {
		// Mirror the shutdown drain: FINISH the runs AND end the entry's
		// upstream session, so a dropped idle entry does not leak its
		// session upstream. Bounded by the same RequestTimeout ctx as the
		// per-token maintain work so a hung upstream cannot stall the loop.
		eCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		entry.runs.FinishAllRuns(eCtx)
		if endErr := entry.session.EndSession(eCtx); endErr != nil {
			// Log EndSession errors at WARN so failed session teardowns
			// are visible in diagnostics instead of silently swallowed.
			p.logger.Warn("pool: bridge EndSession failed during idle eviction",
				"err", endErr, "token_label", bridgeTokenLabel(entry))
		}
		cancel()
	}
	for _, entry := range toMaintain {
		if idle {
			// Idle pass: the per-entry maintain work pauses with the fixed
			// tokens; only the idle-eviction sweep above runs.
			continue
		}
		// Same cooldown skip as the fixed-token loop: no queued-session
		// EnsureSession, no rotation while cooling down — and the same
		// live-ban skip so a hard-banned entry stops Maintain/rotate traffic
		// (its cooldown deadline is zero until an operator acts).
		if time.Now().Before(entry.runs.CooldownUntil()) || entry.runs.BanError() != nil {
			continue
		}
		mCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		entry.runs.Maintain(mCtx)
		// Same in-flight gate as the fixed-token loop: skip the queued-
		// session GET while a chat is in flight so it cannot kick the active
		// session (reference/freebuff-proxy-hengxin session-manager.js:37-49,
		// 259-260). Active-session liveness polls run on the jittered
		// bridgeSessionPollTick schedule instead.
		if entry.runs.InflightCount() == 0 {
			snap := entry.session.Snapshot()
			if snap.Status == "queued" {
				if _, err := entry.session.EnsureSession(mCtx); err != nil {
					p.logger.Debug("pool: bridge maintain session not ready", "err", err)
				} else {
					// Issue #90a: pre-create the run for the session's model
					// agent so the first request on this session does not pay
					// the START latency (mirrors the fixed-token path).
					after := entry.session.Snapshot()
					if agentID, err := p.reg.AgentForModel(after.Model); err == nil && agentID != "" {
						_ = entry.runs.Precreate(mCtx, agentID)
					}
				}
			}
		}
		cancel()
	}
}

// bridgeEvictToken immediately removes a token from the bridge cache:
// used when a token is confirmed dead (ErrAuthRejected) so it does not sit
// in the cache for the full idle-eviction TTL window. The removed entry's
// runs are FINISHed best-effort after releasing the lock. Entries with an
// outstanding lease are left cached for the idle sweep instead: FINISHing
// their runs would kill the concurrent chat stream, and dropping the entry
// now would orphan the stream's draining run outside bridgeMaintain's and
// Pool.Shutdown's reach.
func (p *Pool) bridgeEvictToken(rawToken string) {
	key := tokenKey(rawToken)
	p.bridgeMu.Lock()
	entry, ok := p.bridge[key]
	if !ok {
		p.bridgeMu.Unlock()
		return
	}
	// Gate the eviction on zero in-flight leases. A token confirmed
	// dead can still serve a live stream (the 401 may have raced another
	// request on the same token); FinishAllRuns on the busy entry would
	// kill the concurrent chat, and removing it would leave the stream's
	// run invisible to bridgeMaintain and Pool.Shutdown. When busy, keep
	// the entry cached — it is cooled down, so no new request passes it —
	// and let the idle sweep drop it once the leases drain.
	if inflight := entry.runs.InflightCount(); inflight > 0 {
		p.logger.Debug("pool: dead-token eviction deferred (in-flight leases)",
			"token_label", bridgeTokenLabel(entry), "inflight", inflight)
		p.bridgeMu.Unlock()
		return
	}
	p.bridgeRecordSurvivorLocked(entry, time.Now())
	delete(p.bridge, key)
	p.bridgeOrder = removeBridgeOrder(p.bridgeOrder, key)
	p.logger.Debug("pool: bridge entry evicted (dead token)", "token_label", bridgeTokenLabel(entry))
	p.bridgeMu.Unlock()

	// Best-effort cleanup: FINISH runs and end session, bounded by context.
	// A hung upstream should not stall the caller.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entry.runs.FinishAllRuns(ctx)
	if endErr := entry.session.EndSession(ctx); endErr != nil {
		p.logger.Warn("pool: bridge EndSession failed during dead-token eviction",
			"err", endErr, "token_label", bridgeTokenLabel(entry))
	}
}

// removeBridgeOrder drops token from the LRU order slice.
func removeBridgeOrder(order []string, token string) []string {
	for i, tok := range order {
		if tok == token {
			return append(order[:i], order[i+1:]...)
		}
	}
	return order
}
