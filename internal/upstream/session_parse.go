// Session response parsing: parseSessionResponse (the JSON decode and state
// build behind sessionCall), the per-model quota/standing parser, and the
// availability-window parser (issue #158).
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"freebuff-proxy/internal/stealth"
)

// SessionState is the parsed result of a free-session create/poll.
type SessionState struct {
	Status             string
	InstanceID         string
	Model              string
	CurrentModel       string
	RequestedModel     string
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

// AvailabilityWindow is the parsed daily availability window from a
// model_unavailable admission response's availableHours string (issue #158),
// e.g. "9am ET-5pm PT every day" or "08:00-20:00". Times are normalized to
// minutes since midnight in US Pacific — the reference zone FreeBuff
// sessions and quota windows are Pacific-based — so a skip decision needs no
// DST math at compare time. ET/EST/EDT are converted by subtracting the
// fixed 3-hour ET→PT offset (both observe US DST in lockstep).
type AvailabilityWindow struct {
	// StartMinute/EndMinute bound the daily window, minutes since midnight
	// Pacific. StartMinute == EndMinute means a degenerate/24-7 window:
	// callers must not skip on it.
	StartMinute int
	EndMinute   int
	// Raw is the original availableHours string, for logging.
	Raw string
}

// availableTimeRE matches "H[:MM] [am|pm] [zone]" - "H[:MM] [am|pm] [zone]"
// pairs in an availableHours string. Zones are best-effort 2-3 letter
// tokens (ET/PT/…); unrecognized tails are ignored (the regex is not
// anchored).
var availableTimeRE = regexp.MustCompile(`(?i)(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\s*([a-z]{2,3})?\s*-\s*(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\s*([a-z]{2,3})?`)

// ParseAvailabilityWindow parses an upstream availableHours string into a
// daily window. Supported shapes:
//
//	"08:00-20:00"            24-hour window (interpreted in Pacific)
//	"9am ET-5pm PT every day"  12-hour window with US timezone abbreviations
//
// ok is false when no start/end time pair can be extracted, so callers fall
// back to the plain cache TTL instead of deriving a skip bound.
func ParseAvailabilityWindow(s string) (AvailabilityWindow, bool) {
	m := availableTimeRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return AvailabilityWindow{}, false
	}
	start, ok1 := parseAvailableTime(m[1], m[2], m[3], m[4])
	end, ok2 := parseAvailableTime(m[5], m[6], m[7], m[8])
	if !ok1 || !ok2 {
		return AvailabilityWindow{}, false
	}
	return AvailabilityWindow{StartMinute: start, EndMinute: end, Raw: s}, true
}

// parseAvailableTime converts one time token to minutes since midnight
// Pacific. hour/minute may be 12-hour (with meridiem) or 24-hour; the zone
// token converts ET-family zones to Pacific minutes. Unknown/absent zones
// are interpreted directly as Pacific (best-effort — the caller's TTL cap
// bounds any misparse).
func parseAvailableTime(hour, minute, meridiem, zone string) (int, bool) {
	h, err := strconv.Atoi(hour)
	if err != nil || h < 0 || h > 23 {
		return 0, false
	}
	min := 0
	if minute != "" {
		m, err := strconv.Atoi(minute)
		if err != nil || m < 0 || m > 59 {
			return 0, false
		}
		min = m
	}
	switch strings.ToLower(meridiem) {
	case "am":
		if h == 12 {
			h = 0
		}
	case "pm":
		if h < 12 {
			h += 12
		}
	case "":
	default:
		return 0, false
	}
	total := h*60 + min
	switch strings.ToUpper(zone) {
	case "ET", "EST", "EDT":
		total -= 3 * 60
	case "PT", "PST", "PDT", "":
	default:
		// Unknown zone: keep as Pacific (best-effort).
	}
	total %= 1440
	if total < 0 {
		total += 1440
	}
	return total, true
}

// pacificLoc returns America/Los_Angeles, falling back to a fixed PDT zone
// when the tz database is unavailable (mirrors pool/spend.go's loc helper).
var pacificLoc = sync.OnceValue(func() *time.Location {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return loc
	}
	return time.FixedZone("PDT", -7*60*60)
})

// AvailableAt reports whether minute-of-day (Pacific) falls inside the
// window. A degenerate window (StartMinute == EndMinute) is always
// available: the caller must not skip on it.
func (w AvailabilityWindow) AvailableAt(now time.Time) bool {
	if w.StartMinute == w.EndMinute {
		return true
	}
	m := now.In(pacificLoc()).Hour()*60 + now.In(pacificLoc()).Minute()
	if w.StartMinute < w.EndMinute {
		return m >= w.StartMinute && m < w.EndMinute
	}
	// Overnight window (e.g. 22:00-06:00): open from start until midnight,
	// then from midnight until end.
	return m >= w.StartMinute || m < w.EndMinute
}

// NextStart returns the next wall-clock instant the window opens, strictly
// after now. For a degenerate window it returns now (no future opening to
// wait for).
func (w AvailabilityWindow) NextStart(now time.Time) time.Time {
	loc := pacificLoc()
	t := now.In(loc)
	m := t.Hour()*60 + t.Minute()
	if w.StartMinute == w.EndMinute {
		return now
	}
	start := time.Date(t.Year(), t.Month(), t.Day(), w.StartMinute/60, w.StartMinute%60, 0, 0, loc)
	if m < w.StartMinute {
		return start
	}
	// The window already opened today (and we are outside it — a refusal
	// was cached, so the upstream disagrees with our parse): the next
	// opening is tomorrow.
	return start.AddDate(0, 0, 1)
}

// SessionStanding is the upstream account standing block (issue #96): the
// pre-join/session response's "standing" field. NextLevelAt is parsed with
// parseFlexTime; zero when the server omits it.
//
// CappedBy/CappedReason name the trust cap holding the account at its level
// (e.g. third_party_client, anonymous_network — reference/freebuff
// freebuff-trust.ts FreebuffStandingInfo), Blurb is the human explanation,
// and NextSteps are the earn-back actions upstream suggests (issue #140 P3d).
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
	Period      string // "pacific_day" | "pacific_week" (empty when absent)
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
	}
	if err := json.Unmarshal([]byte(body), &raw); err == nil && raw.Status != "" {
		state := &SessionState{
			Status:             raw.Status,
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
			ActiveUsersForIP:   raw.ActiveUsersForIP,
			Limit:              raw.Limit,
			RecentCount:        raw.RecentCount,
			RetryAfterMs:       raw.RetryAfterMs,
			AvailableHours:     raw.AvailableHours,
			Message:            raw.Message,
			GlmPromo:           string(raw.GlmPromo),
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
		// Feed the passive ban-risk engine (#64): ipPrivacySignals and the
		// ip_capped activeUsersForIp/limit arrive on the session admission
		// and probe responses. Read-only — the engine only warns.
		if c.risk != nil && (len(state.IpPrivacySignals) > 0 ||
			state.ActiveUsersForIP > 0 || state.Limit > 0 || state.CountryCode != "") {
			c.risk.Observe(stealth.RiskSample{
				At:               time.Now(),
				Country:          state.CountryCode,
				IPPrivacySignals: state.IpPrivacySignals,
				ActiveUsersForIP: state.ActiveUsersForIP,
				Limit:            state.Limit,
			})
		}
		return state, nil
	}

	if resp.StatusCode >= 400 {
		return nil, c.classify(resp.StatusCode, body, resp.Header)
	}

	return nil, fmt.Errorf("upstream: unparseable session response %q", truncate(body, 200))
}
