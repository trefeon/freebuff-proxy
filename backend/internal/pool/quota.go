package pool

import (
	"fmt"
	"time"

	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
)

// freebucksCapped reports whether the token's Freebucks allowance is exhausted
// for model (issue #321 wire drift: balance is now the server-computed
// spendable = daily.remaining + wallet.balance; vendor af898dc adds eligible
// earned grants admission may convert, so the gate uses Spendable() =
// balance + claimableGrantFreebucks, mirroring getFreebucksModelMeter).
// When Freebucks is absent or the model has no price, the token is not
// capped. The token is capped when spendable < price, or when the monthly
// dollar allowance is spent (wire drift 2026-09-04, issue #330 — fresh
// sessions stop upstream regardless of the daily balance). RetryAfter is the
// earliest future recovery instant among the applicable windows. When every
// recovery instant is past or unknown, the stored numbers are self-declared
// stale and the token is NOT capped — one admission revalidates live truth
// (polls never carry Freebucks, so nothing else could refresh them).
func freebucksCapped(acc tokenAccount, model string) (bool, time.Duration) {
	return freebucksCappedForSnapshot(acc.sessionMgr().Snapshot(), model)
}

// freebucksCappedForSnapshot is the snapshot-direct form of freebucksCapped
// (kept for testing and for acquireOrder's quotaLimited loop which already
// holds a snapshot).
func freebucksCappedForSnapshot(snap session.SessionSnapshot, model string) (bool, time.Duration) {
	fb := snap.Freebucks
	if fb == nil {
		return false, 0
	}
	price, ok := fb.Prices[model]
	if !ok {
		return false, 0
	}
	// Monthly dollar allowance (wire drift 2026-09-04, issue #330): when
	// the period is spent, fresh sessions stop upstream regardless of the
	// daily balance. Absent on older servers (nil) — no behavior change.
	monthlySpent := fb.Monthly != nil && fb.Monthly.RemainingUsd <= 0
	// Server-authorized quota exemption (wire drift 2026-09-05, issue
	// #350): new sessions stay usable at zero balance — the meter's
	// canStart is exempt || balance >= price. The monthly allowance still
	// gates (separate upstream refusal).
	if fb.QuotaExempt && !monthlySpent {
		return false, 0
	}
	// Claimable earned grants count toward canStart (vendor af898dc
	// getFreebucksModelMeter: balance + claimableGrantFreebucks >= price).
	if fb.Spendable() >= price && !monthlySpent {
		return false, 0
	}
	// Capped. Recovery signals: the daily pool refill, the plan's next
	// wallet bonus, and the monthly allowance reset (when the monthly
	// period is what blocks). Take the earliest future instant; when
	// nothing is known, surface 0.
	now := time.Now()
	earliest := time.Time{}
	candidates := []time.Time{fb.Daily.ResetAt, fb.Wallet.NextBonusAt}
	if monthlySpent {
		candidates = append(candidates, fb.Monthly.ResetAt)
	}
	for _, t := range candidates {
		if t.IsZero() || !t.After(now) {
			continue
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	if earliest.IsZero() {
		// No future recovery instant: the stored numbers are self-declared
		// stale (their own windows passed, or a server that never sent
		// reset times). Treat as unknown so one admission revalidates
		// against live upstream truth — a genuine refusal re-caps with
		// fresh data — instead of 429ing forever on frozen numbers no
		// refresh path can update (polls do not carry Freebucks).
		return false, 0
	}
	return true, time.Until(earliest)
}

// freebucksLimitError builds the 429 surfaced when Freebucks balance is
// insufficient for model. RetryAfter mirrors freebucksCapped's window-reset
// signal.
func freebucksLimitError(acc tokenAccount, model string) *upstream.RateLimitError {
	return freebucksLimitErrorForSnapshot(acc.sessionMgr().Snapshot(), model)
}

func freebucksLimitErrorForSnapshot(snap session.SessionSnapshot, model string) *upstream.RateLimitError {
	fb := snap.Freebucks
	price := 0.0
	if fb != nil {
		if p, ok := fb.Prices[model]; ok {
			price = p
		}
	}
	capped, retryAfter := freebucksCappedForSnapshot(snap, model)
	_ = capped
	body := "freebucks balance insufficient for model"
	// Surface price vs spendable in the diagnostic body when available
	// (spendable = balance + claimable grants, the gated amount).
	if fb != nil {
		body = body + " (spendable " + formatFreebucksBalance(fb.Spendable()) + " < price " + formatFreebucksBalance(price) + ")"
		if fb.Monthly != nil && fb.Monthly.RemainingUsd <= 0 {
			body = "freebucks monthly allowance exhausted for model"
		}
	}
	return &upstream.RateLimitError{
		Status:     "rate_limited",
		Model:      model,
		RetryAfter: retryAfter,
		Body:       body,
	}
}

func formatFreebucksBalance(v float64) string {
	return fmt.Sprintf("%g", v)
}

// recordChat appends one successful upstream chat for token and prunes the
// token's usage history outside the 24h window. The ledger travels with the
// entry (issue #263), so the roster's single mutex guards it.
func (p *Pool) recordChat(token int) { p.roster.recordChat(token) }

// recordChatEntry appends one successful upstream chat for the lease's
// backing entry by pointer and prunes its usage history outside the 24h
// window. The entry is the authoritative owner of its ledger, so after a
// concurrent RemoveLastToken+AddToken a lease's Token index is never used
// to locate the ledger — the pointer stays immune to index reuse.
func (p *Pool) recordChatEntry(entry *tokenEntry) {
	p.roster.recordChatEntry(entry)
	p.markPersistDirty()
}

// usageCount returns how many successful chats token sent within the last
// usageWindow, pruning expired timestamps. Feeds the dashboard messages_24h
// display; upstream quota/429 is the enforcement.
func (p *Pool) usageCount(token int) int { return p.roster.usageCount(token) }

// dayRequestCount returns how many successful chats token sent in the
// current Pacific day, rolling the bucket at Pacific midnight. Read by the
// dashboard per-day display, the maturity client-active skip, and maturity
// touch guards; upstream quota/429 is the enforcement.
func (p *Pool) dayRequestCount(token int) int { return p.roster.dayRequestCount(token) }
