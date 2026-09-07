package upstream

import "time"

// ModelQuota is one model's live session quota from the upstream
// rateLimitsByModel map, per the official CLI wire shape
// (reference/freebuff/common/src/types/freebuff-session.ts).
// Entitlement holds the per-period breakdown (base/referral/streak/promo;
// promo is omitted by default) that sums to Limit when the server emits it.
type ModelQuota struct {
	Model       string
	Limit       float64
	RecentCount float64
	ResetAt     time.Time
	Period      string // "pacific_day" | "pacific_week" | "pacific_month" (empty when absent)
	// Pool / PoolLabel group models that share one session-quota pool on
	// the wire (FreebuffSessionRateLimit). Pool is opaque — group by it, never
	// match on its value; PoolLabel is the server-authored display string.
	Pool        string
	PoolLabel   string
	Entitlement map[string]float64
}

// rawModelQuota mirrors one rateLimitsByModel entry on the wire. resetAt is
// parsed with parseFlexTime (RFC3339, unix seconds, or unix ms); windowHours
// (deprecated) is deliberately not surfaced.
type rawModelQuota struct {
	Model                string             `json:"model"`
	Limit                float64            `json:"limit"`
	RecentCount          float64            `json:"recentCount"`
	Period               string             `json:"period"`
	ResetAt              any                `json:"resetAt"`
	Pool                 string             `json:"pool"`
	PoolLabel            string             `json:"poolLabel"`
	EntitlementBreakdown map[string]float64 `json:"entitlementBreakdown"`
}
