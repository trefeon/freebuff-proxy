package upstream

import "time"

// SessionUpgradeHint mirrors the upstream upgradeHint wire shape
// (common/src/types/freebuff-session.ts:323-326).
type SessionUpgradeHint struct {
	URL     string `json:"url"`
	Message string `json:"message"`
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
// (e.g. third_party_client, anonymous_network — upstream/freebuff
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
