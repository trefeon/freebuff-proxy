// scarce.go — scarce-model allocation and session-quota protection (issue #155).
package pool

import (
	"fmt"
	"strings"
	"time"

	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// scarceSwitchLead is how long before a scarce session expires that a request
// for a DIFFERENT model is allowed to switch away from it. A request arriving
// earlier than this leaves the scarce session running so the irreplaceable
// 1-session/day allocation is not burned.
const scarceSwitchLead = time.Minute

// ScarceSessionError is returned when all eligible tokens are busy running
// active scarce-model sessions (deepseek-v4-pro, gpt-5.6-luna) for a different
// model that cannot be released without burning irreplaceable daily quota.
// Surfaced as 503 Service Unavailable with Retry-After matching the earliest
// expiry time.
type ScarceSessionError struct {
	Model     string
	ExpiresAt time.Time
}

func (e *ScarceSessionError) Error() string {
	return fmt.Sprintf("scarce session (%s) in use until %s", e.Model, e.ExpiresAt.Format(time.RFC3339))
}

// scarceHeld reports whether the token's cached session is an active scarce
// model with more than scarceSwitchLead remaining — a request for a different
// model must not evict or switch away from it.
func scarceHeld(snap session.SessionSnapshot, requested string, scarce map[string]bool) bool {
	if !snap.Usable() || snap.Model == "" || snap.MatchesModel(requested) {
		return false
	}
	if !scarce[snap.Model] {
		return false
	}
	return !snap.ExpiresAt.IsZero() && time.Until(snap.ExpiresAt) > scarceSwitchLead
}

// scarceActive reports whether the token's cached session is an active scarce
// model with any remaining lifetime (greater than 0). Used by bridge idle
// eviction and shutdown teardown to keep the session alive.
func scarceActive(snap session.SessionSnapshot, scarce map[string]bool) bool {
	if !snap.Usable() || snap.Model == "" {
		return false
	}
	if !scarce[snap.Model] {
		return false
	}
	return !snap.ExpiresAt.IsZero() && time.Until(snap.ExpiresAt) > 0
}

// scarceModelSet builds a fast lookup map from the configured scarce models.
func scarceModelSet(models []string) map[string]bool {
	if len(models) == 0 {
		return nil
	}
	out := make(map[string]bool, len(models))
	for _, m := range models {
		if m != "" {
			out[m] = true
		}
	}
	return out
}

// bridgeQuotaRemaining reports the bridge entry's session-quota state for model
// from its last admission (mirrors quotaRemaining in quota.go).

// isQuotaExhaustedError reports whether rle represents a session quota exhaustion
// (recentCount >= limit or local session quota error) as opposed to a transient rate limit.
// Issue #178: a refusal carrying a reset timestamp or a pacific_day/pacific_week
// period is quota-shaped too — those are the quota windows the upstream serves
// on daily/weekly caps, so the pool treats them as per-model quota exhaustion
// and lets the token keep serving its other models.
func isQuotaExhaustedError(rle *upstream.RateLimitError) bool {
	if rle == nil {
		return false
	}
	if !rle.ResetAt.IsZero() || rle.Period == "pacific_day" || rle.Period == "pacific_week" || isDailyCapReset(rle) {
		return true
	}
	if rle.Limit > 0 && rle.RecentCount >= rle.Limit {
		return true
	}
	if rle.Body == "session quota exhausted for model" || strings.Contains(rle.Body, "referral entitlement required") || strings.Contains(rle.Body, "no referral quota") {
		return true
	}
	return false
}

// isDailyCapReset mirrors upstream.isDailyCapReset: a no-timestamp 429 body
// signals a genuine daily-cap reset when the quota period is
// pacific_day/pacific_week AND the recent counter is at/over the limit (the
// session-quota bodies the CLI serves on daily-cap refusals). The pool needs
// its own copy because the upstream helper is unexported.
func isDailyCapReset(rle *upstream.RateLimitError) bool {
	if rle.Period != "pacific_day" && rle.Period != "pacific_week" {
		return false
	}
	return rle.Limit > 0 && rle.RecentCount >= rle.Limit
}
func bridgeQuotaRemaining(entry *bridgeEntry, model string) (known bool, remaining float64, capped bool) {
	snap := entry.session.Snapshot()
	if isReferralGatedModel(model) {
		if !snap.HasGlmEntitlement() {
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
	return false, 0, false
}

// bridgeQuotaCapped reports whether the bridge entry's session quota is capped.
func bridgeQuotaCapped(entry *bridgeEntry, model string) bool {
	_, _, capped := bridgeQuotaRemaining(entry, model)
	return capped
}

// bridgeQuotaLimitError builds the 429 RateLimitError for a quota-capped bridge entry.
func bridgeQuotaLimitError(entry *bridgeEntry, model string) *upstream.RateLimitError {
	snap := entry.session.Snapshot()
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
