// Upstream error classification: maps a status + body to the recovery
// matrix (classifyError) and the body parsers that build the typed errors
// (parseRateLimit/parseBan/parseIpCapped/parseCountryBlock/parseRetryAfter/
// parseFlexTime), plus the bounded-cooldown constants and the class-name
// helper. The quota-ledger policy (rate-limit event counting, window
// derivation, log lines) lives in ratelimit.go; the sentinels and typed
// error values live in errors.go.
package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// classifyError maps an upstream error response to the recovery matrix. It
// matches on the WireCode vocabulary defined in wirecodes.go.
func classifyError(status int, body string, hdr http.Header) error {
	lower := strings.ToLower(body)
	retryAfter := parseRetryAfter(hdr)

	switch {
	case status == http.StatusForbidden && strings.Contains(lower, `"status":"banned"`):
		// The canonical ban body is {"status":"banned"} (the free-session
		// status wire shape, reference/freebuff freebuff-session.ts). Match
		// the marker exactly: any 403 whose body merely mentions the word
		// "banned" (e.g. {"error":"model temporarily banned..."}) must stay
		// a generic 403, not trigger the ban cooldown.
		return parseBan(body)
	case status == http.StatusForbidden && strings.Contains(lower, `"error":"`+string(WireCodeAccountSuspended)+`"`):
		// Hard-ban shape: 403 {"error":"account_suspended","message":"...
		// suspended due to billing issues."} (reference/freebuff
		// sdk run-cancellation.test.ts:314-359, api/_post.ts:298-307).
		// Same ban class as "status":"banned": the body carries no
		// resumes_at, so BanError.ResumesAt stays zero and CooldownBan
		// treats it as a PERMANENT hard ban (no timed retry — re-contacting
		// a suspended account only generates more 403 signal). Exact marker
		// only: 'account-suspended' or a message merely containing the word
		// must stay a generic 403.
		return parseBan(body)
	case strings.Contains(lower, string(WireCodeDeploymentOutsideHours)):
		// Free tier is outside its operating hours: temporarily unavailable
		// but worth a later retry. Checked before the status-driven 503/429
		// cases because upstream can attach it to any status (reference:
		// freebuff-reverse adapter.go classifies it Retryable by body first).
		return &UpstreamError{Status: status, Body: truncate(body, 500), RetryAfter: retryAfter, Retryable: true}
	case containsAny(lower, string(WireCodeFreeModeRunFanout)):
		// free_mode_run_fanout: the free tier refused the request because the
		// account's concurrent-run counter looked like proxy fanout (upstream
		// common/src/constants/freebuff-spend-ceilings.ts names "proxy fanout"
		// a ban-grade sweep signal). Body-marker driven and status-agnostic —
		// upstream attaches it to a bare {"error":...,"message":"Free mode
		// request rejected."} whose status the default branch turned into a
		// dead 502. It is a CONCURRENCY refusal, not a quota one: it clears
		// as soon as the account's other runs drain, so it gets the bounded
		// load_shedding/peak_hours treatment (distinct Status, no ResetAt,
		// no Period => isQuotaExhaustedError is false, no Pacific-midnight
		// lock) and the token cools for FanoutCooldown so the pool rotates to
		// another token instead of re-feeding the fanout counter.
		ra := clampCooldown(retryAfter)
		if ra <= 0 {
			ra = FanoutCooldown
		}
		return &RateLimitError{
			Status:     string(WireCodeFreeModeRunFanout),
			RetryAfter: ra,
			Body:       truncate(body, 200),
		}
	case containsAny(lower, string(WireCodeFreeModeCapacityDeferred)):
		// Free-tier transient capacity queue: upstream says "your request
		// will be retried automatically" and a same-session retry recovers
		// immediately. Retryable transport-level condition handled under the
		// TRANSIENT_RETRIES budget in ChatCompletions against the SAME
		// lease/session — never a token cooldown, never a session
		// invalidation (reference/freebuff-proxy-hengxin proxy.js:652-668).
		return &CapacityDeferredError{Status: status, Body: truncate(body, 500), RetryAfter: retryAfter}
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %d %s", ErrAuthRejected, status, truncate(body, 200))
	case status == http.StatusServiceUnavailable:
		return &WaitingRoomError{RetryAfter: retryAfter, Detail: truncate(body, 200)}
	case status == http.StatusPaymentRequired:
		return &CreditsError{Status: status, Body: truncate(body, 200)}
	case status == http.StatusConflict && containsAny(lower, string(WireCodeSessionLimitReached)):
		// 409 session_limit_reached: the ACCOUNT is over its concurrent-tab
		// budget, but this session's row is fine (endsTheSession:false).
		// Distinct non-invalid error: the server surfaces 409 and never
		// refreshes/recreates the session
		// (reference/freebuff freebuff-session.ts FREEBUFF_GATE_CODES).
		return &SessionLimitError{Status: status, Body: truncate(body, 200)}
	case status == http.StatusForbidden && strings.Contains(lower, string(WireCodeFreeModeCLIRequired)):
		return fmt.Errorf("%w: %d %s", ErrFreeModeCLIRequired, status, truncate(body, 200))
	case status == http.StatusForbidden && strings.Contains(lower, string(WireCodeCountryBlocked)):
		return parseCountryBlock(body)
	case containsAny(lower, string(WireCodeIpCapped)):
		// 429 ip_capped: too many DISTINCT users on the egress IP.
		// Admission-only — existing sessions keep running, so unlike
		// rate_limited this is NOT tied to a quota reset. Cooldown is
		// bounded by the proxy to retryAfterMs + jitter, with a per-token
		// daily re-admission cap (3rd hit in a rolling window locks until
		// Pacific midnight — #118) (reference/freebuff freebuff-session.ts).
		return parseIpCapped(body, retryAfter)
	case containsAny(lower, string(WireCodeWaitingRoomQueued)):
		// 429 waiting_room_queued: transient admission race — the session
		// row was caught mid-admit (endsTheSession:false). NOT session
		// invalid: the row is fine, so the cached session must not be
		// invalidated or refreshed. Surfaced as 503 waiting_room_queued +
		// Retry-After via the shared WaitingRoomError
		// (reference/freebuff freebuff-session.ts FREEBUFF_GATE_CODES).
		return &WaitingRoomError{RetryAfter: retryAfter, Detail: truncate(body, 200)}
	case containsAny(lower, string(WireCodeWaitingRoomRequired)):
		// 428 waiting_room_required (issue #94): the account must walk the
		// reference pre-session ad-chain + streak flow before the next
		// session create. Own retryable signal (Retry-After honored, no
		// cooldown) — deliberately NOT ErrSessionInvalid: the session row is
		// fine, so nothing must be invalidated (reference
		// freebuff2api-optimized codebuff.py:1048-1074). The body marker is
		// the discriminator (upstream can attach it to 428/429 alike); the
		// Client.classify wrapper records the flag so the pool can fire the
		// gated WAITING_ROOM_CHAIN before the next create.
		return &WaitingRoomRequiredError{RetryAfter: retryAfter, Detail: truncate(body, 200)}
	case containsAny(lower, string(WireCodeSessionModelMismatch)) && containsAny(lower, "limited"):
		// The egress IP cannot serve the requested model (e.g. "Limited free
		// access is only available with DeepSeek V4 Flash or MiMo 2.5." or
		// "model <id> is limited on this IP"). The session row is fine — it
		// stays bound to its admitted model — so this is NOT session-invalid:
		// invalidating would re-admit and burn a daily session slot. The server
		// marks the refusal and the pool registry cools the (egress, model)
		// pairing instead.
		return &LimitedIpError{RetryAfter: retryAfter, Body: truncate(body, 200)}
	case containsAny(lower, string(WireCodeFreeModeInvalidAgentModel)):
		// free_mode_invalid_agent_model: the (agent, model) pair is not in
		// upstream's FREE_MODE_AGENT_MODELS allowlist — historically what a
		// stale registry serving a retired id produces (#121; MiMo 2.5 Pro's
		// second-stage retirement 403s exactly this). The default branch
		// turned it into a dead 502 and retries amplified invisibly — issue
		// #140's escalation-guard target. It is a CONFIG/mismatch refusal,
		// not quota: bounded cooldown (InvalidModelCooldown) so the pool
		// rotates instead of hammering, no ResetAt/Period (no midnight lock),
		// distinct Status so the server can surface it with an operator hint.
		return &RateLimitError{
			Status:     string(WireCodeFreeModeInvalidAgentModel),
			RetryAfter: InvalidModelCooldown,
			Body:       truncate(body, 200),
		}
	case containsAny(lower, string(WireCodeSessionSuperseded)):
		// #119: 409 session_superseded is a TERMINAL gate rejection
		// (endsTheSession:true — another instance took over the account;
		// reference/freebuff freebuff-session.ts FREEBUFF_GATE_CODES).
		// Deliberately NOT ErrSessionInvalid: the server must never
		// auto-reacquire in-request (auto-takeover risks ping-pong) — it
		// surfaces 409 session_superseded and lets the NEXT request re-join
		// fresh (send-message.ts handleFreebuffGateError marks the session
		// superseded and stops polling; use-freebuff-session.ts
		// nextDelayMs returns null).
		return &SessionSupersededError{Status: status, Body: truncate(body, 200)}
	case containsAny(lower,
		string(WireCodeFreebuffUpdateRequired), string(WireCodeSessionExpired),
		string(WireCodeSessionModelMismatch), string(WireCodeModelLocked),
		string(WireCodeFreeModeLegacyLunaAgent), string(WireCodeFreeModeLegacyLuna)):
		return fmt.Errorf("%w: %s%s", ErrSessionInvalid, truncate(body, 200), retryDetail(retryAfter))
	case status == http.StatusBadRequest && containsAny(lower, string(WireCodeRunIDNotFound), string(WireCodeRunIDNotRunning)):
		return fmt.Errorf("%w: %s", ErrRunInvalid, truncate(body, 200))
	case status == http.StatusTooManyRequests && containsAny(lower, string(WireCodeInsufficientQuota), string(WireCodeLimitBurstRate)):
		// #133: upstream load saturation ("The current group's upstream
		// load is saturated, please try again later"). No Retry-After in
		// the body — parseRateLimit would lock the token until Pacific
		// midnight on what is a minutes-scale transient. Bounded cooldown,
		// distinct code, no midnight lock.
		return &RateLimitError{
			Status:     string(WireCodeLoadShedding),
			RetryAfter: LoadShedCooldown,
			Body:       truncate(body, 200),
		}
	case status == http.StatusTooManyRequests && containsAny(lower, string(WireCodePeakHours)):
		// #133: "Usage is temporarily limited during peak hours, when
		// upstream model prices double…". The peak end is unknowable from
		// the body: bounded conservative cooldown instead of locking the
		// token until Pacific midnight (the peak is hours, not a day).
		return &RateLimitError{
			Status:     string(WireCodePeakHoursStatus),
			RetryAfter: PeakHoursCooldown,
			Body:       truncate(body, 200),
		}
	case status == http.StatusTooManyRequests || containsAny(lower, string(WireCodeRateLimited), string(WireCodeSpendLimited)):
		return parseRateLimit(body, parseRetryAfter(hdr))
	default:
		return &UpstreamError{Status: status, Body: truncate(body, 500), RetryAfter: retryAfter}
	}
}

// errClassName names the classified error type for the `upstream response`
// debug line. Wrapped sentinel errors (auth/session/run refusals built
// with fmt.Errorf) fall back to the generic upstream error class.
func errClassName(err error) string {
	switch err.(type) {
	case *RateLimitError:
		return "RateLimitError"
	case *IpCappedError:
		return "IpCappedError"
	case *BanError:
		return "BanError"
	case *CountryBlockedError:
		return "CountryBlockedError"
	case *CreditsError:
		return "CreditsError"
	case *SessionLimitError:
		return "SessionLimitError"
	case *SessionSupersededError:
		return "SessionSupersededError"
	case *LimitedIpError:
		return "LimitedIpError"
	case *CapacityDeferredError:
		return "CapacityDeferredError"
	case *WaitingRoomError:
		return "WaitingRoomError"
	case *WaitingRoomRequiredError:
		return "WaitingRoomRequiredError"
	case *UpstreamError:
		return "UpstreamError"
	}
	if err == nil {
		return ""
	}
	return "UpstreamError"
}

// FanoutCooldown bounds a free_mode_run_fanout refusal: the upstream
// concurrent-run counter clears as the account's other runs drain (seconds,
// not a day), and re-hitting it feeds a ban-grade sweep signal — so the token
// backs off for a minute rather than being locked until Pacific midnight.
// Used only when the refusal carries no Retry-After header.
const FanoutCooldown = 60 * time.Second

// InvalidModelCooldown bounds a free_mode_invalid_agent_model refusal
// (issue #140): the pair is not in the allowlist until the registry
// refreshes, so retrying sooner only amplifies the 403 storm that escalated
// accounts to banned in the v0.11.3 incident. A minute per hit gives the
// live registry refresh time to land while keeping the token available for
// other models. Used only when the refusal carries no Retry-After header.
const InvalidModelCooldown = 60 * time.Second

// LoadShedCooldown bounds a 429 load-saturation refusal (issue #133): the
// upstream sheds load for minutes, not a day, so the token re-probes after
// ~90s instead of being locked until Pacific midnight by the no-timestamp
// parseRateLimit default.
const LoadShedCooldown = 90 * time.Second

// PeakHoursCooldown bounds a 429 peak-hours refusal (issue #133): the peak
// window lasts hours and its end is not in the body; 30 minutes is a
// conservative floor that re-probes long before the daily-cap lock would
// have lifted.
const PeakHoursCooldown = 30 * time.Minute

// opaqueRateLimitBackoff bounds a 429 with no timestamp, no daily-reset
// signal, and no Retry-After header (issue #140): a fully opaque body
// must never lock the token until Pacific midnight over a minutes-scale
// transient, so it gets the same bounded cooldown the other no-timestamp
// refusals get.
const opaqueRateLimitBackoff = 60 * time.Second

// MaxCooldown is the ceiling for any cooldown derived from upstream retry
// fields (retryAfterMs, Retry-After, resetAt): 7 days. Those fields are
// untrusted input; without a ceiling an absurd value could overflow the
// int64-nanosecond duration multiply — wrapping to a multi-year positive
// window (time.Duration(ms)*time.Millisecond wraps for ms >= ~9.2e12) — or
// lock a token for decades on a misbehaving upstream.
const MaxCooldown = 7 * 24 * time.Hour

// CooldownFromMillis converts an upstream retryAfterMs value to a cooldown
// duration clamped to MaxCooldown. The overflow guard runs BEFORE the
// multiply: time.Duration(ms)*time.Millisecond wraps for ms >=
// math.MaxInt64/1e6 (~9.2e12), which would turn an absurd upstream value
// into a (positive) multi-year window. Non-positive values return 0 so
// callers' <=0 fallback logic is unaffected.
func CooldownFromMillis(ms float64) time.Duration {
	if ms <= 0 {
		return 0
	}
	if ms > float64(math.MaxInt64)/float64(time.Millisecond) {
		return MaxCooldown
	}
	return clampCooldown(time.Duration(ms) * time.Millisecond)
}

// cooldownFromSeconds converts an upstream retryAfter (seconds) value to a
// cooldown clamped to MaxCooldown, guarding the float64→duration conversion
// the same way CooldownFromMillis guards the multiply.
func cooldownFromSeconds(sec float64) time.Duration {
	if sec <= 0 {
		return 0
	}
	if sec > float64(math.MaxInt64)/float64(time.Second) {
		return MaxCooldown
	}
	return clampCooldown(time.Duration(sec * float64(time.Second)))
}

// clampCooldown bounds a positive duration to MaxCooldown; non-positive
// durations pass through untouched so callers' <=0 fallback logic is
// unaffected.
func clampCooldown(d time.Duration) time.Duration {
	if d > MaxCooldown {
		return MaxCooldown
	}
	return d
}

// untilResetAt converts a future reset timestamp to a cooldown clamped to
// MaxCooldown. time.Until (time.Time.Sub) is undefined when the difference
// exceeds the int64-nanosecond range (~292 years), so a centuries-out
// timestamp is detected on the unix-seconds difference instead of relying
// on a wrapped Duration.
func untilResetAt(t, now time.Time) time.Duration {
	secs := t.Unix() - now.Unix()
	if secs <= 0 {
		return 0
	}
	if secs > math.MaxInt64/int64(time.Second) {
		return MaxCooldown
	}
	return clampCooldown(time.Duration(secs) * time.Second)
}

// reRetryAfterNs matches "retry after Ns" or "retry after N s" (N = digits).
var reRetryAfterNs = regexp.MustCompile(`retry\s+after\s+(\d+)\s*s`)

// reMinutesLimit matches "N minutes limit" (the free_mode_rate_limited body
// for 30-minute windows).
var reMinutesLimit = regexp.MustCompile(`(\d+)\s+minutes?\s+limit`)

// reHours matches "N hours" for a broader duration fallback.
var reHours = regexp.MustCompile(`(\d+)\s+hours?`)

// reResetAt matches "reset(s) at <ISO-8601>" in the body text.
// Lowercase [tT]/[zZ] because the body is lowercased before matching.
var reResetAt = regexp.MustCompile(`resets?\s+at\s+(\d{4}-\d{2}-\d{2}[tT][\d:.]+[zZ]?)`)

// parseRetryAfterFromText extracts a retry-after duration from plain-text
// body content. Three patterns are tried in order:
//  1. "retry after Ns" — explicit delay from the upstream message.
//  2. "N minutes limit" — the free_mode_rate_limited 30-minute window.
//  3. "N hours" — broader duration fallback.
//
// Returns 0 when none match so callers' <= 0 fallback logic is unaffected.
func parseRetryAfterFromText(text string) time.Duration {
	if m := reRetryAfterNs.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return clampCooldown(time.Duration(n) * time.Second)
		}
	}
	if m := reMinutesLimit.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return clampCooldown(time.Duration(n) * time.Minute)
		}
	}
	if m := reHours.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return clampCooldown(time.Duration(n) * time.Hour)
		}
	}
	return 0
}

// parseResetAtFromText extracts a reset timestamp from plain-text body
// content matching "reset(s) at <ISO-8601>". Returns the zero time when no
// match is found so callers' IsZero() checks are unaffected.
func parseResetAtFromText(text string) time.Time {
	if m := reResetAt.FindStringSubmatch(text); m != nil {
		// The body is already lowercased; RFC3339 requires uppercase T/Z.
		iso := strings.ToUpper(m[1])
		if t, err := time.Parse(time.RFC3339, iso); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseRateLimit builds a RateLimitError from a 429 body, extracting
// retryAfterMs/resetAt/limit/recentCount/period best-effort across multiple
// JSON schemas. Falls back to the Retry-After header; a body with no
// timestamp/period and no header delay is bounded to opaqueRateLimitBackoff,
// except a genuine daily-cap body (resetAt or an at-cap daily/weekly period)
// which locks until the upcoming Pacific midnight (07:00 UTC).
func parseRateLimit(body string, headerRetryAfter time.Duration) error {
	rle := &RateLimitError{Body: truncate(body, 200), RetryAfter: headerRetryAfter}
	lower := strings.ToLower(body)

	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err == nil {
		target := raw
		if errObj, ok := raw["error"].(map[string]any); ok {
			target = errObj
		}

		if ms, ok := getNumber(target, "retryAfterMs", "retry_after_ms"); ok && ms > 0 {
			rle.RetryAfter = CooldownFromMillis(ms)
		} else if sec, ok := getNumber(target, "retryAfter", "retry_after"); ok && sec > 0 {
			rle.RetryAfter = cooldownFromSeconds(sec)
		}

		if t, ok := getTime(target, "resetAt", "reset_at", "resets_at", "resumes_at", "reset"); ok && !t.IsZero() {
			rle.ResetAt = t
		}

		if lim, ok := getNumber(target, "limit"); ok {
			rle.Limit = lim
		}
		if cnt, ok := getNumber(target, "recentCount", "recent_count"); ok {
			rle.RecentCount = cnt
		}
		if mod, ok := target["model"].(string); ok {
			rle.Model = mod
		}
		if st, ok := target["status"].(string); ok {
			rle.Status = st
		}
		if period, ok := target["period"].(string); ok {
			rle.Period = period
		}
	}

	// Text-based fallback: extract retry-after duration from the body
	// text when JSON fields didn't provide one (e.g. "30 minutes limit"
	// in free_mode_rate_limited bodies).
	if rle.RetryAfter <= 0 {
		rle.RetryAfter = parseRetryAfterFromText(lower)
	}

	// Text-based fallback: extract reset timestamp from the body text
	// when JSON fields didn't provide one (e.g. "reset at 2026-08-22T07:00:00Z").
	if rle.ResetAt.IsZero() {
		rle.ResetAt = parseResetAtFromText(lower)
	}

	if !rle.ResetAt.IsZero() && rle.ResetAt.After(time.Now()) {
		if rle.RetryAfter <= 0 {
			rle.RetryAfter = untilResetAt(rle.ResetAt, time.Now())
		}
	} else if rle.RetryAfter <= 0 {
		// No timestamp and no header delay: lock until the next Pacific
		// reset window (07:00 UTC) ONLY when the body signals a genuine
		// daily reset — a parsed resetAt (handled above) or a daily/weekly
		// quota period whose counter is at/over the limit (the daily-cap
		// bodies). Every other opaque 429 gets a bounded backoff so a
		// minutes-scale transient is never treated as a full-day lock
		// (issue #140).
		if IsDailyCapReset(rle) {
			nextReset := NextPacificMidnight()
			rle.ResetAt = nextReset
			rle.RetryAfter = untilResetAt(nextReset, time.Now())
		} else {
			rle.RetryAfter = opaqueRateLimitBackoff
		}
	}

	if rle.RetryAfter <= 0 {
		rle.RetryAfter = 60 * time.Second
	}
	rle.RetryAfter = clampCooldown(rle.RetryAfter)
	// Ledger window, computed after ResetAt/RetryAfter are finalized
	// (the daily-cap fallback above sets ResetAt so the window is "reset"
	// for daily-cap timestamp-less 429s; opaque ones carry just
	// RetryAfter → "retry-after").
	rle.Window = rateLimitWindow(body, rle)
	return rle
}

// IsDailyCapReset reports whether a no-timestamp 429 body signals a genuine
// daily-cap reset: the quota period is pacific_day/pacific_week/
// pacific_month AND the recent counter is at/over the limit (the
// session-quota bodies the CLI serves on daily-cap refusals; monthly added
// in wire drift 2026-09-04, issue #330). Only these lock until the next
// Pacific midnight; truly opaque bodies get opaqueRateLimitBackoff.
func IsDailyCapReset(rle *RateLimitError) bool {
	if rle.Period != "pacific_day" && rle.Period != "pacific_week" && rle.Period != "pacific_month" {
		return false
	}
	return rle.Limit > 0 && rle.RecentCount >= rle.Limit
}

// banFromBody builds a BanError from a banned body, extracting the
// resumes_at timestamp best-effort. resumes_at may be RFC3339, unix seconds,
// or unix milliseconds (parseFlexTime). It is the single constructor for both
// the classification matrix (parseBan) and ProbeAccount's status conversion,
// so both sites produce identical typed errors (issue #306).
func banFromBody(body string) *BanError {
	be := &BanError{Body: truncate(body, 200)}
	var parsed struct {
		ResumesAt any    `json:"resumes_at"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		if t, perr := parseFlexTime(parsed.ResumesAt); perr == nil {
			be.ResumesAt = t
		}
	}
	return be
}

// parseBan builds a BanError from a 403 banned body (delegates to banFromBody).
func parseBan(body string) error {
	return banFromBody(body)
}

// countryBlockFromBody builds a CountryBlockedError from a country_blocked
// body, extracting countryCode/countryBlockReason/ipPrivacySignals
// best-effort (absent fields are tolerated). It is the single constructor for
// both the classification matrix (parseCountryBlock) and ProbeAccount's status
// conversion (issue #306).
func countryBlockFromBody(body string) *CountryBlockedError {
	cbe := &CountryBlockedError{}
	var parsed struct {
		CountryCode        string   `json:"countryCode"`
		CountryBlockReason string   `json:"countryBlockReason"`
		IpPrivacySignals   []string `json:"ipPrivacySignals"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		cbe.CountryCode = parsed.CountryCode
		cbe.CountryBlockReason = parsed.CountryBlockReason
		cbe.IpPrivacySignals = parsed.IpPrivacySignals
	}
	return cbe
}

// parseCountryBlock builds a CountryBlockedError from a 403 country_blocked
// body (delegates to countryBlockFromBody).
func parseCountryBlock(body string) error {
	return countryBlockFromBody(body)
}

// parseIpCapped builds an IpCappedError from a 429 ip_capped body,
// extracting retryAfterMs/activeUsersForIp/limit best-effort (absent fields
// are tolerated). The ERROR's retryAfter stays bounded to the body's
// retryAfterMs (1m default) — ip_capped is admission-only and not a quota
// reset upstream, so the parse never fabricates a Pacific-midnight window;
// the proxy's bounded re-admission policy (full retryAfter + jitter, daily
// cap — #118) is applied by runs.CooldownIpCapped at cooldown time.
func parseIpCapped(body string, headerRetryAfter time.Duration) error {
	ice := &IpCappedError{Body: truncate(body, 200), RetryAfter: headerRetryAfter}

	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err == nil {
		target := raw
		if errObj, ok := raw["error"].(map[string]any); ok {
			target = errObj
		}

		if ms, ok := getNumber(target, "retryAfterMs", "retry_after_ms"); ok && ms > 0 {
			ice.RetryAfter = CooldownFromMillis(ms)
		} else if sec, ok := getNumber(target, "retryAfter", "retry_after"); ok && sec > 0 {
			ice.RetryAfter = cooldownFromSeconds(sec)
		}

		if n, ok := getNumber(target, "activeUsersForIp", "active_users_for_ip"); ok {
			ice.ActiveUsersForIP = int(n)
		}
		if lim, ok := getNumber(target, "limit"); ok {
			ice.Limit = lim
		}
	}

	ice.RetryAfter = clampCooldown(ice.RetryAfter)
	if ice.RetryAfter <= 0 {
		ice.RetryAfter = time.Minute
	}
	return ice
}

// isCapacityDeferred reports whether err is a free_mode_capacity_deferred
// response (the free tier's transient capacity queue).
func isCapacityDeferred(err error) bool {
	var cde *CapacityDeferredError
	return errors.As(err, &cde)
}

// parseRetryAfter reads the Retry-After header (seconds or HTTP date).
func parseRetryAfter(hdr http.Header) time.Duration {
	raw := hdr.Get("Retry-After")
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return cooldownFromSeconds(float64(seconds))
	}
	if t, err := http.ParseTime(raw); err == nil {
		return untilResetAt(t, time.Now())
	}
	return 0
}

// parseFlexTime accepts RFC3339, unix seconds, or unix milliseconds.
func parseFlexTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case nil:
		return time.Time{}, errors.New("nil time")
	case string:
		if t == "" {
			return time.Time{}, errors.New("empty time")
		}
		if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return parsed, nil
		}
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed, nil
		}
		if secs, err := strconv.ParseInt(t, 10, 64); err == nil {
			return unixFrom(secs), nil
		}
		return time.Time{}, fmt.Errorf("unparseable time %q", t)
	case float64:
		return unixFrom(int64(t)), nil
	default:
		return time.Time{}, fmt.Errorf("unexpected time type %T", v)
	}
}
