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
	"strings"
	"sync"
	"time"

	"freebuff-proxy/internal/phasetiming"
	"freebuff-proxy/internal/runs"
	"freebuff-proxy/internal/upstream"
)

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

	// B5: Global bridge daily limit check — before per-entry check, reject
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
	if until := entry.runs.CooldownUntil(); time.Now().Before(until) {
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
	// Issue #155: scarce-model session protection in bridge mode.
	scarceSet := scarceModelSet(cfg.ScarceSessionModels)
	if scarceHeld(entry.session.Snapshot(), model, scarceSet) {
		return nil, &ScarceSessionError{Model: model, ExpiresAt: entry.session.Snapshot().ExpiresAt}
	}

	// Issue #155: quota-exhaustion fallback in bridge mode.
	fellBack := false
	if bridgeQuotaCapped(entry, model) {
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
			return nil, bridgeQuotaLimitError(entry, model)
		}
	}

	// Per-entry single-flight: concurrent requests for the same bridge
	// token share one session creation. The leader creates the session;
	// followers block on the admissionGate channel until it completes.
	// On failure, the gate resets so the next request retries.
	var needsCreation bool
	select {
	case <-entry.admissionGate:
		// Session already created (or failed) by a concurrent request.
		// Re-read the cached state — if the session is usable AND
		// matches the requested model, skip creation; otherwise reset
		// the gate and create a fresh session for the new model.
		ss := entry.session.Snapshot()
		if (ss.Usable() || ss.Refreshing) && (ss.Model == "" || ss.Model == model) {
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

	if needsCreation {
		entry.admissionOnce.Do(func() {
			defer close(entry.admissionGate)

			// Session-create admission gate (issue #86): global + per-model
			// concurrency limiter.
			permit, gerr := p.gate.acquire(ctx, model)
			if gerr != nil {
				entry.admissionErr = gerr
				return
			}
			sessionStart := time.Now()
			_, serr := entry.session.EnsureSessionForModel(ctx, model)
			permit.Release()
			phasetiming.FromContext(ctx).Since(phasetiming.SessionRefreshMS, sessionStart)
			entry.admissionErr = serr
		})
	}

	// Check the leader's result.
	if entry.admissionErr != nil {
		err := entry.admissionErr
		// Reset the gate so the next request retries.
		entry.admissionGate = make(chan struct{})
		entry.admissionOnce = sync.Once{}
		entry.admissionErr = nil

		if errors.Is(err, upstream.ErrAuthRejected) {
			entry.runs.Cooldown(runs.DefaultCooldown)
			p.logger.Debug("pool: bridge entry cooling down", "duration", runs.DefaultCooldown.String())
			p.bridgeEvictToken(clientToken)
		}
		if rle := asRateLimit(err); rle != nil {
			entry.runs.CooldownRateLimit(rle)
			if rle.Status == "spend_limited" {
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
		if ice := asIpCapped(err); ice != nil {
			entry.runs.CooldownIpCapped(ice)
		}
		if be := asBan(err); be != nil {
			entry.runs.CooldownBan(be)
		}
		if cbe := asCountryBlocked(err); cbe != nil {
			entry.runs.CooldownCountryBlocked(cbe)
		}
		return nil, err
	}
	entry.runs.ClearCooldowns()

sessionReady:
	if err != nil {
		if errors.Is(err, upstream.ErrAuthRejected) {
			entry.runs.Cooldown(runs.DefaultCooldown)
			p.logger.Debug("pool: bridge entry cooling down", "duration", runs.DefaultCooldown.String())
			// B6: immediate eviction — the token is dead; do not let it
			// sit in the cache for 2h until the idle sweep catches it.
			p.bridgeEvictToken(clientToken)
		}
		if rle := asRateLimit(err); rle != nil {
			entry.runs.CooldownRateLimit(rle)
			if rle.Status == "spend_limited" {
				p.bridgeMu.Lock()
				p.bridgeRecordSpendLimited(entry)
				p.bridgeMu.Unlock()
			}
			// Issue #155: bridge quota exhaustion fallback
			if isQuotaExhaustedError(rle) {
				if fb := cfg.QuotaFallbackModels[model]; fb != "" && fb != model {
					p.logger.Info("pool: bridge token quota exhausted on admission, falling back", "token", bridgeTokenLabel(entry), "requested", model, "fallback", fb)
					// Issue #164: report the switch (X-FreeBuff-Fallback:
					// quota_exhausted) on the fallback lease.
					fbLease, fbErr := p.AcquireBridge(ctx, clientToken, fb)
					if fbLease != nil {
						fbLease.FallbackReason = "quota_exhausted"
					}
					return fbLease, fbErr
				}
			}
		}
		if ice := asIpCapped(err); ice != nil {
			entry.runs.CooldownIpCapped(ice)
		}
		if be := asBan(err); be != nil {
			entry.runs.CooldownBan(be)
		}
		if cbe := asCountryBlocked(err); cbe != nil {
			entry.runs.CooldownCountryBlocked(cbe)
		}
		return nil, err
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
		if errors.Is(err, upstream.ErrAuthRejected) {
			entry.runs.Cooldown(runs.DefaultCooldown)
			p.logger.Debug("pool: bridge entry cooling down", "duration", runs.DefaultCooldown.String())
			// B6: immediate eviction — the token is dead.
			p.bridgeEvictToken(clientToken)
		}
		if rle := asRateLimit(err); rle != nil {
			entry.runs.CooldownRateLimit(rle)
			// Issue #122: count run-start spend_limited refusals on the
			// bridge entry's ledger (same counter as the chat-path refusal).
			if rle.Status == "spend_limited" {
				p.bridgeMu.Lock()
				p.bridgeRecordSpendLimited(entry)
				p.bridgeMu.Unlock()
			}
		}
		if ice := asIpCapped(err); ice != nil {
			entry.runs.CooldownIpCapped(ice)
		}
		if be := asBan(err); be != nil {
			entry.runs.CooldownBan(be)
		}
		if cbe := asCountryBlocked(err); cbe != nil {
			entry.runs.CooldownCountryBlocked(cbe)
		}
		return nil, err
	}

	p.logger.Debug("pool: bridge lease acquired", "model", effectiveModel, "agent", effectiveAgentID, "instance_id", ss.InstanceID,
		"country", ss.CountryCode)
	entry.lastModel.Store(effectiveModel)
	// Track the activity and end any idle-maintenance pause, mirroring
	// Acquire: without this, IDLE_ROTATION_TIMEOUT was dead config in
	// bridge mode — lastActive stayed zero forever, so the pool never
	// idle-paused and bridge entries were maintained, polled, and
	// queued-advanced every pass indefinitely.
	p.lastActiveMu.Lock()
	p.lastActive = time.Now()
	p.idleFinished = false
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
	if toks := p.toks.Load(); len(*toks) > 0 {
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
	toks := p.toks.Load()
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
