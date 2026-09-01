// quotaerr.go — quota-exhaustion classification and bridge quota helpers
// (issue #85/#178). Premium scarce-session protection was removed in the
// session redesign: model switches release the previous slot instead of
// holding it (see session.EnsureSessionForModel).
package pool

import (
	"strings"

	"freebuff-proxy/backend/internal/upstream"
)

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
