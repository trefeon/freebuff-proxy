// bridge.go — bridge-mode acquire and token validation.
//
// Bridge mode serves a single upstream account per client-supplied token
// (no AUTH_TOKENS configured). Each client token is lazily mapped to a
// bridgeEntry (upstream client + session manager + run manager); the LRU
// cache and its maintain/evict loops live in bridge_cache.go.
package pool

import (
	"context"
	"errors"
	"fmt"
	"freebuff-proxy/backend/internal/phasetiming"
	"freebuff-proxy/backend/internal/runs"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// bridgeEntry is one lazily-created client-token slot in bridge mode: the
// upstream client, session manager, and run manager for a single client-
// supplied token, created on first use and reused across that client's
// later requests. lastUsed and the ledger are guarded by Pool.bridgeMu.
// It lives here (with the bridge cache twins) so the bridge subsystem is not
// entangled with the pool core through shared state (issue #261).
type bridgeEntry struct {
	token    string
	client   *upstream.Client
	session  *session.Manager
	runs     *runs.RunManager
	lastUsed time.Time
	// ledger is the entry's usage + spend state (issue #263); guarded by
	// Pool.bridgeMu like lastUsed.
	ledger *AccountLedger
	// nextPollAt / pollFailures carry the session-liveness poll schedule;
	// touched only by the maintain goroutine (bridgeSessionPollTick).
	nextPollAt   time.Time
	pollFailures int
	// locked is an administrative lock that prevents AcquireBridge from
	// leasing runs to this entry (#187). Set/cleared by LockBridgeEntry/
	// UnlockBridgeEntry; in-flight leases are unaffected.
	locked atomic.Bool

	// admissionGate serializes session creation per entry: the first
	// request creates the session; concurrent requests block on the
	// channel until it completes or fails. sync.Once ensures the session
	// is created exactly once per entry lifecycle. Guarded by mu.
	mu            sync.Mutex
	admissionGate chan struct{}
	admissionOnce sync.Once
	admissionErr  error // result of leader's session creation
}

// tokenAccount is the entry-adapter interface shared by tokenEntry and
// bridgeEntry so the quota/ledger helpers can serve both front doors through
// one set of accessors instead of per-mode twins (issues #261/#263).
type tokenAccount interface {
	sessionMgr() *session.Manager
	runMgr() *runs.RunManager
	clientMgr() *upstream.Client
	accountLedger() *AccountLedger
}

func (e *bridgeEntry) sessionMgr() *session.Manager  { return e.session }
func (e *bridgeEntry) runMgr() *runs.RunManager      { return e.runs }
func (e *bridgeEntry) clientMgr() *upstream.Client   { return e.client }
func (e *bridgeEntry) accountLedger() *AccountLedger { return e.ledger }

func (e *tokenEntry) sessionMgr() *session.Manager  { return e.session }
func (e *tokenEntry) runMgr() *runs.RunManager      { return e.runs }
func (e *tokenEntry) clientMgr() *upstream.Client   { return e.client }
func (e *tokenEntry) accountLedger() *AccountLedger { return e.ledger }

// AcquireBridge acquires a lease for one client-supplied token in bridge
// mode (no AUTH_TOKENS configured). The entry — upstream client, session
// manager, and run manager — is created lazily on first use and cached for
// reuse across that client's later requests (least quota burn). There is no
// multi-token failover: a single token either yields a lease or its error
// is returned as-is. Registry misses pass through.
func (p *Pool) AcquireBridge(ctx context.Context, clientToken, model string) (*Lease, error) {
	clientToken = strings.TrimSpace(clientToken)
	cfg := p.cfg.Load()
	if clientToken == "" {
		return nil, errors.New("bridge: empty client token")
	}
	// QUOTA_FALLBACK_MODELS recursion backstop (mirrors the pooled path in
	// Acquire): the bridge fallback chain is bounded by a depth counter
	// carried in ctx, so a fallback cycle degrades to an error instead of a
	// stack overflow.
	depth, _ := ctx.Value(fallbackDepthKey{}).(int)
	if depth >= maxFallbackDepth {
		return nil, errors.New("pool: QUOTA_FALLBACK_MODELS cycle detected (max fallback depth reached); check QUOTA_FALLBACK_MODELS for a loop")
	}
	ctx = context.WithValue(ctx, fallbackDepthKey{}, depth+1)

	// Post-drain re-admission gate (mirrors Acquire): no session POST or
	// run START once Shutdown is draining.
	if p.draining.Load() {
		return nil, errors.New("pool: shutting down")
	}

	// Hybrid mode: a credential that equals a pooled AUTH_TOKENS entry must
	// not be relayed as a bridge token — it would create a second path to
	// the same upstream account (a pooled lease AND a bridge entry), which
	// can admit two concurrent sessions. The legit hybrid route for a
	// pooled account is API_KEYS-authenticated pooled access.
	if cfg.HybridBridgeMode() && p.isPooledToken(clientToken) {
		return nil, fmt.Errorf("bridge: credential matches a pooled AUTH_TOKENS entry; use an API_KEYS key for pooled access or a different client token")
	}

	agentID, err := p.reg.AgentForModel(model)
	if err != nil {
		return nil, err
	}

	entry, err := p.bridgeEntryFor(clientToken)
	if err != nil {
		return nil, err
	}

	// Administrative lock: skip entries locked by the dashboard (#187).
	// In-flight leases are unaffected (they were acquired before the lock).
	if entry.locked.Load() {
		return nil, fmt.Errorf("bridge token %s is locked by administrator", tokenKey(clientToken))
	}

	// Global bridge daily limit check — before per-entry check, reject
	// if the total across ALL bridge entries exceeds BRIDGE_DAILY_LIMIT.
	if cfg.BridgeDailyLimit > 0 {
		// TOCTOU: snapshot read, then compare after unlock. Worst case: one
		// extra request past the limit. Acceptable for a best-effort cap.
		p.bridgeMu.Lock()
		total := p.bridgeDailyUsage
		p.bridgeMu.Unlock()
		if total >= cfg.BridgeDailyLimit {
			p.logger.Debug("pool: bridge global daily limit reached", "limit", cfg.BridgeDailyLimit, "used", total)
			return nil, fmt.Errorf("bridge: global daily limit %d reached (%d used)", cfg.BridgeDailyLimit, total)
		}
	}

	// Cooldown: skip the entry during its window; surface the remembered
	// ban/country-block/rate-limit error so the client keeps getting 403/429
	// instead of a generic failure (mirrors the fixed-token cooldown-skip
	// branch). The remembered errors are mutually exclusive in the run
	// manager; checked in pool precedence order.
	if until := entry.runs.CooldownUntil(); time.Now().Before(until) || entry.runs.BanError() != nil {
		if rle := entry.runs.RateLimitError(); rle != nil && rle.Model != "" && rle.Model != model && isQuotaExhaustedError(rle) {
			// Entry is only quota-capped for rle.Model, proceed for `model`.
		} else {
			if be := entry.runs.BanError(); be != nil {
				return nil, be
			}
			if cbe := entry.runs.CountryBlockedError(); cbe != nil {
				return nil, cbe
			}
			if rle := entry.runs.RateLimitError(); rle != nil {
				return nil, rle
			}
			if ice := entry.runs.IpCappedError(); ice != nil {
				return nil, ice
			}
			return nil, fmt.Errorf("bridge: token cooling down until %s", until.Format(time.RFC3339))
		}
	}

	// Daily rolling cap, per client token (mirrors the fixed-token path).
	if cfg.MaxMessagesPerDay > 0 && p.bridgeUsageCount(entry) >= cfg.MaxMessagesPerDay {
		p.logger.Debug("pool: bridge entry daily message limit", "limit", cfg.MaxMessagesPerDay)
		return nil, p.bridgeDailyLimitError(entry)
	}
	// Per-minute request cap (MAX_REQUESTS_PER_MINUTE), per client token.
	if cfg.MaxRequestsPerMinute > 0 && p.bridgeRpmCount(entry) >= cfg.MaxRequestsPerMinute {
		p.logger.Debug("pool: bridge entry per-minute request limit", "limit", cfg.MaxRequestsPerMinute)
		return nil, p.bridgeRpmLimitError(entry)
	}
	// Daily request cap (MAX_REQUESTS_PER_DAY), per client token: unlocks at
	// the next Pacific midnight (the official daily reset instant).
	if cfg.MaxRequestsPerDay > 0 && p.bridgeDayRequestCount(entry) >= cfg.MaxRequestsPerDay {
		p.logger.Debug("pool: bridge entry daily request limit", "limit", cfg.MaxRequestsPerDay)
		return nil, p.bridgeDayRequestLimitError(entry)
	}
	fellBack := false
	// Issue #155: quota-exhaustion fallback in bridge mode. A capped
	// entry holding a live session for the requested model is EXEMPT
	// (hotReusableForModel): the single-flight gate below reuses the
	// live instance with zero admission POST, so falling back (or 429ing)
	// would strand a session that can still serve.
	if _, _, quotaCapped := quotaRemaining(entry, model); quotaCapped && !hotReusableForModel(entry, model) {
		if fb := cfg.QuotaFallbackModels[model]; fb != "" && fb != model {
			p.logger.Info("pool: bridge token quota exhausted, falling back", "token", bridgeTokenLabel(entry), "requested", model, "fallback", fb)
			fbAgent, err := p.reg.AgentForModel(fb)
			if err != nil {
				return nil, err
			}
			model = fb
			agentID = fbAgent
			fellBack = true // issue #164: report the switch to the client
		} else {
			return nil, quotaLimitError(entry, model)
		}
	}

	// Per-entry single-flight: concurrent requests for the same bridge
	// token share one session creation. The leader creates the session;
	// followers block on the admissionGate channel until it completes.
	// On failure, the gate resets so the next request retries.
	// Guarded by entry.mu to avoid races on admissionGate/Once/Err.
	// Review 2026-08-31 P2-4: the single-flight is per ENTRY, not per
	// model — a follower that parked on a leader admitted for a different
	// model re-checks the live session after the leader result below and
	// restarts admission for its own model (admitRetry), bounded by
	// admitAttempts so back-to-back foreign-model leaders cannot livelock.
	var needsCreation bool
	var admitAttempts int
	// maxAdmissionModelRetries bounds the foreign-model restarts below.
	const maxAdmissionModelRetries = 3
admitRetry:
	entry.mu.Lock()
	select {
	case <-entry.admissionGate:
		// Session already created (or failed) by a concurrent request.
		// Re-read the cached state — if the session is usable AND
		// matches the requested model, skip creation; otherwise reset
		// the gate and create a fresh session for the new model.
		ss := entry.session.Snapshot()
		if (ss.Usable() || ss.Refreshing) && (ss.Model == "" || ss.Model == model) {
			entry.mu.Unlock()
			goto sessionReady
		}
		// Model mismatch or stale — reset gate for fresh creation.
		entry.admissionGate = make(chan struct{})
		entry.admissionOnce = sync.Once{}
		entry.admissionErr = nil
		needsCreation = true
	default:
		needsCreation = true
	}
	entry.mu.Unlock()

	if needsCreation {
		entry.admissionOnce.Do(func() {
			defer func() {
				entry.mu.Lock()
				close(entry.admissionGate)
				entry.mu.Unlock()
			}()

			// Session-create admission gate (issue #86): global + per-model
			// concurrency limiter.
			permit, gerr := p.gate.acquire(ctx, model)
			if gerr != nil {
				entry.mu.Lock()
				entry.admissionErr = gerr
				entry.mu.Unlock()
				return
			}
			// Issue #94(b): WAITING_ROOM_CHAIN gate — when the upstream last
			// refused this bridge token with 428 waiting_room_required, fire
			// the reference pre-session ad-chain + streak flow (best-effort,
			// bounded by the client's own chain timeout) before the next
			// session create so the admission does not bounce off the same
			// 428 again (mirrors the fixed-token path in acquire.go).
			if cfg.WaitingRoomChain && entry.client.ConsumeWaitingRoomChain() {
				p.logger.Debug("pool: bridge firing waiting-room pre-session chain", "token", bridgeTokenLabel(entry))
				entry.client.FireWaitingRoomChain(ctx)
			}
			sessionStart := time.Now()
			_, serr := entry.session.EnsureSessionForModel(ctx, model)
			permit.Release()
			phasetiming.FromContext(ctx).Since(phasetiming.SessionRefreshMS, sessionStart)
			entry.mu.Lock()
			entry.admissionErr = serr
			entry.mu.Unlock()
		})
	}

	// Check the leader's result (guarded by entry.mu).
	entry.mu.Lock()
	if entry.admissionErr != nil {
		errCopy := entry.admissionErr
		// Reset the gate so the next request retries.
		entry.admissionGate = make(chan struct{})
		entry.admissionOnce = sync.Once{}
		entry.admissionErr = nil
		entry.mu.Unlock()
		err = errCopy
		c := p.classifyAndCooldown(entry.runs, err)
		if c.authRejected {
			p.logger.Debug("pool: bridge entry cooling down", "duration", runs.DefaultCooldown.String())
			p.bridgeEvictToken(clientToken)
		}
		if rle := c.rateLimited; rle != nil {
			if c.spendLimited {
				p.bridgeMu.Lock()
				p.bridgeRecordSpendLimited(entry)
				p.bridgeMu.Unlock()
			}
			if isQuotaExhaustedError(rle) {
				if fb := cfg.QuotaFallbackModels[model]; fb != "" && fb != model {
					p.logger.Info("pool: bridge token quota exhausted on admission, falling back", "token", bridgeTokenLabel(entry), "requested", model, "fallback", fb)
					fbLease, fbErr := p.AcquireBridge(ctx, clientToken, fb)
					if fbLease != nil {
						fbLease.FallbackReason = "quota_exhausted"
					}
					return fbLease, fbErr
				}
			}
		}
		if lie := c.limitedIp; lie != nil {
			// Issue #74: the shared egress cannot serve this model
			// (limited_ip) — mark it unfit so pooled requests refuse fast
			// instead of re-admitting and burning a daily session slot on
			// every token. Bridge requests keep their own token, so the
			// unfit gate stays skipped for them (by design), but the
			// surfaced error is now self-describing.
			lie.Model = model
			p.MarkModelUnfit(model, lie)
		}
		if be := c.banned; be != nil {
			p.notifyBan(0, model) // issue #48: alert on admission-path bans
		}
		return nil, err
	}
	entry.mu.Unlock()

	// Review 2026-08-31 P2-4: the single-flight above is per ENTRY, not
	// per model. A follower that parked on a leader whose session was
	// admitted for a different model must not chat on that session while
	// reporting its own model: reset the gate exactly like the
	// closed-gate branch above and restart admission for the requested
	// model, bounded so pathological interleavings give up instead of
	// looping.
	if snap := entry.session.Snapshot(); snap.Usable() && snap.Model != "" && snap.Model != model {
		admitAttempts++
		if admitAttempts >= maxAdmissionModelRetries {
			entry.mu.Lock()
			entry.admissionGate = make(chan struct{})
			entry.admissionOnce = sync.Once{}
			entry.admissionErr = nil
			entry.mu.Unlock()
			return nil, fmt.Errorf("bridge: session serves model %q, requested %q (admission retry bound exhausted)", snap.Model, model)
		}
		entry.mu.Lock()
		entry.admissionGate = make(chan struct{})
		entry.admissionOnce = sync.Once{}
		entry.admissionErr = nil
		entry.mu.Unlock()
		goto admitRetry
	}

sessionReady:
	// The error path was handled above (inside the leader-result check):
	// every admission error returns there, so this label is only reached
	// with a usable session and err == nil.
	// Re-validate the entry is still cached before handing it a run: the
	// admission single-flight can park this goroutine with zero in-flight
	// leases, and an eviction (dead-token or LRU) in that window deletes
	// the entry and ends its upstream session — chatting against it would
	// surface a spurious 401/428 to a live client and strand the run
	// outside bridgeMaintain/Pool.Shutdown (review 2026-08-31 P2). The
	// pooled path skips a token that vanished mid-admission
	// (acquire.go:499); bridge has no failover, so the analog is aborting
	// with a retryable error — a fresh AcquireBridge recreates the entry.
	// An in-call retry was rejected: it would redo the whole admission
	// single-flight for a brand-new entry.
	p.bridgeMu.RLock()
	evicted := p.bridge[tokenKey(clientToken)] != entry
	p.bridgeMu.RUnlock()
	if evicted {
		return nil, fmt.Errorf("bridge: entry evicted during admission; retry the request")
	}
	entry.runs.ClearCooldowns()
	ss := entry.session.Snapshot()
	effectiveModel := model
	effectiveAgentID := agentID
	// Bridge mode: always use the requested model. The upstream may serve
	// a different model in the response body — we track what the client asked for.
	if p.reg != nil {
		if resolvedAgent, aerr := p.reg.AgentForModel(effectiveModel); aerr == nil {
			effectiveAgentID = resolvedAgent
		}
	}
	// Issue #90a: pre-create the run at session admission (best-effort).
	_ = entry.runs.Precreate(ctx, effectiveAgentID)
	runStart := time.Now()
	run, err := entry.runs.Acquire(ctx, effectiveAgentID)
	phasetiming.FromContext(ctx).Since(phasetiming.RunAcquireMS, runStart)
	if err != nil {
		c := p.classifyAndCooldown(entry.runs, err)
		if c.authRejected {
			p.logger.Debug("pool: bridge entry cooling down", "duration", runs.DefaultCooldown.String())
			// immediate eviction — the token is dead.
			p.bridgeEvictToken(clientToken)
		}
		if rle := c.rateLimited; rle != nil {
			// Issue #122: count run-start spend_limited refusals on the
			// bridge entry's ledger (same counter as the chat-path refusal).
			if c.spendLimited {
				p.bridgeMu.Lock()
				p.bridgeRecordSpendLimited(entry)
				p.bridgeMu.Unlock()
			}
		}
		if lie := c.limitedIp; lie != nil {
			// Issue #74: the shared egress cannot serve this model
			// (limited_ip) — mark it unfit so pooled requests refuse fast
			// instead of re-admitting and burning a daily session slot on
			// every token. Bridge requests keep their own token, so the
			// unfit gate stays skipped for them (by design), but the
			// surfaced error is now self-describing.
			lie.Model = model
			p.MarkModelUnfit(model, lie)
		}
		if be := c.banned; be != nil {
			p.notifyBan(0, model) // issue #48: alert on admission-path bans
		}
		return nil, err
	}

	p.logger.Debug("pool: bridge lease acquired", "model", effectiveModel, "agent", effectiveAgentID, "instance_id", ss.InstanceID,
		"country", ss.CountryCode)
	// MAX_REQUESTS_PER_MINUTE admission enforced atomically at grant time,
	// mirroring Acquire: the pre-filter above only reads the window, and a
	// concurrent burst must not pass the cap before any record lands.
	// Admission is always recorded (even with cap 0 = unlimited) so the
	// bridge snapshot counters stay meaningful.
	if !p.bridgeTryAdmitRequest(entry) {
		p.LeaseRelease(&Lease{Token: -1, Model: effectiveModel, AgentID: effectiveAgentID, Run: run, SessionInstanceID: ss.InstanceID,
			Bridge: entry, AcquiredAt: time.Now()})
		return nil, p.bridgeRpmLimitError(entry)
	}
	// Track the activity and end any idle-maintenance pause, mirroring
	// Acquire: without this, IDLE_ROTATION_TIMEOUT was dead config in
	// bridge mode — lastActive stayed zero forever, so the pool never
	// idle-paused and bridge entries were maintained, polled, and
	// queued-advanced every pass indefinitely.
	p.lastActiveMu.Lock()
	p.lastActive = time.Now()
	p.idleFinished = false
	p.sessionsEnded = false
	p.lastActiveMu.Unlock()
	fallbackReason := ""
	if fellBack {
		fallbackReason = "quota_exhausted"
	}
	return &Lease{Token: -1, Model: effectiveModel, AgentID: effectiveAgentID, Run: run, SessionInstanceID: ss.InstanceID,
		Bridge: entry, FallbackReason: fallbackReason, AcquiredAt: time.Now()}, nil
}

// ProbeNewToken validates a NOT-yet-added token against upstream with a
// zero-cost GET session probe (no session claim, no model needed). It builds
// the probe client from the pool's own config, so the base URL matches what
// AddToken would use (tests inject a mock URL here). Returns the session
// state on success, ErrNoActiveSession when the token is valid but idle,
// or the classified auth/network error (ErrBanned / ErrCountryBlocked /
// ErrAuthRejected / ErrRateLimited) otherwise.
func (p *Pool) ProbeNewToken(ctx context.Context, token string) (*upstream.SessionState, error) {
	if token == "" {
		return nil, errors.New("pool: empty token")
	}
	cfg := *p.cfg.Load()
	// Match the base URL of an existing pooled client when one exists: the
	// pool's fixed-token clients were built by the caller with the effective
	// upstream URL (tests inject a mock URL), while p.cfg.UpstreamBaseURL
	// may still hold the production default. A probe built from the wrong
	// URL would validate against a different host than the one the token
	// will actually use — silently false results.
	if toks := p.roster.Load(); len(*toks) > 0 {
		if base := (*toks)[0].client.BaseURL(); base != "" {
			cfg.UpstreamBaseURL = base
		}
	}
	client, err := upstream.New(token, &cfg)
	if err != nil {
		return nil, fmt.Errorf("pool: probe token: %w", err)
	}
	return client.ProbeAccount(ctx)
}

// ProbeToken validates token against upstream with a zero-cost GET session
// probe (dashboard test action): no session is created or claimed. Returns
// the live session state (including RateLimitsByModel quota) on success, or
// ErrNoActiveSession when the token has no active session (still a valid
// token), or the classified auth/network error otherwise.
func (p *Pool) ProbeToken(ctx context.Context, token int) (*upstream.SessionState, error) {
	toks := p.roster.Load()
	if token < 0 || token >= len(*toks) {
		return nil, fmt.Errorf("pool: token %d out of range", token)
	}
	tok := (*toks)[token]
	st, err := tok.client.ProbeAccount(ctx)
	if err == nil && st != nil {
		tok.session.UpdateQuotaFromProbe(st)
	}
	return st, err
}
