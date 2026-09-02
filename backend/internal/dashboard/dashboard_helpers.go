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
	return card
}

func freebucksCardFromInfo(info *upstream.FreebucksInfo) *freebucksCard {
	if info == nil {
		return nil
	}
	return &freebucksCard{
		Balance:       info.Balance,
		Daily:         freebucksWindowCardFromWindow(info.Daily),
		Weekly:        freebucksWindowCardFromWindow(info.Weekly),
		Monthly:       freebucksWindowCardFromWindow(info.Monthly),
		BindingWindow: info.BindingWindow,
		Prices:        info.Prices,
		PlanDaily:     info.PlanDaily,
	}
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
	return fmt.Sprintf("balance %s · daily %s · weekly %s · monthly %s · binding %s",
		formatQuota(fb.Balance),
		formatFreebucksWindow(fb.Daily),
		formatFreebucksWindow(fb.Weekly),
		formatFreebucksWindow(fb.Monthly),
		fb.BindingWindow,
	)
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
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}
