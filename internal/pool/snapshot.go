// snapshot.go — pool-wide health snapshots for /healthz and /metrics.
package pool

import (
	"time"

	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// BridgeTokenSnapshot is a dashboard-ready view of one bridge entry (#187).
type BridgeTokenSnapshot struct {
	Key           string                           `json:"key"` // raw client token (hashed for display)
	LastUsed      time.Time                        `json:"last_used"`
	ActiveRuns    int                              `json:"active_runs"`
	Requests      int                              `json:"requests"`
	Locked        bool                             `json:"locked"`
	CooldownUntil time.Time                        `json:"cooldown_until"`
	SessionActive bool                             `json:"session_active"`
	Model         string                           `json:"model"`
	QuotaByModel  map[string]session.QuotaSnapshot `json:"quota_by_model,omitempty"`
	SpendDay      float64                          `json:"spend_day"`
	SpendPct      int                              `json:"spend_pct"`
	// BanType / BannedUntil mirror TokenSnapshot's active-ban view
	// (issues #198/#199): "temporary" (auto-lifts at BannedUntil) vs
	// "hard" (never self-heals); zero values when no ban is active.
	BanType     string    `json:"ban_type,omitempty"`
	BannedUntil time.Time `json:"banned_until,omitempty"`
}

// banView derives the snapshot ban view from a remembered runs ban
// (issues #198/#199). The ban is active while its folded cooldown window
// still applies; the type must be read off BanError.ResumesAt — NOT the
// effective BannedUntil — because runs.CooldownBan folds a hard ban (zero
// ResumesAt) into a 24h safety window. Returns ""/zero when no ban is
// active.
func banView(ban *upstream.BanError, until time.Time) (string, time.Time) {
	if ban == nil || (!until.IsZero() && !time.Now().Before(until)) {
		return "", time.Time{}
	}
	if ban.ResumesAt.IsZero() {
		return "hard", time.Time{}
	}
	return "temporary", ban.ResumesAt
}

// Snapshot returns the per-token healthz view.
func (p *Pool) Snapshot() []TokenSnapshot {
	toks := p.toks.Load()
	out := make([]TokenSnapshot, 0, len(*toks))
	dailyLimit := p.cfg.Load().MaxMessagesPerDay
	spendLimit := p.cfg.Load().MaxSpendPerDay
	for i, tok := range *toks {
		rs := tok.runs.Snapshot()
		ss := tok.session.Snapshot()
		msgs := p.usageCount(i)

		// Region view: the session snapshot carries the last admitted country; an
		// active country-block cooldown overrides it with the remembered
		// block (the session never admitted after a block, so its snapshot
		// would be empty for the blocked country).
		countryCode, countryReason := ss.CountryCode, ss.CountryBlockReason
		if cbe := tok.runs.CountryBlockedError(); cbe != nil {
			if cbe.CountryCode != "" {
				countryCode = cbe.CountryCode
			}
			if cbe.CountryBlockReason != "" {
				countryReason = cbe.CountryBlockReason
			}
		}

		usagePct := 0
		if dailyLimit > 0 {
			usagePct = (msgs * 100) / dailyLimit
			if usagePct > 100 {
				usagePct = 100
			}
		}

		riskLevel := "low"
		switch {
		// Ban is checked first: CooldownBan fills the shared cooldown
		// deadline, so the cooldown case below would otherwise shadow a
		// banned token as "high". The ban risk is gated on the ban window
		// still being active (BannedUntil) so an expired ban does not stay
		// sticky "critical" forever.
		case rs.BanError != nil && time.Now().Before(rs.BannedUntil):
			riskLevel = "critical"
		case !rs.CooldownUntil.IsZero() && time.Now().Before(rs.CooldownUntil):
			riskLevel = "high"
		case dailyLimit > 0 && usagePct >= 90:
			riskLevel = "critical"
		case dailyLimit > 0 && usagePct >= 70:
			riskLevel = "high"
		case msgs > 120:
			riskLevel = "high"
		case (dailyLimit > 0 && usagePct >= 30) || msgs >= 50:
			riskLevel = "moderate"
		}

		spend := p.spendSnapshot(i)

		sessionRemaining := int64(0)
		if ss.Status == "active" && !ss.ExpiresAt.IsZero() {
			if rem := time.Until(ss.ExpiresAt); rem > 0 {
				sessionRemaining = int64(rem.Seconds())
			}
		}

		// Advisory spend ceiling (issue #122): the Pacific-day bucket vs
		// MAX_SPEND_PER_DAY, capped at 100% like UsagePct. Informational only —
		// the upstream $ ceilings are server-enforced.
		spendPct := 0
		if spendLimit > 0 {
			spendPct = int((spend.Day * 100) / spendLimit)
			if spendPct > 100 {
				spendPct = 100
			}
		}
		// Active-ban view for healthz/dashboard consumers (issues #198/#199).
		banType, bannedUntil := banView(rs.BanError, rs.BannedUntil)

		out = append(out, TokenSnapshot{
			Token:                   i,
			CooldownUntil:           rs.CooldownUntil,
			ActiveRuns:              rs.ActiveRuns,
			Requests:                rs.Requests,
			Messages24h:             msgs,
			DailyLimit:              dailyLimit,
			UsagePct:                usagePct,
			RiskLevel:               riskLevel,
			SessionStatus:           ss.Status,
			SessionInstanceID:       ss.InstanceID,
			SessionQueuePosition:    ss.QueuePosition,
			SessionQueueDepth:       ss.QueueDepth,
			SessionModel:            ss.Model,
			SessionRemainingSeconds: sessionRemaining,
			CountryCode:             countryCode,
			CountryBlockReason:      countryReason,
			SessionActiveUsersForIP: ss.ActiveUsersForIP,
			QuotaByModel:            ss.QuotaByModel,
			Entitlement:             ss.Entitlement,
			GlmPromo:                ss.GlmPromo,
			Standing:                ss.Standing,
			Locked:                  tok.locked.Load(),
			TransientRetries:        tok.client.TransientRetries(),
			FingerprintRotations:    tok.client.FingerprintRotations(),
			RateLimitEvents:         tok.client.RateLimitEvents(),
			ModelLocked:             tok.session.ModelLocked(),
			Spend24h:                spend.Rolling24h,
			SpendDay:                spend.Day,
			SpendWeek:               spend.Week,
			SpendMonth:              spend.Month,
			SpendDayStart:           spend.DayStart,
			SpendWeekStart:          spend.WeekStart,
			SpendMonthStart:         spend.MonthStart,
			SpendLimit:              spendLimit,
			SpendPct:                spendPct,
			SpendLimited:            spend.SpendLimited,
			BanType:                 banType,
			BannedUntil:             bannedUntil,
		})
	}
	return out
}

// PoolSnapshot is the pool-wide metrics view: aggregate transient-retry
// counters summed across every fixed token's client, plus the per-token rows
// (same shape as Snapshot). Bridge-mode entries are not counted in the
// per-token rows (they are per-client-token ephemeral slots), but live
// bridge clients' retry/rotation counters are summed in, and RequestsServed
// is mode-independent (every successful upstream chat).
type PoolSnapshot struct {
	TransientRetries     int64
	FingerprintRotations int64
	RequestsServed       uint64
	Tokens               []TokenSnapshot
}

// PoolSnapshot returns the pool-wide snapshot with aggregate counters.
func (p *Pool) PoolSnapshot() PoolSnapshot {
	ps := PoolSnapshot{Tokens: p.Snapshot(), RequestsServed: p.requestsServed.Load()}
	toks := p.toks.Load()
	for _, tok := range *toks {
		ps.TransientRetries += tok.client.TransientRetries()
		ps.FingerprintRotations += tok.client.FingerprintRotations()
	}
	// Live bridge entries: their counters survive while the entry is cached
	// (LRU eviction drops old ones — the view is "recent bridge activity").
	p.bridgeMu.Lock()
	for _, be := range p.bridge {
		ps.TransientRetries += be.client.TransientRetries()
		ps.FingerprintRotations += be.client.FingerprintRotations()
	}
	p.bridgeMu.Unlock()
	return ps
}
