package dashboard

import (
	"fmt"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/upstream"
	"sort"
	"strconv"
	"strings"
	"time"
)

func cardFromSnapshot(t pool.TokenSnapshot) tokenCard {
	card := tokenCard{
		Index:            t.Token,
		Email:            t.Email,
		AccountID:        t.AccountID,
		SessionStatus:    t.SessionStatus,
		QueuePosition:    t.SessionQueuePosition,
		QueueDepth:       t.SessionQueueDepth,
		ActiveRuns:       t.ActiveRuns,
		Requests:         t.Requests,
		Messages24h:      t.Messages24h,
		DailyLimit:       t.DailyLimit,
		UsagePct:         t.UsagePct,
		RiskLevel:        t.RiskLevel,
		TransientRetries: t.TransientRetries,
		Locked:           t.Locked,
	}
	if !t.CooldownUntil.IsZero() && time.Now().Before(t.CooldownUntil) {
		card.CooldownActive = true
		card.CooldownUntil = t.CooldownUntil.Format(time.RFC3339)
	}
	if t.BanType != "" {
		card.BanType = t.BanType
		if !t.BannedUntil.IsZero() {
			card.BannedUntil = t.BannedUntil.Format(time.RFC3339)
		}
	}
	if t.Standing != nil {
		card.HasStanding = true
		card.StandingLevel = t.Standing.Level
		card.StandingLabel = t.Standing.Label
		card.StandingScore = t.Standing.Score
		card.StandingNextLevel = t.Standing.NextLevel
		if !t.Standing.NextLevelAt.IsZero() {
			card.StandingNextLevelAt = t.Standing.NextLevelAt.Format(time.RFC3339)
		}
		card.StandingCappedBy = t.Standing.CappedBy
		card.StandingCappedReason = t.Standing.CappedReason
		card.StandingBlurb = t.Standing.Blurb
		for _, s := range t.Standing.NextSteps {
			card.StandingNextSteps = append(card.StandingNextSteps, standingStepCard{
				ID: s.ID, Label: s.Label, Detail: s.Detail, Points: s.Points, Href: s.Href,
			})
		}
	}
	if t.Referral != nil {
		card.HasReferral = true
		card.ReferralCode = t.Referral.Code
		card.ReferralQualifiedCount = t.Referral.QualifiedCount
		card.ReferralSessionsLeft = t.Referral.WeeklySessionsRemaining
		card.ReferralGithubLinked = t.Referral.GithubLinked
		if !t.Referral.ResetAt.IsZero() {
			card.ReferralResetAt = t.Referral.ResetAt.Format(time.RFC3339)
		}
	}
	if t.Freebucks != nil {
		card.Freebucks = freebucksCardFromInfo(t.Freebucks)
	}
	if t.FreeWindows != nil {
		card.FreeWindows = freeWindowsCardFromInfo(t.FreeWindows)
	}
	if t.Subscription != nil {
		card.Subscription = subscriptionCardFromInfo(t.Subscription)
	}
	if t.Streak > 0 {
		card.Streak = t.Streak
		card.TodayUsed = t.TodayUsed
		card.LastUsage = t.LastUsageDate
		if !t.StreakUpdatedAt.IsZero() {
			card.StreakUpdatedAt = t.StreakUpdatedAt.Format(time.RFC3339)
		}
	}
	card.AllowedModels = t.AllowedModels
	return card
}

// tokenLiveCard is the hot-poll subset of tokenCard (issue #322): live
// counters and status only. Account-stable fields (email, account_id,
// daily_limit, standing_*, referral_*) ride the once-per-mount full fetch;
// the SPA merges them back by index. Quota-adjacent cards (freebucks, free
// windows, subscription) stay live: they change mid-session.
type tokenLiveCard struct {
	Index            int    `json:"index"`
	SessionStatus    string `json:"session_status"`
	QueuePosition    int    `json:"queue_position"`
	QueueDepth       int    `json:"queue_depth"`
	ActiveRuns       int    `json:"active_runs"`
	Requests         int    `json:"requests"`
	Messages24h      int    `json:"messages_24h"`
	UsagePct         int    `json:"usage_pct"`
	RiskLevel        string `json:"risk_level"`
	CooldownActive   bool   `json:"cooldown_active"`
	CooldownUntil    string `json:"cooldown_until"`
	Locked           bool   `json:"locked"`
	BanType          string `json:"ban_type,omitempty"`
	BannedUntil      string `json:"banned_until,omitempty"`
	TransientRetries int64  `json:"transient_retries"`
	// AllowlistSkips is live (like TransientRetries): every poll refreshes
	// it, so it stays out of the SPA's static cache.
	AllowlistSkips  int64             `json:"allowlist_skips,omitempty"`
	Freebucks       *freebucksCard    `json:"freebucks,omitempty"`
	FreeWindows     *freeWindowsCard  `json:"free_windows,omitempty"`
	Subscription    *subscriptionCard `json:"subscription,omitempty"`
	Streak          int               `json:"streak,omitempty"`
	TodayUsed       bool              `json:"today_used,omitempty"`
	LastUsage       string            `json:"last_usage,omitempty"`
	StreakUpdatedAt string            `json:"streak_updated_at,omitempty"`
}

// liveCardFromSnapshot builds the hot-poll card for one token snapshot.
func liveCardFromSnapshot(t pool.TokenSnapshot) tokenLiveCard {
	card := tokenLiveCard{
		Index:            t.Token,
		SessionStatus:    t.SessionStatus,
		QueuePosition:    t.SessionQueuePosition,
		QueueDepth:       t.SessionQueueDepth,
		ActiveRuns:       t.ActiveRuns,
		Requests:         t.Requests,
		Messages24h:      t.Messages24h,
		UsagePct:         t.UsagePct,
		RiskLevel:        t.RiskLevel,
		TransientRetries: t.TransientRetries,
		Locked:           t.Locked,
	}
	card.AllowlistSkips = t.AllowlistSkips
	if !t.CooldownUntil.IsZero() && time.Now().Before(t.CooldownUntil) {
		card.CooldownActive = true
		card.CooldownUntil = t.CooldownUntil.Format(time.RFC3339)
	}
	if t.BanType != "" {
		card.BanType = t.BanType
		if !t.BannedUntil.IsZero() {
			card.BannedUntil = t.BannedUntil.Format(time.RFC3339)
		}
	}
	if t.Freebucks != nil {
		card.Freebucks = freebucksCardFromInfo(t.Freebucks)
	}
	if t.FreeWindows != nil {
		card.FreeWindows = freeWindowsCardFromInfo(t.FreeWindows)
	}
	if t.Subscription != nil {
		card.Subscription = subscriptionCardFromInfo(t.Subscription)
	}
	if t.Streak > 0 {
		card.Streak = t.Streak
		card.TodayUsed = t.TodayUsed
		card.LastUsage = t.LastUsageDate
		if !t.StreakUpdatedAt.IsZero() {
			card.StreakUpdatedAt = t.StreakUpdatedAt.Format(time.RFC3339)
		}
	}
	return card
}

func freeWindowsCardFromInfo(info *upstream.FreeWindowsInfo) *freeWindowsCard {
	if info == nil {
		return nil
	}
	card := &freeWindowsCard{
		DayUsed:    info.DayUsed,
		DayLimit:   info.DayLimit,
		WeekUsed:   info.WeekUsed,
		WeekLimit:  info.WeekLimit,
		MonthUsed:  info.MonthUsed,
		MonthLimit: info.MonthLimit,
	}
	if !info.DayResetAt.IsZero() {
		card.DayResetAt = info.DayResetAt.Format(time.RFC3339)
	}
	if !info.MonthResetAt.IsZero() {
		card.MonthResetAt = info.MonthResetAt.Format(time.RFC3339)
	}
	return card
}

func subscriptionCardFromInfo(info *upstream.SubscriptionInfo) *subscriptionCard {
	if info == nil {
		return nil
	}
	card := &subscriptionCard{
		DayUsed:            info.DayUsed,
		DayLimit:           info.DayLimit,
		FiveDayUsed:        info.FiveDayUsed,
		FiveDayLimit:       info.FiveDayLimit,
		MonthUsed:          info.MonthUsed,
		MonthLimit:         info.MonthLimit,
		DayPremiumUsed:     info.DayPremiumUsed,
		DayPremiumLimit:    info.DayPremiumLimit,
		MonthSpendUsd:      info.MonthSpendUsd,
		MonthSpendLimitUsd: info.MonthSpendLimitUsd,
		FreeDayUsed:        info.FreeDayUsed,
		FreeDayLimit:       info.FreeDayLimit,
	}
	if !info.DayResetAt.IsZero() {
		card.DayResetAt = info.DayResetAt.Format(time.RFC3339)
	}
	if !info.PeriodEndsAt.IsZero() {
		card.PeriodEndsAt = info.PeriodEndsAt.Format(time.RFC3339)
	}
	return card
}

func freebucksCardFromInfo(info *upstream.FreebucksInfo) *freebucksCard {
	if info == nil {
		return nil
	}
	card := &freebucksCard{
		Balance: info.Balance,
		Daily:   freebucksWindowCardFromWindow(info.Daily),
		PlanID:  info.PlanID,
		Prices:  info.Prices,
	}
	card.Wallet = freebucksWalletCard{
		Balance:      info.Wallet.Balance,
		MonthlyBonus: info.Wallet.MonthlyBonus,
	}
	if !info.Wallet.NextBonusAt.IsZero() {
		card.Wallet.NextBonusAt = info.Wallet.NextBonusAt.Format(time.RFC3339)
	}
	card.Spend = freebucksSpendCard{LimitUsd: info.Spend.LimitUsd}
	if !info.Spend.ResetAt.IsZero() {
		card.Spend.ResetAt = info.Spend.ResetAt.Format(time.RFC3339)
	}
	if info.Monthly != nil {
		m := freebucksWindowCard{
			Limit:     info.Monthly.LimitUsd,
			Spent:     info.Monthly.SpentUsd,
			Remaining: info.Monthly.RemainingUsd,
		}
		if !info.Monthly.ResetAt.IsZero() {
			m.ResetAt = info.Monthly.ResetAt.Format(time.RFC3339)
		}
		if info.Monthly.LimitUsd != 0 {
			m.PercentUsed = info.Monthly.SpentUsd / info.Monthly.LimitUsd * 100
		}
		card.Monthly = &m
	}
	return card
}

func freebucksWindowCardFromWindow(w upstream.FreebucksWindow) freebucksWindowCard {
	card := freebucksWindowCard{
		Limit:     w.Limit,
		Spent:     w.Spent,
		Remaining: w.Remaining,
	}
	if !w.ResetAt.IsZero() {
		card.ResetAt = w.ResetAt.Format(time.RFC3339)
	}
	if w.Limit != 0 {
		card.PercentUsed = w.Spent / w.Limit * 100
	}
	return card
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

// shortKey returns the first 8 chars of a bridge key hash for display (#187).
func shortKey(key string) string {
	if len(key) > 8 {
		return key[:8] + "…"
	}
	return key
}

func formatQuota(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// formatFreebucks returns a human summary of the Freebucks allowance.
// Nil-safe: returns "" when fb is nil.
//
//nolint:unused
func formatFreebucks(fb *freebucksCard) string {
	if fb == nil {
		return ""
	}
	s := fmt.Sprintf("balance %s · daily %s · wallet %s · spend-ceiling $%s",
		formatQuota(fb.Balance),
		formatFreebucksWindow(fb.Daily),
		formatQuota(fb.Wallet.Balance),
		formatQuota(fb.Spend.LimitUsd),
	)
	if fb.PlanID != "" {
		s += " · plan " + fb.PlanID
	}
	return s
}

// formatFreebucksWindow formats one Freebucks window as "spent/limit (remaining left)".
// Zero-limit windows return "spent/limit".
//
//nolint:unused
func formatFreebucksWindow(w freebucksWindowCard) string {
	base := formatQuota(w.Spent) + "/" + formatQuota(w.Limit)
	if w.Limit == 0 {
		return base
	}
	return base + " (" + formatQuota(w.Remaining) + " left, " + formatQuota(w.PercentUsed) + "%)"
}

func formatEntitlement(e map[string]float64) string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+formatQuota(e[k]))
	}
	return strings.Join(parts, ", ")
}

func shortTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("15:04 Jan 2")
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		d = time.Minute
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h >= 24 {
		dd := h / 24
		hr := h % 24
		if hr > 0 {
			return fmt.Sprintf("%dd %dh", dd, hr)
		}
		return fmt.Sprintf("%dd", dd)
	}
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}
