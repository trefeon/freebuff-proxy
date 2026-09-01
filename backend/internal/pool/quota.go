package pool

import (
	"time"

	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
)

// ReferralGatedModel is the referral-earned model in the FreeBuff catalog
// (issue #183, reference freebuff-models.ts:1327-1370). It is NOT part of the
// daily free session pool and requires explicit referral quota or active promo.
const ReferralGatedModel = "z-ai/glm-5.2"

// isReferralGatedModel reports whether model is referral-only.
func isReferralGatedModel(model string) bool {
	return model == ReferralGatedModel
}

// quotaStateForSnapshot reports one current snapshot's session-quota state
// for model (issue #85, #183): known reports whether the quota is known with
// a positive remaining allowance; remaining is the positive delta; capped
// reports RecentCount >= Limit with a future ResetAt, or absence of referral
// entitlement for referral-gated models (the token must be skipped this pass —
// it cannot serve the model right now). Quotas with a past/absent ResetAt are
// treated as fresh (the window rolled) and never capped. It is the single
// implementation behind BOTH quotaRemaining (pooled token) and
// bridgeQuotaRemaining (bridge entry), so the window semantics — Pacific
// resets, capped vs fresh windows, referral entitlement — cannot drift
// between the two modes.
func quotaStateForSnapshot(snap session.SessionSnapshot, model string) (known bool, remaining float64, capped bool) {
	if isReferralGatedModel(model) {
		if !snap.HasGlmEntitlement() {
			// Token has no referral entitlement for GLM 5.2. Treat as capped
			// so it is excluded from admission attempts (prevents 403 account_banned).
			return false, 0, true
		}
		if q, ok := snap.QuotaByModel[model]; ok && q.Limit > 0 {
			resetFuture := !q.ResetAt.IsZero() && q.ResetAt.After(time.Now())
			if resetFuture && q.RecentCount >= q.Limit {
				return false, 0, true
			}
			if q.RecentCount < q.Limit {
				return true, q.Limit - q.RecentCount, false
			}
		}
		return true, 1, false
	}
	q, ok := snap.QuotaByModel[model]
	if !ok || q.Limit <= 0 {
		return false, 0, false
	}
	resetFuture := !q.ResetAt.IsZero() && q.ResetAt.After(time.Now())
	if resetFuture && q.RecentCount >= q.Limit {
		return false, 0, true
	}
	if q.RecentCount < q.Limit {
		return true, q.Limit - q.RecentCount, false
	}
	// RecentCount >= Limit but the window already rolled: unknown until the
	// next admission reports a fresh count.
	return false, 0, false
}

// quotaRemaining reports the entry's session-quota state for model
// (quotaStateForSnapshot on the entry's current snapshot; see there for the
// window semantics). It is the single implementation behind both the pooled
// and bridge modes (issue #261) — the entry-adapter interface collapses the
// former quotaRemaining / bridgeQuotaRemaining twins.
func quotaRemaining(acc tokenAccount, model string) (known bool, remaining float64, capped bool) {
	return quotaStateForSnapshot(acc.sessionMgr().Snapshot(), model)
}

// quotaLimitErrorForSnapshot builds the 429 surfaced when a snapshot's quota
// is exhausted for model (issue #85, #183): RetryAfter is the time until the
// window reset, mirroring the upstream RateLimitError contract. Shared by
// quotaLimitError (pooled) and bridgeQuotaLimitError (bridge).
func quotaLimitErrorForSnapshot(snap session.SessionSnapshot, model string) *upstream.RateLimitError {
	q := snap.QuotaByModel[model]
	retryAfter := time.Duration(0)
	if !q.ResetAt.IsZero() && q.ResetAt.After(time.Now()) {
		retryAfter = time.Until(q.ResetAt)
	}
	body := "session quota exhausted for model"
	if isReferralGatedModel(model) && !snap.HasGlmEntitlement() {
		body = "referral entitlement required for " + model
	}
	return &upstream.RateLimitError{
		Status:      "rate_limited",
		Model:       model,
		RetryAfter:  retryAfter,
		Limit:       q.Limit,
		RecentCount: q.RecentCount,
		ResetAt:     q.ResetAt,
		Body:        body,
	}
}

// quotaLimitError builds the 429 surfaced when an entry is excluded for the
// model's exhausted session quota (see quotaLimitErrorForSnapshot). Shared
// by both modes (issue #261).
func quotaLimitError(acc tokenAccount, model string) *upstream.RateLimitError {
	return quotaLimitErrorForSnapshot(acc.sessionMgr().Snapshot(), model)
}

// --- pool quota/snapshot ---

// recordChat appends one successful upstream chat for token and prunes the
// token's usage history outside the 24h window. The ledger travels with the
// entry (issue #263), so the roster's single mutex guards it.
func (p *Pool) recordChat(token int) { p.roster.recordChat(token) }

// recordChatEntry appends one successful upstream chat for the lease's
// backing entry by pointer and prunes its usage history outside the 24h
// window. The entry is the authoritative owner of its ledger, so after a
// concurrent RemoveLastToken+AddToken a lease's Token index is never used
// to locate the ledger — the pointer stays immune to index reuse.
func (p *Pool) recordChatEntry(entry *tokenEntry) { p.roster.recordChatEntry(entry) }

// usageCount returns how many successful chats token sent within the last
// usageWindow, pruning expired timestamps.
func (p *Pool) usageCount(token int) int { return p.roster.usageCount(token) }

// dailyLimitError builds the 429 surfaced when token is capped by
// MAX_MESSAGES_PER_DAY: RetryAfter is the time until the token's oldest
// recorded chat ages out of the 24h window (the earliest moment a slot
// frees).
func (p *Pool) dailyLimitError(token int) *upstream.RateLimitError {
	return &upstream.RateLimitError{
		RetryAfter:  p.roster.usageResetIn(token),
		Limit:       float64(p.cfg.Load().MaxMessagesPerDay),
		RecentCount: float64(p.roster.usageCount(token)),
		Body:        "daily message limit reached",
	}
}

// usageResetIn is how long until token's oldest usage timestamp ages out of
// the window (0 when the token has no recorded usage or the reset is due).
func (p *Pool) usageResetIn(token int) time.Duration { return p.roster.usageResetIn(token) }

// --- bridge mode internals ---

// bestDailyLimit picks the daily-cap error whose window frees first: the
// client retries when the first token has a free slot again.
func bestDailyLimit(entries []*upstream.RateLimitError) *upstream.RateLimitError {
	best := entries[0]
	for _, e := range entries[1:] {
		if e.RetryAfter < best.RetryAfter {
			best = e
		}
	}
	return best
}

// bridgeRecordChat appends one successful upstream chat for the bridge entry
// and prunes its usage history outside the 24h window. The entry's ledger is
// shared with the pooled path (issue #263); only this mode's global
// BRIDGE_DAILY_LIMIT counter is extra.
func (p *Pool) bridgeRecordChat(entry *bridgeEntry) {
	if entry == nil {
		return
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	entry.ledger.recordChat(time.Now())
	p.bridgeDailyUsage++
}

// bridgeUsageCount returns how many successful chats the bridge entry sent
// within the last usageWindow, pruning expired timestamps.
func (p *Pool) bridgeUsageCount(entry *bridgeEntry) int {
	if entry == nil {
		return 0
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return entry.ledger.usageCount(time.Now())
}

// bridgeUsageResetIn is how long until the bridge entry's oldest usage
// timestamp ages out of the window (0 when no usage is recorded or the
// reset is due).
func (p *Pool) bridgeUsageResetIn(entry *bridgeEntry) time.Duration {
	if entry == nil {
		return 0
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return entry.ledger.usageResetIn(time.Now())
}

// bridgeDailyLimitError builds the 429 surfaced when the bridge entry is
// capped by MAX_MESSAGES_PER_DAY (mirrors dailyLimitError for fixed
// tokens): RetryAfter is the time until the entry's oldest recorded chat
// ages out of the 24h window.
func (p *Pool) bridgeDailyLimitError(entry *bridgeEntry) *upstream.RateLimitError {
	return &upstream.RateLimitError{
		RetryAfter:  p.bridgeUsageResetIn(entry),
		Limit:       float64(p.cfg.Load().MaxMessagesPerDay),
		RecentCount: float64(p.bridgeUsageCount(entry)),
		Body:        "daily message limit reached",
	}
}
