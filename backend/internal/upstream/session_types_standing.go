package upstream

import "time"

// FreeWindowsInfo mirrors upstream FreebuffFreeWindowsInfo (free-tier
// session pool windows; display-only).
type FreeWindowsInfo struct {
	DayUsed      float64   `json:"dayUsed"`
	DayLimit     float64   `json:"dayLimit"`
	WeekUsed     float64   `json:"weekUsed"`
	WeekLimit    float64   `json:"weekLimit"`
	MonthUsed    float64   `json:"monthUsed"`
	MonthLimit   float64   `json:"monthLimit"`
	DayResetAt   time.Time `json:"dayResetAt"`
	MonthResetAt time.Time `json:"monthResetAt"`
}

// SessionUpgradeHint mirrors the upstream upgradeHint wire shape
// (common/src/types/freebuff-session.ts:323-326).
type SessionUpgradeHint struct {
	URL     string `json:"url"`
	Message string `json:"message"`
}

// SubscriptionInfo mirrors upstream FreebuffSubscriptionUsage (subscriber
// usage rings + provider spend; rollout audience only).
// MonthSpendUsd/MonthSpendLimitUsd are deprecated legacy fields upstream
// marked @deprecated at abd1eed4a ("new servers omit it"): absent on the
// wire they decode to zero, which callers must read as "not reported".
type SubscriptionInfo struct {
	DayUsed            float64   `json:"dayUsed"`
	DayLimit           float64   `json:"dayLimit"`
	FiveDayUsed        float64   `json:"fiveDayUsed"`
	FiveDayLimit       float64   `json:"fiveDayLimit"`
	MonthUsed          float64   `json:"monthUsed"`
	MonthLimit         float64   `json:"monthLimit"`
	DayPremiumUsed     float64   `json:"dayPremiumUsed"`
	DayPremiumLimit    float64   `json:"dayPremiumLimit"`
	DayResetAt         time.Time `json:"dayResetAt"`
	PeriodEndsAt       time.Time `json:"periodEndsAt"`
	MonthSpendUsd      float64   `json:"monthSpendUsd"`
	MonthSpendLimitUsd float64   `json:"monthSpendLimitUsd"`
	FreeDayUsed        *float64  `json:"freeDayUsed,omitempty"`
	FreeDayLimit       *float64  `json:"freeDayLimit,omitempty"`
}

// SessionReferral mirrors the upstream FreebuffReferralInfo wire block.
type SessionReferral struct {
	Code                    string
	ReferrerName            string
	QualifiedCount          int
	WeeklySessionsRemaining int
	ResetAt                 time.Time
	GithubLinked            bool
}

// SessionStanding is the upstream account standing block (issue #96): the
// pre-join/session response's "standing" field. NextLevelAt is parsed with
// parseFlexTime; zero when the server omits it.
//
// CappedBy/CappedReason name the trust cap holding the account at its level
// (e.g. third_party_client, anonymous_network — reference/freebuff
// freebuff-trust.ts FreebuffStandingInfo), Blurb is the human explanation,
// and NextSteps are the earn-back actions upstream suggests (issue #140).
type SessionStanding struct {
	Level        string
	Label        string
	Score        float64
	NextLevelAt  time.Time
	NextLevel    string
	CappedBy     string
	CappedReason string
	Blurb        string
	NextSteps    []StandingNextStep
}

// StandingNextStep is one suggested trust-earning action
// (FreebuffTrustNextStep, freebuff-trust.ts:415-422).
type StandingNextStep struct {
	ID     string
	Label  string
	Detail string
	Points float64
	Href   string
}

// rawReferral mirrors the session response's "referral" block
// (FreebuffReferralInfo in reference/common/src/types/freebuff-session.ts).
type rawReferral struct {
	Code                    string `json:"code"`
	ReferrerName            string `json:"referrerName"`
	QualifiedCount          int    `json:"qualifiedCount"`
	WeeklySessionsRemaining int    `json:"weeklySessionsRemaining"`
	ResetAt                 any    `json:"resetAt"`
	GithubLinked            bool   `json:"githubLinked"`
}

// rawStanding mirrors the session response's "standing" block (issue #96).
// nextLevelAt is parsed with parseFlexTime.
type rawStanding struct {
	Level        string            `json:"level"`
	Label        string            `json:"label"`
	Score        float64           `json:"score"`
	NextLevelAt  any               `json:"nextLevelAt"`
	NextLevel    string            `json:"nextLevel"`
	CappedBy     string            `json:"cappedBy"`
	CappedReason string            `json:"cappedReason"`
	Blurb        string            `json:"blurb"`
	NextSteps    []rawStandingStep `json:"nextSteps"`
}

// rawStandingStep mirrors one FreebuffTrustNextStep on the wire.
type rawStandingStep struct {
	ID     string  `json:"id"`
	Label  string  `json:"label"`
	Detail string  `json:"detail"`
	Points float64 `json:"points"`
	Href   string  `json:"href"`
}

type rawFreeWindows struct {
	DayUsed      float64 `json:"dayUsed"`
	DayLimit     float64 `json:"dayLimit"`
	WeekUsed     float64 `json:"weekUsed"`
	WeekLimit    float64 `json:"weekLimit"`
	MonthUsed    float64 `json:"monthUsed"`
	MonthLimit   float64 `json:"monthLimit"`
	DayResetAt   any     `json:"dayResetAt"`
	MonthResetAt any     `json:"monthResetAt"`
}

type rawSubscription struct {
	DayUsed            float64  `json:"dayUsed"`
	DayLimit           float64  `json:"dayLimit"`
	FiveDayUsed        float64  `json:"fiveDayUsed"`
	FiveDayLimit       float64  `json:"fiveDayLimit"`
	MonthUsed          float64  `json:"monthUsed"`
	MonthLimit         float64  `json:"monthLimit"`
	DayPremiumUsed     float64  `json:"dayPremiumUsed"`
	DayPremiumLimit    float64  `json:"dayPremiumLimit"`
	DayResetAt         any      `json:"dayResetAt"`
	PeriodEndsAt       any      `json:"periodEndsAt"`
	MonthSpendUsd      float64  `json:"monthSpendUsd"`
	MonthSpendLimitUsd float64  `json:"monthSpendLimitUsd"`
	FreeDayUsed        *float64 `json:"freeDayUsed"`
	FreeDayLimit       *float64 `json:"freeDayLimit"`
}
