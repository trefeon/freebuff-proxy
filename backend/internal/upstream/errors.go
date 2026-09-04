// Package upstream implements the codebuff.com wire protocol for a single token.
package upstream

import (
	"errors"
	"fmt"
	"time"
)

// Typed error sentinels. Callers use errors.Is against these; the concrete
// error values wrap an UpstreamError where applicable.
var (
	// ErrSessionInvalid: the free session is stale/expired, model-locked, or
	// an update is required. Refresh the session and retry once. (409
	// session_superseded is its OWN terminal sentinel, ErrSessionSuperseded
	// — #119.)
	ErrSessionInvalid = errors.New("upstream session invalid")
	// ErrRunInvalid: the agent run is gone. Rotate the run and retry once.
	ErrRunInvalid = errors.New("upstream run invalid")
	// ErrAuthRejected: 401 — the token is rejected. Cool the token down.
	ErrAuthRejected = errors.New("upstream auth rejected")
	// ErrWaitingRoom: upstream queue. Surface as 503 + Retry-After.
	ErrWaitingRoom = errors.New("upstream waiting room")
	// ErrRateLimited: upstream quota exhausted (429 rate_limited). The token
	// should cool down for RateLimitError.RetryAfter.
	ErrRateLimited = errors.New("upstream rate limited")
	// ErrBanned: the account is temporarily banned upstream (403 {"status":"banned"}).
	// Cool the token down until BanError.ResumesAt.
	ErrBanned = errors.New("upstream account banned")
	// ErrCountryBlocked: free mode is not available from the account's IP
	// region (403 {"status":"country_blocked"}). Surfaced so callers can
	// diagnose the region gate instead of retrying blindly.
	ErrCountryBlocked = errors.New("upstream country blocked")
	// ErrFreeModeCLIRequired: the free tier refused the request because it
	// did not carry the CLI request envelope (403 free_mode_cli_required).
	ErrFreeModeCLIRequired = errors.New("upstream free mode requires CLI request envelope")
	// ErrCredits: 402 payment required — the account has no credits / free
	// quota left to spend.
	ErrCredits = errors.New("upstream payment required")
	// ErrNoActiveSession: a token probe (GET /api/v1/freebuff/session with no
	// instance header) found no active session upstream. The token is still
	// valid — this is the idle state, not a rejection — so health checks
	// surface it as "token OK (no active session)".
	ErrNoActiveSession = errors.New("upstream has no active session")
	// ErrCapacityDeferred: the free tier deferred the request into its
	// capacity queue ("your request will be retried automatically") —
	// empirically common on deepseek-v4-flash. A transient, SAME-session
	// condition: retried against the same lease/session under the
	// TRANSIENT_RETRIES budget, never a token cooldown and never a session
	// invalidation (reference/freebuff-proxy-hengxin proxy.js:652-668 —
	// noCooldown same-session retry).
	ErrCapacityDeferred = errors.New("upstream free capacity deferred")
	// ErrIpCapped: 429 ip_capped — too many DISTINCT users hold an active
	// free session on the egress IP. Admission-only: existing sessions keep
	// running and the request succeeds once one of them ends, so unlike
	// ErrRateLimited it is NOT tied to a quota reset upstream (reference/
	// freebuff freebuff-session.ts). The proxy's CooldownIpCapped adds a
	// bounded re-admission policy (#118): full retryAfter + jitter per hit,
	// with a per-token daily cap (3rd hit in a rolling window) that locks
	// until the Pacific-midnight reset.
	ErrIpCapped = errors.New("upstream ip capped")
	// ErrSessionLimitReached: 409 session_limit_reached — the ACCOUNT is
	// over its concurrent-tab budget; this session's row is fine
	// (endsTheSession:false). Distinct from ErrSessionInvalid so the server
	// surfaces 409 and never refreshes/recreates the session
	// (reference/freebuff freebuff-session.ts FREEBUFF_GATE_CODES).
	ErrSessionLimitReached = errors.New("upstream session limit reached")
	// ErrWaitingRoomRequired: 428 waiting_room_required — the account must
	// walk the reference pre-session flow (request_ad_chain + get_streak)
	// before the next session create (issue #94). endsTheSession:true per
	// FREEBUFF_GATE_CODES (the seat is gone mid-chat), so the server drops
	// the cached session and re-admits once; the WAITING_ROOM_CHAIN gate
	// stays gated — the flag is recorded by Client.classify and the chain
	// fires before the next create. Retryable with Retry-After honored, NO
	// token cooldown, and deliberately DISTINCT from ErrSessionInvalid:
	// the recovery is re-admit-once, never the invalidate+refresh loop
	// (reference/freebuff freebuff-session.ts FREEBUFF_GATE_CODES,
	// send-message.ts handleFreebuffGateError).
	ErrWaitingRoomRequired = errors.New("upstream waiting room required")
	// ErrModelIPLimited: the egress IP cannot serve the requested model
	// (session_model_mismatch + "limited" marker, or the limited_ip session
	// status). The session row is fine — it stays bound to its admitted
	// model — but the request must be retried on a different (egress,
	// model) pairing, so the session must NOT be invalidated/refreshed
	// (that would burn a daily session slot re-admitting).
	ErrModelIPLimited = errors.New("upstream: model limited on egress IP")
	// ErrSessionSuperseded: 409 session_superseded — another client/instance
	// took over the account; this session's row is GONE (endsTheSession:true
	// per FREEBUFF_GATE_CODES). TERMINAL gate rejection: the server must
	// NOT auto-reacquire within the same request (auto-takeover risks
	// ping-pong with the other instance) — it drops the cached row so the
	// NEXT request re-joins fresh and surfaces 409 session_superseded
	// (reference/freebuff freebuff-session.ts FREEBUFF_GATE_CODES,
	// send-message.ts handleFreebuffGateError, use-freebuff-session.ts
	// nextDelayMs returns null for superseded = stop polling).
	ErrSessionSuperseded = errors.New("upstream session superseded")
)

// WaitingRoomError is the concrete value behind ErrWaitingRoom; callers
// unwrap it (errors.As) to surface 503 + Retry-After to the client.
type WaitingRoomError struct {
	Position   int
	QueueDepth int
	RetryAfter time.Duration
	Detail     string
}

func (e *WaitingRoomError) Error() string {
	msg := "upstream waiting room"
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func (e *WaitingRoomError) Unwrap() error { return ErrWaitingRoom }

// WaitingRoomRequiredError is the concrete value behind
// ErrWaitingRoomRequired (issue #94): a 428 waiting_room_required refusal.
// It is retryable (RetryAfter honored) and carries no cooldown; callers
// surface it as 503 + Retry-After.
type WaitingRoomRequiredError struct {
	RetryAfter time.Duration
	Detail     string
}

func (e *WaitingRoomRequiredError) Error() string {
	msg := "upstream waiting room required"
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func (e *WaitingRoomRequiredError) Unwrap() error { return ErrWaitingRoomRequired }

// SessionSupersededError is the concrete value behind ErrSessionSuperseded
// (#119): a 409 session_superseded gate rejection — another instance took
// over the account (endsTheSession:true). Terminal: callers surface it as
// 409 session_superseded and never auto-reacquire in-request.
type SessionSupersededError struct {
	Status int
	Body   string // truncated upstream body
}

func (e *SessionSupersededError) Error() string {
	return fmt.Sprintf("upstream %d: %s", e.Status, e.Body)
}

func (e *SessionSupersededError) Unwrap() error { return ErrSessionSuperseded }

// UpstreamError is a non-recoverable upstream failure surfaced verbatim.
type UpstreamError struct {
	Status     int
	Body       string // truncated to 500 chars
	RetryAfter time.Duration
	// Retryable marks a refusal that is only temporarily unavailable but
	// worth retrying later (e.g. deployment_outside_hours), unlike the
	// default non-retryable UpstreamError.
	Retryable bool
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream %d: %s", e.Status, e.Body)
}

// RateLimitError is a 429 rate_limited response (daily session quota, GLM
// 20h window, ...). RetryAfter comes from the body's retryAfterMs (or the
// Retry-After header when the body is opaque). Unwrap makes
// errors.Is(err, ErrRateLimited) work.
type RateLimitError struct {
	Status      string
	Model       string
	RetryAfter  time.Duration
	Limit       float64
	RecentCount float64
	ResetAt     time.Time
	// Period is the quota period parsed from the body ("pacific_day" /
	// "pacific_week" / "pacific_month"), when present — a reset signal once the
	// recent counter is at/over the limit.
	Period string
	// Window is the ledger window for this refusal (body "1 minute"/
	// "30 minutes" text, else "reset" when ResetAt is set, else
	// "retry-after" when RetryAfter is set, else "none") — reused by the
	// server's `request failed` WARN dedupe.
	Window string
	Body   string // truncated upstream body
}

func (e *RateLimitError) Error() string {
	msg := "upstream rate limited"
	if !e.ResetAt.IsZero() {
		msg += fmt.Sprintf(" (reset at %s)", e.ResetAt.Format(time.RFC3339))
	} else if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// BanError is a temporary account ban (403 {"status":"banned"}). ResumesAt
// is the upstream-provided unban time. Unwrap makes errors.Is(err, ErrBanned) work.
type BanError struct {
	ResumesAt time.Time
	Body      string
}

func (e *BanError) Error() string {
	msg := "upstream account banned"
	if !e.ResumesAt.IsZero() {
		msg += fmt.Sprintf(" (resumes at %s)", e.ResumesAt.Format(time.RFC3339))
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

func (e *BanError) Unwrap() error { return ErrBanned }

// CountryBlockedError is a 403 country_blocked response: free mode is not
// available from the account's IP region. Fields are best-effort — compact
// polls may omit them. Unwrap makes errors.Is(err, ErrCountryBlocked) work.
type CountryBlockedError struct {
	CountryCode        string
	CountryBlockReason string
	IpPrivacySignals   []string
}

func (e *CountryBlockedError) Error() string {
	msg := "upstream country blocked"
	if e.CountryCode != "" {
		msg += " (" + e.CountryCode
		if e.CountryBlockReason != "" {
			msg += ": " + e.CountryBlockReason
		}
		msg += ")"
	}
	return msg
}

func (e *CountryBlockedError) Unwrap() error { return ErrCountryBlocked }

// CreditsError is a 402 payment-required response (no credits / free quota
// left). Mirrors UpstreamError's shape. Unwrap makes
// errors.Is(err, ErrCredits) work.
type CreditsError struct {
	Status int
	Body   string // truncated upstream body
}

func (e *CreditsError) Error() string {
	return fmt.Sprintf("upstream %d: %s", e.Status, e.Body)
}

func (e *CreditsError) Unwrap() error { return ErrCredits }

// CapacityDeferredError is a free_mode_capacity_deferred response: the free
// tier placed the request in its transient capacity queue and retries it
// automatically. The client retries it in-place against the SAME lease and
// session (up to TRANSIENT_RETRIES extra attempts) before surfacing it; it
// is never a token cooldown and never a session invalidation. Unwrap yields
// a Retryable UpstreamError so errors.As finds it, but writeError has a
// dedicated branch: it surfaces as 429 free_mode_capacity_deferred with
// Retry-After once the client-side budget is exhausted (#105) — not a 503.
type CapacityDeferredError struct {
	Status     int
	RetryAfter time.Duration
	Body       string // truncated upstream body
}

func (e *CapacityDeferredError) Error() string {
	return fmt.Sprintf("upstream %d: %s", e.Status, e.Body)
}

// Is makes errors.Is(err, ErrCapacityDeferred) work even though Unwrap
// yields a Retryable UpstreamError (so generic server paths surface 503
// upstream_retryable once the client-side budget is exhausted).
func (e *CapacityDeferredError) Is(target error) bool { return target == ErrCapacityDeferred }

func (e *CapacityDeferredError) Unwrap() error {
	return &UpstreamError{Status: e.Status, Body: e.Body, RetryAfter: e.RetryAfter, Retryable: true}
}

// IpCappedError is a 429 ip_capped response: too many DISTINCT users already
// hold an active free session on this egress IP. Admission-only — existing
// sessions on the IP keep running, and the request succeeds once one of them
// ends — so unlike RateLimitError it is NOT tied to a quota reset upstream.
// RetryAfter comes from the body's retryAfterMs only (reference/freebuff
// freebuff-session.ts). The proxy's CooldownIpCapped applies the bounded
// re-admission policy (#118): full retryAfter + jitter per hit, with a
// per-token daily cap that locks until the Pacific-midnight reset.
// Unwrap makes errors.Is(err, ErrIpCapped) work.
type IpCappedError struct {
	ActiveUsersForIP int
	Limit            float64
	RetryAfter       time.Duration
	Body             string // truncated upstream body
}

func (e *IpCappedError) Error() string {
	msg := "upstream ip capped"
	if e.ActiveUsersForIP > 0 || e.Limit > 0 {
		msg += fmt.Sprintf(" (%d of %v active users on IP)", e.ActiveUsersForIP, e.Limit)
	}
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

func (e *IpCappedError) Unwrap() error { return ErrIpCapped }

// LimitedIpError is the concrete value behind ErrModelIPLimited: the egress
// IP cannot serve the requested model (chat-level session_model_mismatch
// with a "limited" marker, or the limited_ip session status). The session
// row itself is fine — it stays bound to its admitted model — so this must
// never invalidate the session (re-admitting burns a daily session slot).
// RetryAfter comes from the Retry-After header (chat path) or the body's
// retryAfterMs (admission path); it is surfaced to the client but does not
// set the registry window. Unwrap makes errors.Is(err, ErrModelIPLimited)
// work.
type LimitedIpError struct {
	Model      string
	RetryAfter time.Duration
	Body       string // truncated upstream body
}

func (e *LimitedIpError) Error() string {
	msg := "model limited on this egress IP"
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

func (e *LimitedIpError) Unwrap() error { return ErrModelIPLimited }

// SessionLimitError is a 409 session_limit_reached response: the ACCOUNT is
// over its concurrent-tab budget, but this session's row is fine
// (endsTheSession:false). The server surfaces 409 and never refreshes or
// recreates the session. Unwrap makes errors.Is(err, ErrSessionLimitReached)
// work.
type SessionLimitError struct {
	Status int
	Body   string // truncated upstream body
}

func (e *SessionLimitError) Error() string {
	return fmt.Sprintf("upstream %d: %s", e.Status, e.Body)
}

func (e *SessionLimitError) Unwrap() error { return ErrSessionLimitReached }
