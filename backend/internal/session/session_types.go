package session

import (
	"encoding/json"
	"time"

	"freebuff-proxy/backend/internal/upstream"
)

// SessionSnapshot is a lock-free best-effort view of the cached session
// state, for healthz-style reporting (pool.TokenSnapshot).
type SessionSnapshot struct {
	Status        string
	InstanceID    string
	Model         string
	QueuePosition int
	QueueDepth    int
	// RemainingMs is the server-authoritative milliseconds left in the active
	// session (wire remainingMs); 0 when absent.
	RemainingMs int64
	// Refreshing reports whether a session admission or pre-emptive re-admit
	// is currently in flight for this manager.
	Refreshing bool
	// AccessTier is the upstream access tier ("full", "limited", "free") from
	// the last session admission; "" until reported.
	AccessTier         string
	CountryCode        string
	CountryBlockReason string
	// ActiveUsersForIP is the last known distinct-user count on the token's
	// egress IP (upstream activeUsersForIp); 0 when absent.
	ActiveUsersForIP int
	// IPPrivacySignals is the upstream's own egress-IP classification
	// (e.g. vpn/proxy/tor/hosting); Limit is the ip_capped ceiling. Both
	// feed the passive ban-risk view (#64); empty/0 when absent.
	IPPrivacySignals []string
	Limit            float64
	ExpiresAt        time.Time
	// GracePeriodEndsAt is when the 30-minute drain window after ExpiresAt
	// closes (previously computed but never surfaced).
	GracePeriodEndsAt time.Time
	// QuotaByModel carries the live per-model session quotas (key = model id).
	// Entitlement is a top-level per-token view; it stays empty because the
	// upstream wire nests entitlement inside each rate-limit entry.
	QuotaByModel map[string]QuotaSnapshot
	Entitlement  map[string]float64
	// GlmPromo carries the raw upstream glmPromo block ({dailySessions,
	// endsAt}) from the last admission/poll (issue #178); "" when absent.
	// Kept as a string so callers render the shape without the upstream
	// adding fields.
	GlmPromo string
	// Standing is the upstream account standing block (issue #96); nil until
	// an admission/poll that carried it.
	Standing *upstream.SessionStanding
	// Referral is the upstream referral block (FreebuffReferralInfo); nil
	// until an admission/poll that carried it.
	Referral *upstream.SessionReferral
}

// QuotaSnapshot is one model's live session quota for healthz/metrics
// reporting (pool.TokenSnapshot). Mirrors upstream.ModelQuota.
type QuotaSnapshot struct {
	Model       string
	Limit       float64
	RecentCount float64
	ResetAt     time.Time
	Period      string
	Pool        string
	PoolLabel   string
	Entitlement map[string]float64
}

// Usable reports whether the session can serve a chat right now:
// an active session until expiresAt-5s (the reference safety margin), or
// any session that holds an instance id within its grace drain window.
func (s SessionSnapshot) Usable() bool {
	if s.InstanceID == "" {
		return false
	}
	if s.Status == "active" && !s.ExpiresAt.IsZero() && time.Now().Before(s.ExpiresAt.Add(-expiryMargin)) {
		return true
	}
	return !s.GracePeriodEndsAt.IsZero() && time.Now().Before(s.GracePeriodEndsAt)
}

// MatchesModel reports whether the session matches model (empty model matches any).
func (s SessionSnapshot) MatchesModel(model string) bool {
	return model == "" || s.Model == "" || s.Model == model
}

// HasGlmEntitlement reports whether the session snapshot holds an active
// referral quota or valid unexpired promo for z-ai/glm-5.2 (issue #183).
func (s SessionSnapshot) HasGlmEntitlement() bool {
	if q, ok := s.QuotaByModel["z-ai/glm-5.2"]; ok && q.Limit > 0 {
		if q.RecentCount < q.Limit {
			return true
		}
		if !q.ResetAt.IsZero() && q.ResetAt.Before(time.Now()) {
			return true
		}
	}
	if s.GlmPromo != "" {
		var gp struct {
			DailySessions float64 `json:"dailySessions"`
			EndsAt        string  `json:"endsAt"`
		}
		if err := json.Unmarshal([]byte(s.GlmPromo), &gp); err == nil && gp.DailySessions > 0 {
			if gp.EndsAt == "" {
				return true
			}
			if ends, err := time.Parse(time.RFC3339, gp.EndsAt); err == nil {
				if ends.After(time.Now()) {
					return true
				}
			} else {
				return true
			}
		}
	}
	if s.Entitlement != nil {
		if s.Entitlement["glm"] > 0 || s.Entitlement["z-ai/glm-5.2"] > 0 {
			return true
		}
	}
	return false
}
