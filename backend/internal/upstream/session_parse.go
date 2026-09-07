// Session response parsing: parseSessionResponse (the JSON decode and state
// build behind sessionCall), the per-model quota/standing parser, and the
// availability-window parser (issue #158).
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// SessionState is the parsed result of a free-session create/poll.
type SessionState struct {
	Status             string
	InstanceID         string
	Model              string
	CurrentModel       string
	RequestedModel     string
	AccessTier         string
	ExpiresAt          time.Time
	AdmittedAt         time.Time
	RemainingMs        int64
	GracePeriodEndsAt  time.Time
	GraceRemainingMs   int64
	Position           int
	QueueDepth         int
	EstimatedWaitMs    int
	PollAt             time.Time
	CountryCode        string
	CountryBlockReason string
	IpPrivacySignals   []string
	ActiveUsersForIP   int
	Limit              float64
	RecentCount        float64
	ResetAt            time.Time
	ResumesAt          time.Time
	RetryAfterMs       int64
	AvailableHours     string
	Message            string
	// WireBody is the raw upstream body the state was parsed from. ProbeAccount
	// uses it to build BanError/CountryBlockedError through the shared
	// banFromBody/countryBlockFromBody constructors (issue #306), so its typed
	// errors match the classification matrix exactly.
	WireBody string
	// UnavailableWindow is the parsed availability window carried by a
	// model_unavailable admission response (issue #158); nil when the
	// response omitted availableHours or the string could not be parsed.
	UnavailableWindow *AvailabilityWindow
	// GlmPromo carries the raw JSON of the upstream glmPromo block
	// ({dailySessions, endsAt}) when the probe/admission response includes
	// it. Kept as a string so callers render the shape without the upstream
	// adding fields; "" when absent.
	GlmPromo string
	// RateLimitsByModel carries the live per-model session quotas from the
	// admission/poll response (key = model id). Absent on compact polls and
	// pre-join (none) responses; never required.
	RateLimitsByModel map[string]ModelQuota
	// Standing is the upstream account standing block (issue #96), parsed
	// from the session response's "standing" field ({level,label,score,
	// nextLevelAt,nextLevel}); nil when the response omits it.
	Standing *SessionStanding
	// Referral is the upstream referral block (FreebuffReferralInfo), parsed
	// from the session response's "referral" field; nil when omitted.
	Referral *SessionReferral
	// Freebucks is the upstream Freebucks allowance block (issue #232),
	// parsed from the session response's "freebucks" field; nil when omitted.
	Freebucks *FreebucksInfo
	// FreeWindows is the upstream free-tier session-pool windows block
	// (day/week/month; issue #319). Display-only upstream (nothing refuses
	// on the week or month yet); nil when the response omits it — quota-
	// exempt accounts, limited access, or older servers.
	FreeWindows *FreeWindowsInfo
	// Subscription is the upstream subscription usage block (day / fiveDay /
	// month windows plus provider spend USD; issue #319). Sent only to
	// callers in the rollout audience; nil otherwise.
	Subscription *SubscriptionInfo
	// UpgradeHint carries the upstream promotional or upgrade broadcast
	// hint ({url, message}) if provided by the session server; nil otherwise.
	UpgradeHint *SessionUpgradeHint
}

// parseSessionResponse decodes a session control response body into a
// SessionState: the 404 create/poll mapping, JSON decode, quota/standing/
// availability-window parsing, and the passive ban-risk feed (#64). Errors
// are classified through the standard matrix.

func (c *Client) parseSessionResponse(req *http.Request, resp *http.Response, body string) (*SessionState, error) {

	if resp.StatusCode == 404 {
		if req.Method == http.MethodPost {
			// A create 404 means no session slot exists upstream.
			return &SessionState{Status: "disabled"}, nil
		}
		// A poll 404 means the session no longer exists upstream (expired or
		// evicted). Treat it as ended so the session manager re-creates it,
		// instead of caching a permanent "disabled" with no expiry.
		return &SessionState{Status: "ended"}, nil
	}

	c.dump("session", req, resp.StatusCode, body)

	var raw struct {
		Status                 string                   `json:"status"`
		InstanceID             string                   `json:"instanceId"`
		Model                  string                   `json:"model"`
		CurrentModel           string                   `json:"currentModel"`
		RequestedModel         string                   `json:"requestedModel"`
		ExpiresAt              any                      `json:"expiresAt"`
		AdmittedAt             any                      `json:"admittedAt"`
		RemainingMs            int64                    `json:"remainingMs"`
		GracePeriodEndsAt      any                      `json:"gracePeriodEndsAt"`
		GracePeriodRemainingMs int64                    `json:"gracePeriodRemainingMs"`
		Position               int                      `json:"position"`
		QueueDepth             int                      `json:"queueDepth"`
		EstimatedWaitMs        int                      `json:"estimatedWaitMs"`
		PollAt                 any                      `json:"pollAt"`
		CountryCode            string                   `json:"countryCode"`
		CountryBlockReason     string                   `json:"countryBlockReason"`
		AccessTier             string                   `json:"accessTier"`
		IpPrivacySignals       []string                 `json:"ipPrivacySignals"`
		ActiveUsersForIP       int                      `json:"activeUsersForIp"`
		Limit                  float64                  `json:"limit"`
		RecentCount            float64                  `json:"recentCount"`
		ResetAt                any                      `json:"resetAt"`
		ResumesAt              any                      `json:"resumes_at"`
		RetryAfterMs           int64                    `json:"retryAfterMs"`
		AvailableHours         string                   `json:"availableHours"`
		Message                string                   `json:"message"`
		GlmPromo               json.RawMessage          `json:"glmPromo"`
		RateLimitsByModel      map[string]rawModelQuota `json:"rateLimitsByModel"`
		Standing               *rawStanding             `json:"standing"`
		Referral               *rawReferral             `json:"referral"`
		Freebucks              *rawFreebucks            `json:"freebucks"`
		FreeWindows            *rawFreeWindows          `json:"freeWindows"`
		Subscription           *rawSubscription         `json:"subscription"`
		UpgradeHint            *struct {
			URL     string `json:"url"`
			Message string `json:"message"`
		} `json:"upgradeHint"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err == nil && raw.Status != "" {
		state := &SessionState{
			Status:             raw.Status,
			WireBody:           body,
			InstanceID:         raw.InstanceID,
			Model:              raw.Model,
			CurrentModel:       raw.CurrentModel,
			RequestedModel:     raw.RequestedModel,
			RemainingMs:        raw.RemainingMs,
			GraceRemainingMs:   raw.GracePeriodRemainingMs,
			Position:           raw.Position,
			QueueDepth:         raw.QueueDepth,
			EstimatedWaitMs:    raw.EstimatedWaitMs,
			CountryCode:        raw.CountryCode,
			CountryBlockReason: raw.CountryBlockReason,
			IpPrivacySignals:   raw.IpPrivacySignals,
			AccessTier:         raw.AccessTier,
			ActiveUsersForIP:   raw.ActiveUsersForIP,
			Limit:              raw.Limit,
			RecentCount:        raw.RecentCount,
			RetryAfterMs:       raw.RetryAfterMs,
			AvailableHours:     raw.AvailableHours,
			Message:            raw.Message,
			GlmPromo:           string(raw.GlmPromo),
		}
		if raw.UpgradeHint != nil && (raw.UpgradeHint.URL != "" || raw.UpgradeHint.Message != "") {
			state.UpgradeHint = &SessionUpgradeHint{
				URL:     raw.UpgradeHint.URL,
				Message: raw.UpgradeHint.Message,
			}
		}
		if raw.Status == "model_unavailable" && raw.AvailableHours != "" {
			if w, ok := ParseAvailabilityWindow(raw.AvailableHours); ok {
				state.UnavailableWindow = &w
			}
		}
		if raw.Standing != nil {
			standing := &SessionStanding{
				Level:        raw.Standing.Level,
				Label:        raw.Standing.Label,
				Score:        raw.Standing.Score,
				NextLevel:    raw.Standing.NextLevel,
				CappedBy:     raw.Standing.CappedBy,
				CappedReason: raw.Standing.CappedReason,
				Blurb:        raw.Standing.Blurb,
			}
			if standing.NextLevelAt, err = parseFlexTime(raw.Standing.NextLevelAt); err != nil {
				standing.NextLevelAt = time.Time{}
			}
			for _, s := range raw.Standing.NextSteps {
				standing.NextSteps = append(standing.NextSteps, StandingNextStep(s))
			}
			state.Standing = standing
		}
		if raw.Referral != nil {
			ref := &SessionReferral{
				Code:                    raw.Referral.Code,
				ReferrerName:            raw.Referral.ReferrerName,
				QualifiedCount:          raw.Referral.QualifiedCount,
				WeeklySessionsRemaining: raw.Referral.WeeklySessionsRemaining,
				GithubLinked:            raw.Referral.GithubLinked,
			}
			if ref.ResetAt, err = parseFlexTime(raw.Referral.ResetAt); err != nil {
				ref.ResetAt = time.Time{}
			}
			state.Referral = ref
		}
		if raw.Freebucks != nil {
			fb := &FreebucksInfo{
				Balance:      raw.Freebucks.Balance,
				Prices:       raw.Freebucks.Prices,
				PriceNotices: raw.Freebucks.PriceNotices,
			}
			if raw.Freebucks.QuotaExempt != nil {
				fb.QuotaExempt = *raw.Freebucks.QuotaExempt
			}
			for _, c := range raw.Freebucks.PriceChanges {
				fb.PriceChanges = append(fb.PriceChanges, FreebucksPriceChange(c))
			}
			// Apply the server's announced schedule at parse time so every
			// consumer (pool meter, dashboard prices) reads effective prices
			// (issue #350 — mirrors freebucksOf applying the schedule).
			ApplyFreebucksPriceChanges(fb, time.Now())
			if raw.Freebucks.PlanID != nil {
				fb.PlanID = *raw.Freebucks.PlanID
			}
			fb.Daily = windowFromRaw(raw.Freebucks.Daily)
			if raw.Freebucks.Wallet != nil {
				fb.Wallet.Balance = raw.Freebucks.Wallet.Balance
				fb.Wallet.MonthlyBonus = raw.Freebucks.Wallet.MonthlyBonus
				if t, terr := parseFlexTime(raw.Freebucks.Wallet.NextBonusAt); terr == nil {
					fb.Wallet.NextBonusAt = t
				}
			}
			if raw.Freebucks.Spend != nil {
				fb.Spend.LimitUsd = raw.Freebucks.Spend.LimitUsd
				if t, terr := parseFlexTime(raw.Freebucks.Spend.ResetAt); terr == nil {
					fb.Spend.ResetAt = t
				}
			}
			if raw.Freebucks.Monthly != nil {
				m := &FreebucksMonthlyAllowance{
					LimitUsd:     raw.Freebucks.Monthly.LimitUsd,
					SpentUsd:     raw.Freebucks.Monthly.SpentUsd,
					RemainingUsd: raw.Freebucks.Monthly.RemainingUsd,
				}
				if t, terr := parseFlexTime(raw.Freebucks.Monthly.ResetAt); terr == nil {
					m.ResetAt = t
				}
				fb.Monthly = m
			}
			state.Freebucks = fb
		}
		if raw.FreeWindows != nil {
			fw := &FreeWindowsInfo{
				DayUsed:    raw.FreeWindows.DayUsed,
				DayLimit:   raw.FreeWindows.DayLimit,
				WeekUsed:   raw.FreeWindows.WeekUsed,
				WeekLimit:  raw.FreeWindows.WeekLimit,
				MonthUsed:  raw.FreeWindows.MonthUsed,
				MonthLimit: raw.FreeWindows.MonthLimit,
			}
			if t, err := parseFlexTime(raw.FreeWindows.DayResetAt); err == nil {
				fw.DayResetAt = t
			}
			if t, err := parseFlexTime(raw.FreeWindows.MonthResetAt); err == nil {
				fw.MonthResetAt = t
			}
			state.FreeWindows = fw
		}
		if raw.Subscription != nil {
			sub := &SubscriptionInfo{
				DayUsed:            raw.Subscription.DayUsed,
				DayLimit:           raw.Subscription.DayLimit,
				FiveDayUsed:        raw.Subscription.FiveDayUsed,
				FiveDayLimit:       raw.Subscription.FiveDayLimit,
				MonthUsed:          raw.Subscription.MonthUsed,
				MonthLimit:         raw.Subscription.MonthLimit,
				DayPremiumUsed:     raw.Subscription.DayPremiumUsed,
				DayPremiumLimit:    raw.Subscription.DayPremiumLimit,
				MonthSpendUsd:      raw.Subscription.MonthSpendUsd,
				MonthSpendLimitUsd: raw.Subscription.MonthSpendLimitUsd,
				FreeDayUsed:        raw.Subscription.FreeDayUsed,
				FreeDayLimit:       raw.Subscription.FreeDayLimit,
			}
			if t, err := parseFlexTime(raw.Subscription.DayResetAt); err == nil {
				sub.DayResetAt = t
			}
			if t, err := parseFlexTime(raw.Subscription.PeriodEndsAt); err == nil {
				sub.PeriodEndsAt = t
			}
			state.Subscription = sub
		}
		if state.ExpiresAt, err = parseFlexTime(raw.ExpiresAt); err != nil {
			state.ExpiresAt = time.Time{}
		}
		if state.AdmittedAt, err = parseFlexTime(raw.AdmittedAt); err != nil {
			state.AdmittedAt = time.Time{}
		}
		if state.GracePeriodEndsAt, err = parseFlexTime(raw.GracePeriodEndsAt); err != nil {
			state.GracePeriodEndsAt = time.Time{}
		}
		if state.PollAt, err = parseFlexTime(raw.PollAt); err != nil {
			state.PollAt = time.Time{}
		}
		if state.ResetAt, err = parseFlexTime(raw.ResetAt); err != nil {
			state.ResetAt = time.Time{}
		}
		if state.ResumesAt, err = parseFlexTime(raw.ResumesAt); err != nil {
			state.ResumesAt = time.Time{}
		}
		if len(raw.RateLimitsByModel) > 0 {
			state.RateLimitsByModel = make(map[string]ModelQuota, len(raw.RateLimitsByModel))
			for modelID, q := range raw.RateLimitsByModel {
				mq := ModelQuota{
					Model:       q.Model,
					Limit:       q.Limit,
					RecentCount: q.RecentCount,
					Period:      q.Period,
					Pool:        q.Pool,
					PoolLabel:   q.PoolLabel,
					Entitlement: q.EntitlementBreakdown,
				}
				if mq.Model == "" {
					mq.Model = modelID
				}
				if resetAt, perr := parseFlexTime(q.ResetAt); perr == nil {
					mq.ResetAt = resetAt
				}
				state.RateLimitsByModel[modelID] = mq
			}
		}
		return state, nil
	}

	if resp.StatusCode >= 400 {
		return nil, c.classify(resp.StatusCode, body, resp.Header)
	}

	return nil, fmt.Errorf("upstream: unparseable session response %q", truncate(body, 200))
}
