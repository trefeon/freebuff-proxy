package pool

import (
	"fmt"
	"time"

	"freebuff-proxy/internal/notify"
	"freebuff-proxy/internal/upstream"
)

// CooldownToken puts token in a cooldown window of duration d (auth-reject
// recovery, e.g. runs.DefaultCooldown). Out-of-range tokens are ignored.
func (p *Pool) CooldownToken(token int, d time.Duration) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return
	}
	(*toks)[token].runs.Cooldown(d)
}

// CooldownTokenRateLimit applies a rate-limit cooldown to token
// (remembered so Acquire surfaces 429 + Retry-After during the window).
// Out-of-range tokens are ignored. When the refusal is spend_limited
// (issue #122), the event is also counted on the token's spend ledger —
// the $ ceiling is server-enforced, so the ledger only records the event.
func (p *Pool) CooldownTokenRateLimit(token int, rle *upstream.RateLimitError) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) || rle == nil {
		return
	}
	(*toks)[token].runs.CooldownRateLimit(rle)
	if rle.Status == "spend_limited" {
		p.spendMu.Lock()
		defer p.spendMu.Unlock()
		p.recordSpendLimited(token)
	}
	p.recordMismatchEscalation(token, rle)
}

// CooldownTokenIpCapped applies an ip_capped cooldown to token via
// runs.CooldownIpCapped: each hit backs off the error's RetryAfter + ±20%
// jitter, with a per-token daily re-admission cap (#118 — the 3rd hit in a
// rolling window locks until the Pacific-midnight reset and surfaces
// 429 ip_capped; upstream itself is admission-only, not a quota reset).
// Out-of-range tokens are ignored.
func (p *Pool) CooldownTokenIpCapped(token int, ice *upstream.IpCappedError) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) || ice == nil {
		return
	}
	(*toks)[token].runs.CooldownIpCapped(ice)
}

// CooldownTokenBan applies a ban cooldown to token (remembered so
// Acquire surfaces 403 banned + resumes-at during the window) and fires the
// token_banned webhook alert (issue #48, throttled per event type).
func (p *Pool) CooldownTokenBan(token int, be *upstream.BanError) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) || be == nil {
		return
	}
	(*toks)[token].runs.CooldownBan(be)
	p.notifyBan(token+1, "")
}

// CooldownTokenCountryBlocked applies a country-block cooldown to token
// (remembered so Acquire surfaces the region-block error during the ~15m
// window instead of re-hitting upstream).
func (p *Pool) CooldownTokenCountryBlocked(token int, cbe *upstream.CountryBlockedError) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) || cbe == nil {
		return
	}
	(*toks)[token].runs.CooldownCountryBlocked(cbe)
}

// CooldownBridge puts the bridge entry's token in a cooldown window of
// duration d (auth-reject recovery, e.g. runs.DefaultCooldown).
func (p *Pool) CooldownBridge(lease *Lease, d time.Duration) {
	if lease == nil || lease.Bridge == nil {
		return
	}
	lease.Bridge.runs.Cooldown(d)
}

// CooldownBridgeRateLimit applies a rate-limit cooldown to the bridge entry
// (remembered so AcquireBridge surfaces 429 + Retry-After). When the refusal
// is spend_limited (issue #122), the event is also counted on the entry's
// spend ledger — the $ ceiling is server-enforced, so the ledger only
// records the event.
func (p *Pool) CooldownBridgeRateLimit(lease *Lease, rle *upstream.RateLimitError) {
	if lease == nil || lease.Bridge == nil || rle == nil {
		return
	}
	lease.Bridge.runs.CooldownRateLimit(rle)
	if rle.Status == "spend_limited" {
		p.bridgeMu.Lock()
		defer p.bridgeMu.Unlock()
		p.bridgeRecordSpendLimited(lease.Bridge)
	}
	p.recordMismatchEscalation(0, rle) // bridge: tokenIndex 0, shared window
}

// CooldownBridgeIpCapped applies an ip_capped cooldown to the bridge entry
// via runs.CooldownIpCapped (full RetryAfter + jitter, per-token daily cap
// until Pacific midnight — #118).
func (p *Pool) CooldownBridgeIpCapped(lease *Lease, ice *upstream.IpCappedError) {
	if lease == nil || lease.Bridge == nil || ice == nil {
		return
	}
	lease.Bridge.runs.CooldownIpCapped(ice)
}

// CooldownBridgeBan applies a ban cooldown to the bridge entry (remembered
// so AcquireBridge surfaces 403 banned + resumes-at during the window) and
// fires the token_banned webhook alert (issue #48, throttled).
func (p *Pool) CooldownBridgeBan(lease *Lease, be *upstream.BanError) {
	if lease == nil || lease.Bridge == nil || be == nil {
		return
	}
	lease.Bridge.runs.CooldownBan(be)
	p.notifyBan(0, "")
}

// CooldownBridgeCountryBlocked applies a country-block cooldown to the
// bridge entry (remembered so AcquireBridge surfaces the region-block error
// during the ~15m window instead of re-hitting upstream).
func (p *Pool) CooldownBridgeCountryBlocked(lease *Lease, cbe *upstream.CountryBlockedError) {
	if lease == nil || lease.Bridge == nil || cbe == nil {
		return
	}
	lease.Bridge.runs.CooldownCountryBlocked(cbe)
}

// notifyBan fires the token_banned webhook (issue #48). tokenIndex is the
// 1-based pooled token index (0 = bridge). model is the requested model
// when the caller knows it ("" otherwise). Throttled by the sender.
func (p *Pool) notifyBan(tokenIndex int, model string) {
	p.notifyMu.Lock()
	defer p.notifyMu.Unlock()
	n := p.notify
	if n == nil {
		return
	}
	n.Send(notify.Event{Event: "token_banned", TokenIndex: tokenIndex, Model: model,
		Message: "a FreeBuff token was classified banned upstream (403)"})
}

// mismatchEscalation is the issue #140 P1 escalation guard's state for one
// token: free_mode_invalid_agent_model 403s inside stormWindow mean the
// registry is serving an id upstream retired — exactly how the v0.11.3-era
// PREFER_MAX_MODELS over-upgrade escalated accounts to `banned`. N hits in
// the window fire ONE operator webhook (the sender throttles repeats); the
// per-hit bounded cooldown already stops the request-path amplification.
type mismatchEscalation struct {
	hits      []time.Time // refusal timestamps, oldest first
	lastStorm time.Time   // zero = no webhook fired yet in this storm
}

const (
	mismatchWindow    = 60 * time.Second
	mismatchThreshold = 3
)

// recordMismatchEscalation counts one free_mode_invalid_agent_model hit for
// token and fires agent_model_mismatch_escalation once per window when the
// count crosses the threshold. Bridge entries share the pooled path through
// CooldownTokenRateLimit/CooldownBridgeRateLimit, so both surfaces alert.
func (p *Pool) recordMismatchEscalation(tokenIndex int, rle *upstream.RateLimitError) {
	if rle == nil || rle.Status != "free_mode_invalid_agent_model" {
		return
	}
	now := time.Now()
	p.mismatchMu.Lock()
	st := p.mismatch[tokenIndex]
	// Drop hits older than the window.
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
	p.mismatch[tokenIndex] = st
	p.mismatchMu.Unlock()

	if !fire {
		return
	}
	model := rle.Status // Status names the code; Model comes from the caller below
	p.notifyMu.Lock()
	n := p.notify
	p.notifyMu.Unlock()
	if n == nil {
		return
	}
	n.Send(notify.Event{Event: "agent_model_mismatch_escalation", TokenIndex: tokenIndex + 1,
		Model:   model,
		Message: "3+ free_mode_invalid_agent_model refusals in 60s on one token — the registry is likely serving a model upstream retired; refresh/restart or check MODELS_ALLOW before upstream escalates to ban"})
}

// UnlockToken clears any cooldown/rate-limit/ban lock on token so Acquire
// can use it again (dashboard unlock action).
func (p *Pool) UnlockToken(token int) error {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	(*toks)[token].runs.ClearCooldowns()
	return nil
}

// LockToken administratively excludes token from Acquire without clearing
// its cooldown state (dashboard lock action).
func (p *Pool) LockToken(token int) error {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	(*toks)[token].locked.Store(true)
	return nil
}

// UnlockLockToken clears the administrative lock on token so Acquire can
// use it again (dashboard unlock-lock action).
func (p *Pool) UnlockLockToken(token int) error {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	(*toks)[token].locked.Store(false)
	return nil
}

// LockBridgeEntry administratively excludes a bridge entry from
// AcquireBridge. key is the SHA-256 hash prefix (tokenKey) (#187).
func (p *Pool) LockBridgeEntry(key string) error {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	e, ok := p.bridge[key]
	if !ok {
		return fmt.Errorf("pool: bridge entry %s not found", key)
	}
	e.locked.Store(true)
	return nil
}

// UnlockBridgeEntry clears the administrative lock on a bridge entry so
// AcquireBridge can use it again (#187).
func (p *Pool) UnlockBridgeEntry(key string) error {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	e, ok := p.bridge[key]
	if !ok {
		return fmt.Errorf("pool: bridge entry %s not found", key)
	}
	e.locked.Store(false)
	return nil
}
