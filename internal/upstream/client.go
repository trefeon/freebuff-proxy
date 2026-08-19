// Package upstream implements the codebuff.com wire client with the CLI
// request envelope required to pass the free-mode gate
// (403 free_mode_cli_required): x-freebuff-* headers, codebuff_metadata,
// provider.data_collection=deny, forced streaming, and the cb_easp stop
// sentinel. Error handling mirrors proxy-freebuff's recovery matrix: typed
// sentinels let callers refresh sessions, rotate runs, or cool down tokens.
package upstream

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	cryptoRand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/net/http2"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/stealth"
	"freebuff-proxy/internal/telemetry"
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
	RetryAfter  time.Duration
	Limit       float64
	RecentCount float64
	ResetAt     time.Time
	// Window is the T7 ledger window for this refusal (body "1 minute"/
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

// SessionState is the parsed result of a free-session create/poll.
type SessionState struct {
	Status             string
	InstanceID         string
	Model              string
	CurrentModel       string
	RequestedModel     string
	ExpiresAt          time.Time
	AdmittedAt         time.Time
	GracePeriodEndsAt  time.Time
	GraceRemainingMs   int64
	Position           int
	QueueDepth         int
	EstimatedWaitMs    int
	PollAt             time.Time
	AccessTier         string // "full" | "limited" (empty when absent)
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
	// GlmPromo carries the raw JSON of the upstream glmPromo block
	// ({dailySessions, endsAt}) when the probe/admission response includes
	// it. Kept as a string so callers render the shape without the upstream
	// adding fields; "" when absent.
	GlmPromo string
	// LimitedModelOffers carries the limited-tier per-model allowances from
	// limitedModelOffers (present on limited-tier admissions, absent on
	// full-tier and compact poll responses; never required).
	LimitedModelOffers []LimitedModelOffer
	// RateLimitsByModel carries the live per-model session quotas from the
	// admission/poll response (key = model id). Absent on compact polls and
	// pre-join (none) responses; never required.
	RateLimitsByModel map[string]ModelQuota
	// Standing is the upstream account standing block (issue #96), parsed
	// from the session response's "standing" field ({level,label,score,
	// nextLevelAt,nextLevel}); nil when the response omits it.
	Standing *SessionStanding
}

// SessionStanding is the upstream account standing block (issue #96): the
// pre-join/session response's "standing" field. NextLevelAt is parsed with
// parseFlexTime; zero when the server omits it.
type SessionStanding struct {
	Level       string
	Label       string
	Score       float64
	NextLevelAt time.Time
	NextLevel   string
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
	EntitlementBreakdown map[string]float64 `json:"entitlementBreakdown"`
}

// rawStanding mirrors the session response's "standing" block (issue #96).
// nextLevelAt is parsed with parseFlexTime.
type rawStanding struct {
	Level       string  `json:"level"`
	Label       string  `json:"label"`
	Score       float64 `json:"score"`
	NextLevelAt any     `json:"nextLevelAt"`
	NextLevel   string  `json:"nextLevel"`
}

// LimitedModelOffer is one model's limited-tier allowance from the upstream
// limitedModelOffers array, per the official CLI wire shape
// (reference/freebuff/common/src/types/freebuff-session.ts). UserResetAt is
// the user-level quota reset; zero when the server omits it.
type LimitedModelOffer struct {
	Model         string
	Remaining     float64
	Total         float64
	UserRemaining float64
	UserResetAt   time.Time
}

// rawLimitedModelOffer mirrors one limitedModelOffers entry on the wire.
// userResetAt is parsed with parseFlexTime.
type rawLimitedModelOffer struct {
	Model         string  `json:"model"`
	Remaining     float64 `json:"remaining"`
	Total         float64 `json:"total"`
	UserRemaining float64 `json:"userRemaining"`
	UserResetAt   any     `json:"userResetAt"`
}

// ChatOptions carries the envelope values for a chat completion request.
type ChatOptions struct {
	Model             string
	RunID             string
	SessionInstanceID string // "" when the session is disabled
	// RequestID is the server's per-request correlation id (D1): the
	// access wrapper mints it once and threads it here so the client's
	// do()/retry log lines (upstream ok/error/transient/retry) share the
	// server's req_id. Never sent upstream.
	RequestID string
	// TraceSessionID is the per-run trace id minted once by the run manager
	// (crypto/rand UUID) and reused across the run's requests, mirroring the
	// CLI (run.ts: previousRun?.traceSessionId ?? randomUUID). Injected as
	// codebuff_metadata["trace_session_id"] when set.
	TraceSessionID string
	// StepNumber is the 1-based per-run agent step counter (CLI parity:
	// llm_step_number is merged on every chat call, String(n);
	// reference/freebuff agent-runtime run-agent-step.ts:1175-1177).
	// Injected as codebuff_metadata["llm_step_number"] when > 0; the run
	// manager sets it per chat call at the server construction sites.
	StepNumber int
}

// Client speaks the codebuff.com wire protocol for a single token.
type Client struct {
	token      string
	tokenIndex int // 0-based index into the pool's token list (0 for bridge clients)
	baseURL    string
	http       *http.Client

	requestTimeout     time.Duration
	sessionCallTimeout time.Duration
	requestJitter      time.Duration
	costMode           string
	userID             string // optional x-freebuff-acting-user-id (ACTING_USER_ID; see New's doc + client.go acting-user comment: only the token's OWN account id is safe)
	debugDump          bool

	// transientRetriesLimit is TRANSIENT_RETRIES: the maximum number of
	// additional attempts after a transient transport failure (0 disables
	// retries entirely). Only transport-level failures (dial/TLS/reset/EOF)
	// retry; classified upstream errors never do.
	transientRetriesLimit int

	// capacityDeferredRetries counts free_mode_capacity_deferred retries
	// served by this client: the free-tier capacity queue is retried
	// in-place against the SAME lease/session, bounded by the
	// TRANSIENT_RETRIES budget (per-request, tracked separately from
	// transient transport retries).
	capacityDeferredRetries atomic.Int64

	// stealthProfile is the active TLS fingerprint. profileMu guards swaps
	// made by the retry loop (rotating the pinned profile before a retry);
	// newRequest and the dialer read it per request/connection. nil means
	// the plain Go transport.
	profileMu      sync.Mutex
	stealthProfile *stealth.Profile

	// http2Upstream negotiates HTTP/2 with the upstream so the TLS ALPN list
	// matches real browsers ("h2,http/1.1") instead of the h1-only list that
	// is itself a JA4 ALPN mismatch (#51). false forces HTTP/1.1.
	http2Upstream bool

	// risk is the passive ban-risk engine fed from session/probe responses
	// (#64). Production always uses stealth.DefaultRiskEngine; nil disables
	// feeding (test seam).
	risk *stealth.RiskEngine

	// Counters surfaced via the pool snapshot for /metrics.
	transientRetries     atomic.Int64 // transient transport failures retried
	fingerprintRotations atomic.Int64 // pinned fingerprint swaps ahead of a retry

	// rateLimitEvents is the T7 rate-limit ledger: upstream rate-limit
	// classifications counted by body code (rate_limited, spend_limited,
	// ip_capped, insufficient_quota, limit_burst_rate,
	// free_mode_rate_limited, ...). rateLimitMu guards the map; values are
	// atomics so snapshot reads never race a concurrent classification.
	rateLimitMu     sync.Mutex
	rateLimitEvents map[string]*atomic.Int64

	// waitingRoomRequired records that the last upstream refusal was a 428
	// waiting_room_required (issue #94): the pre-session ad-chain + streak
	// flow must fire before the next session create (WAITING_ROOM_CHAIN
	// gate). Set by classifyError; consumed (cleared) by the pool's
	// acquire path when the chain fires.
	waitingRoomRequired atomic.Bool

	// authOnly marks a token-less client built by NewForAuth (issue #62):
	// newRequest must never attach auth headers (there is no credential),
	// and the /api/auth/cli/* flow uses its own login-request helper.
	authOnly bool

	// retryBackoff overrides the randomized 200-600ms pre-retry sleep (test
	// seam; nil uses the crypto/rand jitter).
	retryBackoff func() time.Duration
}

// TokenKey returns a stable, non-secret key derived from the client token
// for session-state persistence. The key is a SHA-256 hex digest of the raw
// token, so the token itself never appears in the persisted file.
func (c *Client) TokenKey() string {
	sum := sha256.Sum256([]byte(c.token))
	return hex.EncodeToString(sum[:])
}

// cliUserAgent mirrors the official CLI chat user agent: the pinned
// @codebuff/llm-providers version, NOT the CLI_VERSION knob
// (reference/freebuff model-provider.ts:150; llm-providers package.json
// 1.0.0). The upstream free-tier gate (403 free_mode_cli_required) keys on
// the CLI request envelope (x-freebuff-* headers, codebuff_metadata, forced
// streaming and the cb_easp stop sentinel — see the package comment), but
// the server still fingerprints the UA, and 0.10.7 (the SDK version) is
// never emitted by a real CLI. Every upstream API call (chat + session +
// agent-runs) sends this UA — no browser persona (#108/#109).
const cliUserAgent = "ai-sdk/openai-compatible/1.0.0/codebuff"

// freebuffCliUA is the ads-API request User-Agent, mirroring the installed
// official CLI binary the proxy emulates (reference
// cli/src/hooks/use-gravity-ad.ts getCliAdRequestUserAgent:
// "Freebuff-CLI/<CODEBUFF_CLI_VERSION>"; 0.0.149 = the vendored binary).
const freebuffCliUA = "Freebuff-CLI/0.0.149"

// adUserAgents maps runtime.GOOS to the browser-like Chrome-124 UA sent to
// ad providers for targeting/fraud screening (#124). The CLI ships one entry
// per platform (reference common/src/util/ad-user-agent.ts: darwin/win32/
// linux AD_USER_AGENTS; use-gravity-ad.ts sends it as the body userAgent)
// and warns that native runtime UAs look bot-like to ad networks. The body
// UA must agree with the device block's os (deviceOS): a mixed signal (e.g.
// os:"linux" with a Windows UA) reads as spoofing to ad networks.
var adUserAgents = map[string]string{
	"darwin":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"windows": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"linux":   "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
}

// adBrowserUserAgent returns the platform-consistent ads body UA for the
// host, falling back to the Linux entry exactly like the CLI's
// getAdUserAgent (AD_USER_AGENTS[platformKey] ?? linux).
func adBrowserUserAgent() string {
	if ua, ok := adUserAgents[runtime.GOOS]; ok {
		return ua
	}
	return adUserAgents["linux"]
}

// New builds the client for one token.
func New(token string, cfg *config.Config) (*Client, error) {
	return NewWithIndex(token, 0, cfg)
}

// NewWithIndex builds the client for token at tokenIndex (the token's
// 0-based position in the pool's token list). Egress is always DIRECT: this
// gateway spoofs the official FreeBuff CLI, which has no outbound proxy
// machinery anywhere, and the upstream server hard-blocks proxy/VPN/Tor
// egress — a proxy would only add ban risk.
func NewWithIndex(token string, tokenIndex int, cfg *config.Config) (*Client, error) {
	if token == "" {
		return nil, errors.New("upstream: empty token")
	}
	if cfg == nil {
		return nil, errors.New("upstream: nil config")
	}

	c := &Client{
		token:                 token,
		tokenIndex:            tokenIndex,
		baseURL:               cfg.UpstreamBaseURL,
		requestTimeout:        cfg.RequestTimeout,
		sessionCallTimeout:    cfg.SessionCallTimeout,
		requestJitter:         cfg.RequestJitter,
		costMode:              cfg.CostMode,
		userID:                cfg.ActingUserID,
		debugDump:             cfg.DebugDump,
		transientRetriesLimit: cfg.TransientRetries,
		http2Upstream:         cfg.HTTP2Upstream,
		risk:                  stealth.DefaultRiskEngine,
		rateLimitEvents:       make(map[string]*atomic.Int64),
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	var baseDial func(ctx context.Context, network, addr string) (net.Conn, error)

	var stealthProf *stealth.Profile
	if cfg.TLSFingerprint != "" {
		profile, ok := stealth.Lookup(cfg.TLSFingerprint)
		if !ok {
			return nil, fmt.Errorf("upstream: unknown TLS_FINGERPRINT %q", cfg.TLSFingerprint)
		}
		stealthProf = profile
	}

	// Direct egress only (no proxy support): this gateway spoofs the
	// official FreeBuff CLI, which has no proxy machinery, and the upstream
	// server hard-blocks proxy/VPN/Tor egress. The DefaultTransport clone
	// inherits http.ProxyFromEnvironment; disable it so an operator
	// HTTP_PROXY/HTTPS_PROXY env var never routes upstream traffic through a
	// proxy either (full egress control).
	transport.Proxy = nil

	if stealthProf != nil {
		// Resolve the profile per request (instead of capturing it) so a
		// transient retry can swap the pinned fingerprint without rebuilding
		// the transport: rotateStealthProfileForRetry swaps c.stealthProfile
		// and the next dial picks it up. For auto/random, newRequest resolves
		// a concrete profile and stashes it so the browser headers and the
		// ClientHello always match; dialProfileFor prefers that stash.
		// baseDial is nil on the direct-only path, so the stealth dialer
		// falls back to the default net.Dialer.
		// The ALPN list must match the transport that will speak next: h2
		// when the http2 transport below is registered, h1 otherwise.
		alpn := []string{"http/1.1"}
		if c.http2Upstream {
			alpn = h2ALPN
		}
		transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return stealth.Dialer(c.dialProfileFor(ctx), baseDial, false, alpn)(ctx, network, addr)
		}
	}

	// HTTP/2 upstream (issue #51). Real browsers advertise "h2,http/1.1";
	// forcing h1-only at the TLS layer is itself a JA4 ALPN mismatch. With
	// the stealth profile the stdlib transport cannot dispatch HTTP/2 over a
	// *utls.UConn (its h2 path type-asserts the conn to *tls.Conn), so a
	// dedicated http2.Transport takes over the "https" scheme and dials with
	// the SAME utls dialer (which now advertises h2).
	//
	// KNOWN LIMITATION (documented): the standard http2 transport writes its
	// own SETTINGS/WINDOW_UPDATE frames (order EnablePush, InitialWindowSize,
	// MaxFrameSize, MaxHeaderListSize, HeaderTableSize) and no priority
	// frames — a real Chrome sends its own ordering plus priorities. The
	// values below approximate Chrome's SETTINGS (HEADER_TABLE_SIZE 65536,
	// INITIAL_WINDOW_SIZE 6291456, MAX_HEADER_LIST_SIZE 262144 per
	// reference/tls-client profiles), killing the JA4 ALPN mismatch; exact
	// per-profile SETTINGS-frame fingerprinting is not feasible with the
	// stdlib transport.
	//
	// HTTP2_UPSTREAM=false restores the previous h1-only behavior.
	if c.http2Upstream {
		if stealthProf != nil {
			h2t := &http2.Transport{
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					return stealth.Dialer(c.dialProfileFor(ctx), baseDial, false, h2ALPN)(ctx, network, addr)
				},
				MaxDecoderHeaderTableSize: 65536,   // Chrome SETTINGS_HEADER_TABLE_SIZE
				MaxHeaderListSize:         262_144, // Chrome SETTINGS_MAX_HEADER_LIST_SIZE
			}
			transport.RegisterProtocol("https", h2t)
		} else {
			// Plain Go transport: the stdlib already negotiates HTTP/2 by
			// default (the DefaultTransport clone carries
			// ForceAttemptHTTP2=true, and its bundled h2 transport handles
			// the ALPN dispatch because the TLS handshake is the stdlib's
			// own). HTTP2_UPSTREAM=false forces HTTP/1.1 instead — an empty
			// TLSNextProto map is the documented way to disable h2.
			if !c.http2Upstream {
				transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
			}
		}
	} else if stealthProf == nil {
		// HTTP2_UPSTREAM=false on the plain path: force HTTP/1.1 (the
		// stdlib would otherwise negotiate h2).
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	c.stealthProfile = stealthProf
	c.http = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			// Go strips Authorization/Cookie on cross-host redirects but not
			// x-codebuff-api-key, which carried the same raw token (defensive —
			// newRequest no longer sets it, #107). Drop both when the redirect
			// target is a different host OR downgrades the scheme https->http
			// (same host, plaintext) so the token never leaks to a redirect
			// target; same-scheme same-host redirects (e.g. CDN or bare-host
			// -> www) keep their credentials.
			if !strings.EqualFold(via[0].URL.Host, req.URL.Host) ||
				(strings.EqualFold(via[0].URL.Scheme, "https") && strings.EqualFold(req.URL.Scheme, "http")) {
				req.Header.Del("Authorization")
				req.Header.Del("x-codebuff-api-key")
			}
			return nil
		},
	}
	return c, nil
}

// reqIDKey carries the request correlation id (opts.RequestID) through the
// request context for the do()/retry log lines. The key type is unexported;
// the server threads the same id via ChatOptions.RequestID (its own
// unexported server-side key is separate).
type reqIDKey struct{}

// withReqID returns a context carrying the request correlation id.
func withReqID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqIDKey{}, id)
}

// ReqID returns the request correlation id carried in ctx, or "" when the
// call was not made through ChatCompletions with opts.RequestID set (e.g.
// session/run management calls).
func ReqID(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey{}).(string)
	return id
}

// ChatCompletions POSTs an OpenAI-shaped request to the upstream chat
// endpoint, injecting the CLI envelope, and returns the raw SSE body reader
// on 2xx. On error status it drains (up to 500 chars), classifies, and
// returns a typed error. The returned reader must be closed; closing it
// releases the connection.
func (c *Client) ChatCompletions(ctx context.Context, opts ChatOptions, body []byte) (io.ReadCloser, error) {
	// D1: thread the server's correlation id into the request context so
	// every do()/retry log line for this chat shares the server's req_id.
	if opts.RequestID != "" {
		ctx = withReqID(ctx, opts.RequestID)
	}
	if c.requestJitter > 0 {
		var b [8]byte
		_, _ = cryptoRand.Read(b[:])
		u := binary.BigEndian.Uint64(b[:])
		jitterNano := int64(u % uint64(c.requestJitter))
		timer := time.NewTimer(time.Duration(jitterNano))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}

	enveloped, err := injectEnvelope(body, c.costMode, opts)
	if err != nil {
		return nil, fmt.Errorf("upstream: envelope: %w", err)
	}

	// free_mode_capacity_deferred is the free tier's transient capacity queue:
	// upstream says "your request will be retried automatically" and a
	// same-session retry recovers immediately (empirically common on
	// deepseek-v4-flash). It is retried IN PLACE against the same lease and
	// session (opts are unchanged, so the instance id is reused), bounded by
	// the TRANSIENT_RETRIES budget — never a token cooldown, never a session
	// invalidation (reference/freebuff-proxy-hengxin proxy.js:652-668).
	// capacityDeferredAttempts is the per-request budget: a fresh call starts
	// at zero, so every request gets its own TRANSIENT_RETRIES allowance
	// (review P1 — the client-lifetime atomic only tracks the metric).
	capacityDeferredAttempts := 0
	for {
		req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/chat/completions", enveloped)
		if err != nil {
			return nil, err
		}
		// The streamed response body must stay readable after this call returns,
		// so the request timeout is applied here (not inside do) and released
		// only when the body is closed.
		var cancel context.CancelFunc
		if _, hasDeadline := req.Context().Deadline(); !hasDeadline && c.requestTimeout > 0 {
			reqCtx, cancelFn := context.WithTimeout(req.Context(), c.requestTimeout)
			cancel = cancelFn
			req = req.WithContext(reqCtx)
		}
		req.Header.Set("Accept", "application/json, text/event-stream")
		// The chat POST carries NO x-freebuff-model / x-freebuff-instance-id
		// headers (#106): the official CLI sends exactly Authorization + the
		// ai-sdk UA (+ optional acting-user-id) on chat
		// (reference/freebuff model-provider.ts:146-152); the model and
		// instance id ride only in the body metadata (injectEnvelope).
		if c.userID != "" {
			// The official CLI sends x-freebuff-acting-user-id on every
			// chat call with the account's OWN id, derived from
			// GET /api/v1/me (reference/freebuff sdk/src/run.ts:649-658;
			// sdk/src/impl/model-provider.ts:148-153 — agent-runs
			// START/FINISH carry it too, database.ts:318-320/396-398).
			// The server treats it as a trusted server-to-server header
			// honored only when the request authenticates as the FreeBuff
			// Web service account (reference/freebuff
			// common/src/constants/freebuff-models.ts:1180-1183).
			// ACTING_USER_ID is therefore only safe when it equals the
			// token's own account id; any other value impersonates a
			// foreign user (a possible flag).
			req.Header.Set("x-freebuff-acting-user-id", c.userID)
		}
		resp, _, err := c.do(req, 0)
		if err != nil {
			releaseCancel(cancel)
			return nil, err
		}
		if resp.StatusCode >= 400 {
			bodyText := drainBody(resp.Body)
			_ = resp.Body.Close()
			releaseCancel(cancel)
			c.dump("chat", req, resp.StatusCode, bodyText)
			cerr := c.classify(resp.StatusCode, bodyText, resp.Header)
			if isCapacityDeferred(cerr) && capacityDeferredAttempts < c.transientRetriesLimit {
				capacityDeferredAttempts++
				c.capacityDeferredRetries.Add(1) // lifetime metric
				// #105: the free-tier capacity queue asks the client to WAIT
				// before retrying — the AI SDK absorbs the deferral silently,
				// honoring retry-after with a 10s default
				// (reference/freebuff sdk model-provider.ts:41-49,62-81). Sleep
				// the parsed retry-after (floor 10s) so the same-session retry
				// does not re-POST immediately (amplification); ctx
				// cancellation aborts the sleep like every other upstream wait.
				ra := 10 * time.Second
				var cde *CapacityDeferredError
				if errors.As(cerr, &cde) && cde.RetryAfter > 0 {
					ra = cde.RetryAfter
				}
				timer := time.NewTimer(ra)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				}
				continue
			}
			return nil, cerr
		}
		return &cancelBody{ReadCloser: resp.Body, cancel: cancel}, nil
	}
}

// CreateSession POSTs /api/v1/freebuff/session with no body.
func (c *Client) CreateSession(ctx context.Context) (*SessionState, error) {
	return c.CreateSessionForModel(ctx, "")
}

// CreateSessionForModel POSTs /api/v1/freebuff/session with the requested
// model header. The POST carries NO body and therefore no Content-Type
// (#120): the CLI's session POST is a bare fetch with Authorization + the
// optional x-freebuff-model header only (reference/freebuff
// freebuff-session-api.ts callFreebuffSession, codebuff-api.ts sets
// Content-Type only when body !== undefined).
func (c *Client) CreateSessionForModel(ctx context.Context, model string) (*SessionState, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/freebuff/session", nil)
	if err != nil {
		return nil, err
	}
	if model != "" {
		req.Header.Set("x-freebuff-model", model)
	}
	return c.sessionCall(req)
}

// GetSession polls /api/v1/freebuff/session for the given instance. A poll
// 404 maps to Status "ended" (the session vanished upstream; the session
// manager re-creates it). Only a CREATE 404 maps to "disabled".
func (c *Client) GetSession(ctx context.Context, instanceID string) (*SessionState, error) {
	return c.GetSessionWithOpts(ctx, instanceID, false)
}

// GetSessionWithOpts polls /api/v1/freebuff/session with an optional compact
// response header. There is deliberately NO heartbeat option: the CLI never
// sends x-freebuff-heartbeat (Desktop-only, reference/freebuff
// freebuff-models.ts:1212-1215); liveness comes from the recurring compact
// GET itself (gap #2).
func (c *Client) GetSessionWithOpts(ctx context.Context, instanceID string, compact bool) (*SessionState, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/freebuff/session", nil)
	if err != nil {
		return nil, err
	}
	if instanceID != "" {
		req.Header.Set("x-freebuff-instance-id", instanceID)
	}
	if compact {
		req.Header.Set("x-freebuff-compact-session", "1")
	}
	return c.sessionCall(req)
}

// ProbeAccount validates the token with a zero-cost GET /api/v1/freebuff/session
// that carries NO x-freebuff-instance-id header, so unlike CreateSession it
// claims no session slot and burns none of the daily session allowance. The
// response carries the live per-model quota (RateLimitsByModel) plus the
// account/session state, which callers surface for token checks and doctor
// diagnostics.
//
// A probe 404 maps (via sessionCall) to Status "ended"; that — or a 200 with
// status "ended" — means the token has no active session, returned as
// (nil, ErrNoActiveSession). Terminal refusal statuses the upstream returns
// as session states (403 {"status":"banned"}/{"status":"country_blocked"})
// are converted to the same typed errors the session manager surfaces
// (ErrBanned / ErrCountryBlocked), so probe callers can distinguish a dead
// account from a healthy idle one. All other classifications pass through
// unchanged: 401 → ErrAuthRejected, 429 → ErrRateLimited, transport
// failures as-is. A 200 with any other status (active/queued/disabled/…)
// returns the full *SessionState.
func (c *Client) ProbeAccount(ctx context.Context) (*SessionState, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/freebuff/session", nil)
	if err != nil {
		return nil, err
	}
	// Ask the upstream to include the unused rate limits in the response so
	// the probe observes accessTier/glmPromo/resetAt/rateLimitsByModel for
	// dashboard display without consuming anything (mirrors
	// reference/freebuff2api-netroindonesia/quota.js).
	req.Header.Set("x-freebuff-include-unused-rate-limits", "1")
	state, err := c.sessionCall(req)
	if err != nil {
		return nil, err
	}
	switch state.Status {
	case "ended":
		return nil, ErrNoActiveSession
	case "banned":
		return nil, &BanError{ResumesAt: state.ResumesAt, Body: state.Message}
	case "country_blocked":
		return nil, &CountryBlockedError{
			CountryCode:        state.CountryCode,
			CountryBlockReason: state.CountryBlockReason,
			IpPrivacySignals:   state.IpPrivacySignals,
		}
	}
	return state, nil
}

// EndSession DELETE /api/v1/freebuff/session; 404 is tolerated. The DELETE
// is keyed on the user, not the instance: the CLI releases its slot with
// Authorization only, no x-freebuff-instance-id header (#120,
// reference/freebuff freebuff-session-api.ts releaseFreebuffSlot → DELETE).
func (c *Client) EndSession(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/api/v1/freebuff/session", nil)
	if err != nil {
		return err
	}

	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return err
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	bodyStr := drainBody(resp.Body)
	if resp.StatusCode == 404 {
		return nil // nothing to end
	}
	if resp.StatusCode >= 400 {
		return c.classify(resp.StatusCode, bodyStr, resp.Header)
	}
	return nil
}

// StartRun POSTs /api/v1/agent-runs with action START and returns the run id.
func (c *Client) StartRun(ctx context.Context, agentID string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"action":         "START",
		"agentId":        agentID,
		"ancestorRunIds": []string{},
	})
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/agent-runs", payload)
	if err != nil {
		return "", err
	}
	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return "", err
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	body := drainBody(resp.Body)
	if resp.StatusCode >= 400 {
		return "", c.classify(resp.StatusCode, body, resp.Header)
	}
	var parsed struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return "", fmt.Errorf("upstream: parse START response %q: %w", truncate(body, 200), err)
	}
	if parsed.RunID == "" {
		return "", fmt.Errorf("upstream: START response missing runId: %q", truncate(body, 200))
	}
	return parsed.RunID, nil
}

// RunStep is one agent-run step, batched in memory and sent WITH FINISH
// (issue #114, CLI parity: reference/freebuff/sdk/src/impl/database.ts
// pendingAgentStepSchema — the CLI has NO /steps endpoint, so steps ride
// the FINISH payload). The proxy records one step per completed chat call.
type RunStep struct {
	// ID is a per-step UUID minted at record time.
	ID string `json:"id"`
	// StepNumber is the 1-based per-run step index (sequential 1,2,3…).
	StepNumber int `json:"stepNumber"`
	// Credits is always 0 for the proxy (the upstream account owns spend).
	Credits int `json:"credits,omitempty"`
	// ChildRunIDs is empty for proxy-recorded steps (child runs are
	// separate runs, not steps).
	ChildRunIDs []string `json:"childRunIds,omitempty"`
	// MessageID is the completed chat response id; null when the stream
	// never carried one (the CLI schema allows a null messageId).
	MessageID *string `json:"messageId"`
	// Status mirrors the CLI step lifecycle; proxy-recorded steps are
	// always "completed" (recorded only after a successful chat).
	Status string `json:"status,omitempty"`
	// StartTime is the step start instant, RFC3339Nano UTC.
	StartTime string `json:"startTime"`
}

// FinishRun POSTs /api/v1/agent-runs with action FINISH, reporting the
// run's honest terminal status and its completed steps (issue #114, CLI
// parity: reference/freebuff/sdk/src/impl/database.ts finishAgentRun — the
// full payload is sent in ONE request; there is no /steps endpoint).
// totalSteps is the step count the manager reports (len(steps) preferred,
// falling back to the request count when no steps were recorded);
// errorMessage is omitted when empty and truncated to 5000 runes otherwise,
// exactly like the CLI's truncateString(errorMessage, 5000).
func (c *Client) FinishRun(ctx context.Context, runID, status string, totalSteps int, steps []RunStep, errorMessage string) error {
	if steps == nil {
		steps = []RunStep{}
	}
	payload := map[string]any{
		"action":        "FINISH",
		"runId":         runID,
		"status":        status,
		"totalSteps":    totalSteps,
		"directCredits": 0,
		"totalCredits":  0,
		"steps":         steps,
	}
	if errorMessage != "" {
		payload["errorMessage"] = truncateRunes(errorMessage, 5000)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/agent-runs", body)
	if err != nil {
		return err
	}

	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return err
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	bodyStr := drainBody(resp.Body)
	if resp.StatusCode >= 400 {
		return c.classify(resp.StatusCode, bodyStr, resp.Header)
	}
	return nil
}

// StartChildRun POSTs /api/v1/agent-runs with action START for the
// context-pruner child of parentRunID (issue #91, CLI parity:
// reference/freebuff-reverse .../http.go createChildRun — agentId
// "context-pruner", ancestorRunIds [parent]). The child is created after a
// parent run is STARTed and FINISHed once the parent's session work closes,
// so the upstream run tree stays balanced. Returns the child run id.
func (c *Client) StartChildRun(ctx context.Context, parentRunID string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"action":         "START",
		"agentId":        "context-pruner",
		"ancestorRunIds": []string{parentRunID},
	})
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/agent-runs", payload)
	if err != nil {
		return "", err
	}
	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return "", err
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	body := drainBody(resp.Body)
	if resp.StatusCode >= 400 {
		return "", c.classify(resp.StatusCode, body, resp.Header)
	}
	var parsed struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return "", fmt.Errorf("upstream: parse child START response %q: %w", truncate(body, 200), err)
	}
	if parsed.RunID == "" {
		return "", fmt.Errorf("upstream: child START response missing runId: %q", truncate(body, 200))
	}
	return parsed.RunID, nil
}

// --- internals ---

// sessionCall performs a session control call: parse the JSON body into a
// SessionState; errors are classified through the standard matrix.
func (c *Client) sessionCall(req *http.Request) (*SessionState, error) {
	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return nil, err
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	body := drainBody(resp.Body)
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
		GracePeriodEndsAt      any                      `json:"gracePeriodEndsAt"`
		GracePeriodRemainingMs int64                    `json:"gracePeriodRemainingMs"`
		Position               int                      `json:"position"`
		QueueDepth             int                      `json:"queueDepth"`
		EstimatedWaitMs        int                      `json:"estimatedWaitMs"`
		PollAt                 any                      `json:"pollAt"`
		AccessTier             string                   `json:"accessTier"`
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
		LimitedModelOffers     []rawLimitedModelOffer   `json:"limitedModelOffers"`
		RateLimitsByModel      map[string]rawModelQuota `json:"rateLimitsByModel"`
		Standing               *rawStanding             `json:"standing"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err == nil && raw.Status != "" {
		state := &SessionState{
			Status:             raw.Status,
			InstanceID:         raw.InstanceID,
			Model:              raw.Model,
			CurrentModel:       raw.CurrentModel,
			RequestedModel:     raw.RequestedModel,
			GraceRemainingMs:   raw.GracePeriodRemainingMs,
			Position:           raw.Position,
			QueueDepth:         raw.QueueDepth,
			EstimatedWaitMs:    raw.EstimatedWaitMs,
			AccessTier:         raw.AccessTier,
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
		if raw.Standing != nil {
			standing := &SessionStanding{
				Level:     raw.Standing.Level,
				Label:     raw.Standing.Label,
				Score:     raw.Standing.Score,
				NextLevel: raw.Standing.NextLevel,
			}
			if standing.NextLevelAt, err = parseFlexTime(raw.Standing.NextLevelAt); err != nil {
				standing.NextLevelAt = time.Time{}
			}
			state.Standing = standing
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
		if len(raw.LimitedModelOffers) > 0 {
			state.LimitedModelOffers = make([]LimitedModelOffer, 0, len(raw.LimitedModelOffers))
			for _, o := range raw.LimitedModelOffers {
				offer := LimitedModelOffer{
					Model:         o.Model,
					Remaining:     o.Remaining,
					Total:         o.Total,
					UserRemaining: o.UserRemaining,
				}
				if resetAt, perr := parseFlexTime(o.UserResetAt); perr == nil {
					offer.UserResetAt = resetAt
				}
				state.LimitedModelOffers = append(state.LimitedModelOffers, offer)
			}
		}
		if len(raw.RateLimitsByModel) > 0 {
			state.RateLimitsByModel = make(map[string]ModelQuota, len(raw.RateLimitsByModel))
			for modelID, q := range raw.RateLimitsByModel {
				mq := ModelQuota{
					Model:       q.Model,
					Limit:       q.Limit,
					RecentCount: q.RecentCount,
					Period:      q.Period,
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

// requestProfileKey stashes the concrete stealth profile resolved for one
// request in its context, so the transport dialer builds the ClientHello
// from the SAME profile whose browser headers were applied (auto/random
// must not draw twice — headers and TLS fingerprint would mismatch).
type requestProfileKey struct{}

func withStealthProfile(ctx context.Context, p *stealth.Profile) context.Context {
	return context.WithValue(ctx, requestProfileKey{}, p)
}

func stealthProfileFrom(ctx context.Context) *stealth.Profile {
	if p, ok := ctx.Value(requestProfileKey{}).(*stealth.Profile); ok {
		return p
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("upstream: build %s %s: %w", method, path, err)
	}
	// A bodyless POST/PUT/PATCH is trivially replayable on a transient
	// retry: give it a NoBody GetBody so do()'s TRANSIENT_RETRIES replay
	// works (a nil GetBody silently disables retries, which after #120
	// would break the bodyless session POST's transport-level retry). GETs
	// and DELETEs stay nil-GetBody (never retried — idempotent reads fail
	// fast and the poll loop's own backoff owns them).
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		if body == nil {
			req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.authOnly {
		// Token-less login-flow client (#62/#66): never send an empty
		// credential pair — the /api/auth/cli/* endpoints take the login
		// User-Agent instead (see authLoginRequest).
		req.Header.Del("Authorization")
	}
	// Content-Type only when a body is present (#120): the CLI sets it iff
	// body !== undefined (reference/freebuff codebuff-api.ts:344-346), so a
	// bodyless session POST must not carry it. Chat always has a body.
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The official CLI sends the pinned llm-providers ai-sdk UA on chat and
	// NO browser headers on any API path (bare Bun fetch) (#108/#109 fix
	// option (a)): the utls ClientHello impersonation stays, the browser
	// header persona does not. x-codebuff-api-key is never sent — Bearer is
	// the only credential (#107, reference/freebuff codebuff-api.ts:337-345).
	req.Header.Set("User-Agent", cliUserAgent)
	ctx = req.Context()
	if profile := c.currentStealthProfile(); profile != nil {
		// Resolve the concrete profile ONCE per request and stash it: the
		// dialer reads the stash for the ClientHello, so the TLS fingerprint
		// matches the profile. Pinned profiles resolve to themselves;
		// auto/random get one concrete draw. Only SanitizeHeaders runs here
		// (protective strip of proxy-identifying headers) — the profile's
		// browser headers are deliberately NOT applied to upstream API calls.
		connProf := stealth.GetProfileForConnection(profile)
		ctx = withStealthProfile(ctx, connProf)
		stealth.SanitizeHeaders(req.Header)
	}
	if ctx != req.Context() {
		req = req.WithContext(ctx)
	}
	return req, nil
}

// do executes req, enforcing the given timeout unless ctx already carries an
// earlier deadline. The returned cancel must be released once the caller is
// done with the response BODY: canceling the request context aborts in-flight
// body reads, so it must outlive body streaming. cancel is nil when no
// timeout was applied. Failures are wrapped so errors.Is works both ways.
//
// When TRANSIENT_RETRIES > 0, transport-level failures (dial/TLS handshake/
// reset/EOF) are retried up to that many additional attempts: the body is
// replayed from GetBody on a fresh connection (req.Close), the pinned TLS
// fingerprint is rotated, and a randomized 200-600ms backoff precedes each
// retry. Classified upstream errors (429/403/401, session/run invalids,
// waiting room), any HTTP status >= 400, context cancellation, and requests
// whose body cannot be replayed are NEVER retried.
func (c *Client) do(req *http.Request, timeout time.Duration) (*http.Response, context.CancelFunc, error) {
	ctx := req.Context()
	start := time.Now()
	var cancel context.CancelFunc
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		// The caller already bound the request. The control-call timeout is
		// still honored as an upper bound when it is the TIGHTER of the two:
		// a long caller deadline (e.g. a 15m request timeout) must not
		// silently defeat SessionCallTimeout on session/run control calls.
		if timeout > 0 {
			if remaining := time.Until(deadline); timeout < remaining {
				ctx, cancel = context.WithTimeout(ctx, timeout)
				req = req.WithContext(ctx)
			}
		}
	} else if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		req = req.WithContext(ctx)
	}

	// Capture the body so a transient failure can replay an identical
	// request. nil bodies (GETs) and non-replayable bodies never retry.
	var replayBody func() (io.ReadCloser, error)
	if req.GetBody != nil {
		replayBody = req.GetBody
	}

	for attempt := 1; ; attempt++ {
		resp, err := c.http.Do(req)
		if err == nil {
			if werr := wrapDecompress(resp); werr != nil {
				_ = resp.Body.Close()
				if cancel != nil {
					cancel()
				}
				return nil, nil, fmt.Errorf("upstream: %s %s: %w", req.Method, req.URL.Path, werr)
			}
			if resp.StatusCode >= 400 {
				// T5 wire transparency: error responses are read (2KB cap),
				// logged as `upstream response` (redacted, ≤500 runes), and
				// re-wrapped so the caller's classification parses the same
				// body. Never logged as `upstream ok` — a transport-level
				// 200 and an upstream 429 are different classes of event.
				bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
				_ = resp.Body.Close()
				bodyText := telemetry.RedactSecrets(string(bodyBytes))
				class := errClassName(classifyError(resp.StatusCode, bodyText, resp.Header))
				attrs := []any{
					"method", req.Method, "path", req.URL.Path,
					"status", resp.StatusCode, "ms", time.Since(start).Milliseconds(),
					"class", class,
					"body", truncateRunes(bodyText, 500),
				}
				if reqID := ReqID(ctx); reqID != "" {
					attrs = append(attrs, "req_id", reqID)
				}
				slog.Debug("upstream response", attrs...)
				resp.Body = io.NopCloser(strings.NewReader(bodyText))
				return resp, cancel, nil
			}
			slog.Debug("upstream ok", "method", req.Method, "path", req.URL.Path,
				"status", resp.StatusCode, "ms", time.Since(start).Milliseconds(),
				"req_id", ReqID(ctx))
			return resp, cancel, nil
		}

		// Transient transport failure with attempts remaining: rotate the
		// pinned fingerprint, replay the body on a fresh connection, and
		// retry after a jittered backoff.
		if c.transientRetriesLimit > 0 && attempt <= c.transientRetriesLimit &&
			ctx.Err() == nil && replayBody != nil && isTransient(err) {
			c.rotateStealthProfileForRetry(req)
			body, bodyErr := replayBody()
			if bodyErr != nil {
				slog.Debug("upstream retry aborted: body replay failed",
					"token", c.tokenIndex+1, "attempt", attempt, "err", bodyErr,
					"req_id", ReqID(ctx))
			} else {
				// Count the retry only once the replay succeeded: the counter
				// reflects retries that actually fired, not aborted ones.
				c.transientRetries.Add(1)
				req.Body = body
				req.Close = true // fresh connection for the retry
				slog.Debug("upstream transient failure, retrying",
					"token", c.tokenIndex+1, "attempt", attempt, "reason", err.Error(),
					"path", req.URL.Path, "req_id", ReqID(ctx))
				timer := time.NewTimer(c.retryDelay())
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
				}
				if ctx.Err() == nil {
					continue
				}
				// Context died during the backoff: a retry would fail
				// instantly, surface the context error instead.
				err = ctx.Err()
			}
		}

		slog.Debug("upstream error", "method", req.Method, "path", req.URL.Path,
			"ms", time.Since(start).Milliseconds(), "err", err, "req_id", ReqID(ctx))
		if cancel != nil {
			cancel()
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, nil, context.Canceled
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, nil, fmt.Errorf("%w: %s %s", context.DeadlineExceeded, req.Method, req.URL.Path)
		}
		return nil, nil, fmt.Errorf("upstream: %s %s: %w", req.Method, req.URL.Path, err)
	}
}

// currentStealthProfile returns the active stealth profile (nil = plain Go
// transport). Guarded by profileMu: the retry loop swaps the pinned profile
// to rotate the fingerprint, so readers must take the lock.
func (c *Client) currentStealthProfile() *stealth.Profile {
	c.profileMu.Lock()
	defer c.profileMu.Unlock()
	return c.stealthProfile
}

// dialProfileFor returns the stealth profile the transport dialer should use
// for a connection under ctx. For ProfileAuto/ProfileRandom the concrete
// profile stashed by newRequest wins, so the ClientHello matches the profile
// resolved for that request; a bare context (no stash) resolves per
// connection as before. For pinned profiles the current c.stealthProfile is
// authoritative: the retry loop swaps it ahead of a retry, so the stash
// would be stale.
func (c *Client) dialProfileFor(ctx context.Context) *stealth.Profile {
	profile := c.currentStealthProfile()
	if profile != nil && (profile.ID == stealth.ProfileIDAuto || profile.ID == stealth.ProfileIDRandom) {
		if stashed := stealthProfileFrom(ctx); stashed != nil {
			return stashed
		}
	}
	return profile
}

// h2ALPN is the ALPN list a real browser advertises — the JA4-correct
// fingerprint for HTTP/2 upstreams (#51).
var h2ALPN = []string{"h2", "http/1.1"}

// TransientRetries returns how many transient transport failures were
// retried by this client (pool snapshot /metrics aggregation).
func (c *Client) TransientRetries() int64 { return c.transientRetries.Load() }

// CapacityDeferredRetries returns how many free_mode_capacity_deferred
// retries this client served (same-session retries under the
// TRANSIENT_RETRIES budget, issue #75).
func (c *Client) CapacityDeferredRetries() int64 { return c.capacityDeferredRetries.Load() }

// countRateLimitEvent increments the per-code rate-limit ledger (T7). The
// map entry is created lazily so clients built without the constructor
// (tests, bridge entries) still record safely.
func (c *Client) countRateLimitEvent(code string) {
	c.rateLimitMu.Lock()
	ctr := c.rateLimitEvents[code]
	if ctr == nil {
		ctr = &atomic.Int64{}
		if c.rateLimitEvents == nil {
			c.rateLimitEvents = make(map[string]*atomic.Int64)
		}
		c.rateLimitEvents[code] = ctr
	}
	c.rateLimitMu.Unlock()
	ctr.Add(1)
}

// RateLimitEvents returns a copy of this client's per-code rate-limit
// classification counters (pool snapshot /metrics aggregation).
func (c *Client) RateLimitEvents() map[string]int64 {
	c.rateLimitMu.Lock()
	defer c.rateLimitMu.Unlock()
	out := make(map[string]int64, len(c.rateLimitEvents))
	for code, ctr := range c.rateLimitEvents {
		out[code] = ctr.Load()
	}
	return out
}

// PendingWaitingRoomChain reports whether the client last classified a 428
// waiting_room_required (issue #94) and the pre-session chain has not been
// fired/cleared yet. The pool consults it before a session create when
// WAITING_ROOM_CHAIN is enabled.
func (c *Client) PendingWaitingRoomChain() bool { return c.waitingRoomRequired.Load() }

// ConsumeWaitingRoomChain clears the 428 flag and reports whether it was
// set (so the caller fires the chain exactly once per 428).
func (c *Client) ConsumeWaitingRoomChain() bool { return c.waitingRoomRequired.Swap(false) }

// waitingRoomChainTimeout bounds the whole best-effort pre-session chain so
// a hung upstream never blocks a session create for long.
const waitingRoomChainTimeout = 15 * time.Second

// FireWaitingRoomChain runs the reference pre-session flow (issue #94(b),
// WAITING_ROOM_CHAIN gate): POST /api/v1/ads per configured ad provider,
// then GET /api/v1/freebuff/streak — mirroring freebuff2api-optimized
// codebuff.py _request_ads_and_streak (surface="waiting_room"). Strictly
// best-effort: every failure is logged and swallowed; the caller must never
// depend on it (a gated stub whose real value is keeping the account's
// waiting-room requirement satisfied before the next session create). The
// streak call fires once after the provider loop, matching the reference.
func (c *Client) FireWaitingRoomChain(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, waitingRoomChainTimeout)
	defer cancel()
	for _, provider := range waitingRoomAdProviders {
		if err := c.requestAds(ctx, provider); err != nil {
			slog.Debug("waiting room chain: ads request failed", "provider", provider, "err", err)
		}
	}
	if err := c.getStreak(ctx); err != nil {
		slog.Debug("waiting room chain: streak request failed", "err", err)
	}
}

// waitingRoomAdProviders mirrors the reference default
// (freebuff2api-optimized config.py: ad_providers=("gravity","zeroclick")).
var waitingRoomAdProviders = []string{"gravity", "zeroclick"}

// requestAds POSTs one /api/v1/ads payload (reference cli/src/hooks/
// use-gravity-ad.ts fetchAd + common/src/util/ad-user-agent.ts: provider +
// device block + browser-like body userAgent + Freebuff-CLI header UA).
// Faithful details kept: messages stays [] and sessionId is omitted (the
// chain fires before a session exists — a fresh waiting-room).
func (c *Client) requestAds(ctx context.Context, provider string) error {
	payload := map[string]any{
		"provider": provider,
		"messages": []any{},
		"device": map[string]any{
			"os":       deviceOS(),
			"timezone": egressDeviceTimezone(),
			"locale":   egressDeviceLocale(),
		},
		// Body userAgent: the shared browser-like UA (NOT a runtime UA) so
		// every ad provider sees a usable targeting signal — the CLI sends
		// getAdUserAgent() here (#124).
		"userAgent": adBrowserUserAgent(),
		"surface":   "waiting_room",
	}
	body, _ := json.Marshal(payload)
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/ads", body)
	if err != nil {
		return err
	}
	// Header UA: Freebuff-CLI/<version> (getCliAdRequestUserAgent), NOT the
	// chat ai-sdk UA newRequest set — the CLI's ads POST carries exactly
	// this product UA (#124).
	req.Header.Set("User-Agent", freebuffCliUA)
	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return err
	}
	// do() returns a nil cancel when the context already carried a deadline
	// (the chain's own timeout), so guard the defer.
	if cancel != nil {
		defer cancel()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ads status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}

// deviceOS maps runtime.GOOS to the ads device block's wire contract
// (macos|windows|linux, use-gravity-ad.ts getDeviceInfo platformToOs):
// Go reports "darwin" but the API only accepts "macos", and anything
// unrecognized falls back to "linux" exactly like the CLI.
func deviceOS() string {
	return deviceOSFor(runtime.GOOS)
}

func deviceOSFor(goos string) string {
	switch goos {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	default:
		return "linux"
	}
}

// egressDeviceTimezone returns the host IANA timezone name for the ads
// device block, mirroring the CLI's
// Intl.DateTimeFormat().resolvedOptions().timeZone (use-gravity-ad.ts
// getDeviceInfo). time.Local.String() is the host zone when Go resolved a
// real IANA name; "Local" is Go's placeholder when it could not, so that
// (and anything LoadLocation rejects) falls back to the always-valid "UTC".
func egressDeviceTimezone() string {
	tz := time.Local.String()
	if tz == "" || tz == "Local" {
		return "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return "UTC"
	}
	return tz
}

// egressDeviceLocale returns the host locale for the ads device block,
// derived from LC_ALL/LC_MESSAGES/LANG (POSIX "en_US.UTF-8" → "en-US",
// charset stripped, "_" → "-"), falling back to "en-US" — the CLI's
// Intl.DateTimeFormat().resolvedOptions().locale shape (use-gravity-ad.ts
// getDeviceInfo). "C"/"POSIX" are not real locales and are skipped.
func egressDeviceLocale() string {
	for _, env := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		raw := os.Getenv(env)
		if raw == "" {
			continue
		}
		lang := strings.SplitN(raw, ".", 2)[0]
		lang = strings.ReplaceAll(lang, "_", "-")
		if lang == "" || lang == "C" || lang == "POSIX" {
			continue
		}
		return lang
	}
	return "en-US"
}

// getStreak GETs /api/v1/freebuff/streak (reference
// cli/src/hooks/use-freebuff-streak-query.ts: the request() helper sets NO
// UA override → bun's default). The proxy's equivalent of "no override" is
// newRequest's cliUserAgent (ai-sdk 1.0.0), which is what this request
// inherits — the old 2.0.42 login UA was never a real llm-providers
// version (#124).
func (c *Client) getStreak(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/freebuff/streak", nil)
	if err != nil {
		return err
	}
	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return err
	}
	if cancel != nil {
		defer cancel()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("streak status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}

// FingerprintRotations returns how many times the pinned TLS fingerprint was
// rotated ahead of a retry (pool snapshot /metrics aggregation).
func (c *Client) FingerprintRotations() int64 { return c.fingerprintRotations.Load() }

// SetTransport replaces the HTTP transport backing the client. Exported as a
// test seam for retry-injection tests (substituting a flaky RoundTripper);
// production code never calls it.
func (c *Client) SetTransport(rt http.RoundTripper) { c.http.Transport = rt }

// transientMarkers are transport-level failure signatures that are safe to
// retry: the request never reached the application layer, so no upstream
// quota/credits were burned and nothing was processed. Classified upstream
// errors (429/403/401, session/run invalids, waiting room) and any HTTP
// status >= 400 are handled at the response layer and never enter this path.
// Markers are lowercase: isTransient lowercases the wrapped error messages
// before matching. "tls: handshake failure" is Go's own alert string;
// "tls handshake failed" appears in wrapper libraries (e.g. stealth/uTLS).
var transientMarkers = []string{
	"tls handshake failed",
	"tls: handshake failure",
	"tls: internal error",
	"connection refused",
	"connection reset",
	"unexpected eof",
	"network is unreachable",
	"no route to host",
	"i/o timeout", // dial timeout
}

// isTransient reports whether err is a transient transport failure safe to
// retry. It walks the wrapped error chain and matches message fragments, so
// stealth-wrapped dial errors ("stealth: tcp dial failed: ...: connection
// refused") classify the same as the bare dial error.
//
// Bare "EOF" is matched on exact whole-message equality only: a substring
// match on "eof" would over-retry unrelated errors that merely mention the
// letters ("... eof marker ...").
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		msg := strings.ToLower(cur.Error())
		for _, marker := range transientMarkers {
			if strings.Contains(msg, marker) {
				return true
			}
		}
		if msg == "eof" {
			return true
		}
	}
	return false
}

// retryProfileRotation is the pinned-profile rotation order for transient
// retries: one entry per distinct ClientHelloID, so a retry presents a
// genuinely different JA3 (rotating chrome120 -> chrome126 would change only
// headers, not the TLS fingerprint). ProfileRandom/ProfileAuto are excluded:
// they already resolve a fresh fingerprint per connection.
var retryProfileRotation = []struct {
	ids  []stealth.ProfileID
	next *stealth.Profile
}{
	{ids: []stealth.ProfileID{stealth.ProfileIDChrome120, stealth.ProfileIDChrome126, stealth.ProfileIDEdge126}, next: stealth.ProfileSafari18},
	{ids: []stealth.ProfileID{stealth.ProfileIDSafari17, stealth.ProfileIDSafari18}, next: stealth.ProfileFirefox128},
	{ids: []stealth.ProfileID{stealth.ProfileIDFirefox120, stealth.ProfileIDFirefox128}, next: stealth.ProfileChrome126},
}

// rotateStealthProfileForRetry swaps the pinned TLS fingerprint to a
// different profile before a retry so the retried connection does not repeat
// the fingerprint that just failed. The request keeps its CLI headers —
// only proxy-identifying headers are re-stripped; no browser persona is
// applied on API paths (#109). random/auto already rotate per connection and
// are left alone. No-op when retries are disabled or no fingerprint is
// pinned.
func (c *Client) rotateStealthProfileForRetry(req *http.Request) {
	c.profileMu.Lock()
	defer c.profileMu.Unlock()
	if c.transientRetriesLimit <= 0 || c.stealthProfile == nil {
		return
	}
	id := c.stealthProfile.ID
	if id == stealth.ProfileIDRandom || id == stealth.ProfileIDAuto {
		return
	}
	next := nextStealthProfile(c.stealthProfile)
	if next.ID == id {
		return
	}
	c.stealthProfile = next
	c.fingerprintRotations.Add(1)
	stealth.SanitizeHeaders(req.Header)
}

// nextStealthProfile returns the profile to rotate to after cur: the next
// entry in the fixed rotation order whose ClientHelloID differs from cur's.
func nextStealthProfile(cur *stealth.Profile) *stealth.Profile {
	for _, entry := range retryProfileRotation {
		for _, id := range entry.ids {
			if id == cur.ID {
				return entry.next
			}
		}
	}
	return retryProfileRotation[0].next
}

// retryDelay returns the sleep before a transient retry: a randomized
// 200-600ms backoff using crypto/rand (matching the request-jitter pattern).
// Tests pin it via Client.retryBackoff.
func (c *Client) retryDelay() time.Duration {
	if c.retryBackoff != nil {
		return c.retryBackoff()
	}
	var b [8]byte
	_, _ = cryptoRand.Read(b[:])
	u := binary.BigEndian.Uint64(b[:])
	return 200*time.Millisecond + time.Duration(u%uint64(400*time.Millisecond))
}

// wrapDecompress replaces resp.Body with a transparent decompressing reader
// when the upstream compresses the response. This is REQUIRED with the
// stealth profile: the browser Accept-Encoding ("gzip, deflate, br") makes
// Go's transport skip its automatic gzip handling (that only kicks in when
// Go itself set the header), so compressed bodies would arrive as garbage.
// The plain transport sends no Accept-Encoding and is unaffected (Go
// decompresses its own gzip transparently and strips the header).
func wrapDecompress(resp *http.Response) error {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return nil
	}
	underlying := resp.Body
	switch enc {
	case "gzip":
		zr, err := gzip.NewReader(underlying)
		if err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		resp.Body = &decompressCloser{Reader: zr, underlying: underlying}
	case "deflate":
		// RFC 9110 §8.4.1.3 defines Content-Encoding: deflate as a
		// zlib-wrapped stream (RFC 1950), but some servers historically
		// send raw DEFLATE (RFC 1951). Sniff the zlib header (CMF/FLG:
		// CM=8, CINFO<=7, 16-bit header a multiple of 31) WITHOUT
		// consuming bytes — a consumed header would corrupt the raw
		// fallback — and decode accordingly. (Audit B1: the raw-only
		// reader broke mid-stream on conforming zlib responses.)
		br := bufio.NewReader(underlying)
		head, _ := br.Peek(2)
		if len(head) == 2 && head[0]&0x0f == 8 && head[0]>>4 <= 7 &&
			(uint16(head[0])<<8|uint16(head[1]))%31 == 0 {
			zr, err := zlib.NewReader(br)
			if err != nil {
				return fmt.Errorf("deflate: %w", err)
			}
			resp.Body = &decompressCloser{Reader: zr, underlying: underlying}
		} else {
			resp.Body = &decompressCloser{Reader: flate.NewReader(br), underlying: underlying}
		}
	case "br":
		resp.Body = &decompressCloser{Reader: brotli.NewReader(underlying), underlying: underlying}
	case "zstd":
		// The stealth profiles advertise zstd in Accept-Encoding, so the
		// upstream may legitimately respond with it.
		zr, err := zstd.NewReader(underlying, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return fmt.Errorf("zstd: %w", err)
		}
		// zstd decoders are stateful (per-response buffers), unlike
		// gzip/brotli: Close must release the decoder's resources, not just
		// the underlying socket. (Audit B9.)
		resp.Body = &decompressCloser{Reader: zr, underlying: underlying, closeFn: func() error { zr.Close(); return nil }}
	default:
		return fmt.Errorf("unsupported Content-Encoding %q", enc)
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	return nil
}

// decompressCloser bridges a decompressing reader back to the underlying
// response body so Close always reaches the socket. closeFn optionally
// releases decoder-local resources (e.g. a zstd decoder's buffers) that are
// distinct from the underlying stream.
type decompressCloser struct {
	io.Reader
	underlying io.ReadCloser
	closeFn    func() error
}

func (d *decompressCloser) Close() error {
	if d.closeFn != nil {
		_ = d.closeFn()
	}
	return d.underlying.Close()
}

// releaseCancel cancels a do() timeout context unless it is nil.
func releaseCancel(cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
}

// cancelBody closes the underlying body and then releases the request
// context, so a streamed response body lives exactly as long as its reader.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	releaseCancel(b.cancel)
	return err
}

const (
	cliSystemMarker       = "You are Buffy, the strategic coding assistant. You are the AI agent behind the product, Freebuff, a tool where users can chat with you to code with AI for free."
	cliSystemMarkerPhrase = "You are Buffy, the strategic coding assistant"
)

// ensureCliSystemMarker guarantees the canonical "You are Buffy…" opening at
// byte position 0 of the first system message (the free-mode gate's trimmed
// prefix test — see the check loop below). It prepends the marker rather
// than replacing, so custom system instructions survive.
func ensureCliSystemMarker(payload map[string]any) {
	rawMsgs, ok := payload["messages"].([]any)
	if !ok || len(rawMsgs) == 0 {
		payload["messages"] = []any{
			map[string]any{"role": "system", "content": cliSystemMarker},
		}
		return
	}

	for _, m := range rawMsgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] == "system" {
			// The server gate is a TRIMMED PREFIX test at position 0
			// (hasFreebuffRootSystemPromptOpening, free-agents.ts:617-645),
			// hardened against the prepend-and-cancel proxy trick: a message
			// that merely mentions the phrase mid-string must NOT suppress
			// the canonical prefix (#110).
			if content, ok := msg["content"].(string); ok && strings.HasPrefix(strings.TrimSpace(content), cliSystemMarkerPhrase) {
				return // already present
			}
			if parts, ok := msg["content"].([]any); ok {
				for _, p := range parts {
					if partMap, ok := p.(map[string]any); ok {
						if txt, ok := partMap["text"].(string); ok && strings.HasPrefix(strings.TrimSpace(txt), cliSystemMarkerPhrase) {
							return // already present
						}
					}
				}
			}
		}
	}

	// Not present. Merge into first system message if exists, else unshift.
	for i, m := range rawMsgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] == "system" {
			if str, ok := msg["content"].(string); ok {
				if str == "" {
					msg["content"] = cliSystemMarker
				} else {
					msg["content"] = cliSystemMarker + "\n\n" + str
				}
			} else if parts, ok := msg["content"].([]any); ok {
				msg["content"] = append([]any{map[string]any{"type": "text", "text": cliSystemMarker}}, parts...)
			} else {
				msg["content"] = cliSystemMarker
			}
			rawMsgs[i] = msg
			payload["messages"] = rawMsgs
			return
		}
	}

	newMsgs := make([]any, 0, len(rawMsgs)+1)
	newMsgs = append(newMsgs, map[string]any{"role": "system", "content": cliSystemMarker})
	newMsgs = append(newMsgs, rawMsgs...)
	payload["messages"] = newMsgs
}

// injectEnvelope merges the CLI fingerprint into the request body without
// disturbing client-supplied fields: codebuff_metadata, provider
// data_collection=deny, stream=true, and the cb_easp stop sentinel when the
// request has no stop of its own.
func injectEnvelope(body []byte, costMode string, opts ChatOptions) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse request body: %w", err)
	}

	ensureCliSystemMarker(payload)

	// client_id is a FRESH random SDK-faithful draw per chat call — never
	// the sess:/run:-prefixed shapes the server fingerprints as a proxy
	// (#103; reference/freebuff run.ts:646
	// Math.random().toString(36).substring(2,15), cf-worker-signals.ts
	// ^wf-[a-z0-9]{8}$). trace_session_id remains per run (minted once by
	// the run manager, reused across the run's requests; run.ts:
	// previousRun?.traceSessionId ?? randomUUID, proxy-freebuff
	// lib/runs.js:43-46) and freebuff_instance_id stays per session.
	metadata := map[string]any{
		"run_id":    opts.RunID,
		"client_id": generateClientID(),
	}
	if opts.TraceSessionID != "" {
		metadata["trace_session_id"] = opts.TraceSessionID
	}
	if opts.SessionInstanceID != "" {
		metadata["freebuff_instance_id"] = opts.SessionInstanceID
	}
	// llm_step_number is the 1-based per-run agent step, String(n) on the
	// wire (#113; reference/freebuff run-agent-step.ts:1175-1177).
	if opts.StepNumber > 0 {
		metadata["llm_step_number"] = strconv.Itoa(opts.StepNumber)
	}
	if costMode != "" {
		metadata["cost_mode"] = costMode
	}
	payload["codebuff_metadata"] = metadata
	payload["provider"] = map[string]any{"data_collection": "deny"}
	payload["stream"] = true
	if _, hasStop := payload["stop"]; !hasStop {
		payload["stop"] = []string{"cb_easp"}
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("re-marshal envelope: %w", err)
	}
	return out, nil
}

// classify maps an upstream error response through the shared matrix and
// records the 428 waiting_room_required flag (issue #94) so the pool can
// fire the gated pre-session chain before the next session create. All
// in-client error paths must use this wrapper; the free classifyError stays
// pure for tests.
func (c *Client) classify(status int, body string, hdr http.Header) error {
	err := classifyError(status, body, hdr)
	// classifyError returns a concrete typed error in the interface, never a
	// nil interface — so err is always non-nil; test the sentinel directly.
	if errors.Is(err, ErrWaitingRoomRequired) {
		c.waitingRoomRequired.Store(true)
	}
	// T7 ledger: count every rate-limit-family classification by its
	// upstream body code and surface one Debug line carrying the FULL
	// (redacted) body, so the distinct refusal codes (free_mode_rate_limited,
	// insufficient_quota, limit_burst_rate, ip_capped, spend_limited,
	// rate_limited, ...) are distinguishable in logs before the #133
	// behavior fix lands.
	if code, window := rateLimitInfo(body, err); code != "" {
		c.countRateLimitEvent(code)
		logRateLimitClassified(status, body, code, window, err)
	}
	return err
}

// classifyError maps an upstream error response to the recovery matrix.
func classifyError(status int, body string, hdr http.Header) error {
	lower := strings.ToLower(body)
	retryAfter := parseRetryAfter(hdr)

	switch {
	case status == http.StatusForbidden && strings.Contains(lower, `"status":"banned"`):
		// The canonical ban body is {"status":"banned"} (the free-session
		// status wire shape, reference/freebuff freebuff-session.ts). Match
		// the marker exactly: any 403 whose body merely mentions the word
		// "banned" (e.g. {"error":"model temporarily banned..."}) must stay
		// a generic 403, not trigger the long ban cooldown. (Audit B5.)
		return parseBan(body)
	case strings.Contains(lower, "deployment_outside_hours"):
		// Free tier is outside its operating hours: temporarily unavailable
		// but worth a later retry. Checked before the status-driven 503/429
		// cases because upstream can attach it to any status (reference:
		// freebuff-reverse adapter.go classifies it Retryable by body first).
		return &UpstreamError{Status: status, Body: truncate(body, 500), RetryAfter: retryAfter, Retryable: true}
	case containsAny(lower, "free_mode_capacity_deferred"):
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
	case status == http.StatusConflict && containsAny(lower, "session_limit_reached"):
		// 409 session_limit_reached: the ACCOUNT is over its concurrent-tab
		// budget, but this session's row is fine (endsTheSession:false).
		// Distinct non-invalid error: the server surfaces 409 and never
		// refreshes/recreates the session
		// (reference/freebuff freebuff-session.ts FREEBUFF_GATE_CODES).
		return &SessionLimitError{Status: status, Body: truncate(body, 200)}
	case status == http.StatusForbidden && strings.Contains(lower, "free_mode_cli_required"):
		return fmt.Errorf("%w: %d %s", ErrFreeModeCLIRequired, status, truncate(body, 200))
	case status == http.StatusForbidden && strings.Contains(lower, "country_blocked"):
		return parseCountryBlock(body)
	case containsAny(lower, "ip_capped"):
		// 429 ip_capped: too many DISTINCT users on the egress IP.
		// Admission-only — existing sessions keep running, so unlike
		// rate_limited this is NOT tied to a quota reset. Cooldown is
		// bounded by the proxy to retryAfterMs + jitter, with a per-token
		// daily re-admission cap (3rd hit in a rolling window locks until
		// Pacific midnight — #118) (reference/freebuff freebuff-session.ts).
		return parseIpCapped(body, retryAfter)
	case containsAny(lower, "waiting_room_queued"):
		// 429 waiting_room_queued: transient admission race — the session
		// row was caught mid-admit (endsTheSession:false). NOT session
		// invalid: the row is fine, so the cached session must not be
		// invalidated or refreshed. Surfaced as 503 waiting_room_queued +
		// Retry-After via the shared WaitingRoomError
		// (reference/freebuff freebuff-session.ts FREEBUFF_GATE_CODES).
		return &WaitingRoomError{RetryAfter: retryAfter, Detail: truncate(body, 200)}
	case containsAny(lower, "waiting_room_required"):
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
	case containsAny(lower, "session_model_mismatch") && containsAny(lower, "limited"):
		// The egress IP cannot serve the requested model (e.g. "Limited free
		// access is only available with DeepSeek V4 Flash or MiMo 2.5." or
		// "model <id> is limited on this IP"). The session row is fine — it
		// stays bound to its admitted model — so this is NOT session-invalid:
		// invalidating would re-admit and burn a daily session slot. The server
		// marks the refusal and the pool registry cools the (egress, model)
		// pairing instead.
		return &LimitedIpError{RetryAfter: retryAfter, Body: truncate(body, 200)}
	case containsAny(lower, "session_superseded"):
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
	case containsAny(lower, "freebuff_update_required",
		"session_expired", "session_model_mismatch", "model_locked"):
		return fmt.Errorf("%w: %s%s", ErrSessionInvalid, truncate(body, 200), retryDetail(retryAfter))
	case status == http.StatusBadRequest && containsAny(lower, "runid not found", "runid not running"):
		return fmt.Errorf("%w: %s", ErrRunInvalid, truncate(body, 200))
	case status == http.StatusTooManyRequests && containsAny(lower, "insufficient_quota", "limit_burst_rate"):
		// #133: upstream load saturation ("The current group's upstream
		// load is saturated, please try again later"). No Retry-After in
		// the body — parseRateLimit would lock the token until Pacific
		// midnight on what is a minutes-scale transient. Bounded cooldown,
		// distinct code, no midnight lock.
		return &RateLimitError{
			Status:     "load_shedding",
			RetryAfter: LoadShedCooldown,
			Body:       truncate(body, 200),
		}
	case status == http.StatusTooManyRequests && containsAny(lower, "peak hours"):
		// #133: "Usage is temporarily limited during peak hours, when
		// upstream model prices double…". The peak end is unknowable from
		// the body: bounded conservative cooldown instead of locking the
		// token until Pacific midnight (the peak is hours, not a day).
		return &RateLimitError{
			Status:     "peak_hours",
			RetryAfter: PeakHoursCooldown,
			Body:       truncate(body, 200),
		}
	case status == http.StatusTooManyRequests || containsAny(lower, "rate_limited", "spend_limited"):
		return parseRateLimit(body, parseRetryAfter(hdr))
	default:
		return &UpstreamError{Status: status, Body: truncate(body, 500), RetryAfter: retryAfter}
	}
}

// rateLimitInfo derives the T7 ledger code and window for a rate-limit
// classification. The classification must be in the rate-limit error family
// (RateLimitError/IpCappedError/CapacityDeferredError) — 403 bans, 401 auth
// refusals, waiting rooms and other gates never count; code is empty then
// and nothing is logged.
func rateLimitInfo(body string, err error) (code, window string) {
	switch err.(type) {
	case *RateLimitError, *IpCappedError, *CapacityDeferredError:
	default:
		return "", ""
	}
	code = rateLimitCode(body, err)
	if code == "" {
		return "", ""
	}
	return code, rateLimitWindow(body, err)
}

// rateLimitCode extracts the upstream refusal code from the body's
// "error"/"type" field (free_mode_rate_limited, insufficient_quota,
// limit_burst_rate, ip_capped, spend_limited, rate_limited, ...), falling
// back to the classified error type when the body carries no code key.
func rateLimitCode(body string, err error) string {
	if code := bodyCode(body); code != "" {
		return code
	}
	switch e := err.(type) {
	case *CapacityDeferredError:
		return "free_mode_capacity_deferred"
	case *IpCappedError:
		return "ip_capped"
	case *RateLimitError:
		if e.Status != "" {
			return e.Status // load_shedding | peak_hours
		}
		return "rate_limited"
	}
	return ""
}

// bodyCode reads the first non-empty "error":"X" or "type":"X" string from a
// JSON error body (the ledger's code source).
func bodyCode(body string) string {
	var raw struct {
		Error string `json:"error"`
		Type  string `json:"type"`
	}
	if json.Unmarshal([]byte(body), &raw) != nil {
		return ""
	}
	if raw.Error != "" {
		return raw.Error
	}
	return raw.Type
}

// rateLimitWindow maps a rate-limit classification to the shared window
// table: the body's own "1 minute"/"30 minutes" text when present, else
// "reset" when the error carries a reset timestamp, else "retry-after" when
// it carries a retry delay, else "none".
func rateLimitWindow(body string, err error) string {
	lower := strings.ToLower(body)
	if strings.Contains(lower, "1 minute") {
		return "1 minute"
	}
	if strings.Contains(lower, "30 minutes") {
		return "30 minutes"
	}
	switch e := err.(type) {
	case *RateLimitError:
		if !e.ResetAt.IsZero() {
			return "reset"
		}
		if e.RetryAfter > 0 {
			return "retry-after"
		}
	case *IpCappedError:
		if e.RetryAfter > 0 {
			return "retry-after"
		}
	case *CapacityDeferredError:
		if e.RetryAfter > 0 {
			return "retry-after"
		}
	}
	return "none"
}

// rateLimitFields extracts the retry-after delay and reset timestamp a
// rate-limit-family error carries, for the classification Debug line.
func rateLimitFields(err error) (time.Duration, time.Time) {
	switch e := err.(type) {
	case *RateLimitError:
		return e.RetryAfter, e.ResetAt
	case *IpCappedError:
		return e.RetryAfter, time.Time{}
	case *CapacityDeferredError:
		return e.RetryAfter, time.Time{}
	}
	return 0, time.Time{}
}

// logRateLimitClassified emits the T7 ledger Debug line. The body is logged
// in FULL (the 200-rune truncation applies to the HTTP error response only)
// and must already be redacted by the caller.
func logRateLimitClassified(status int, body, code, window string, err error) {
	attrs := []any{
		"status", status,
		"code", code,
		"window", window,
		"body", body,
	}
	if retryAfter, resetAt := rateLimitFields(err); retryAfter > 0 {
		attrs = append(attrs, "retry_after", int(retryAfter.Seconds()))
		if !resetAt.IsZero() {
			attrs = append(attrs, "reset_at", resetAt.UTC().Format(time.RFC3339))
		}
	}
	slog.Debug("upstream rate limit classified", attrs...)
}

// errClassName names the classified error type for the `upstream response`
// debug line (T5). Wrapped sentinel errors (auth/session/run refusals built
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

// parseCountryBlock builds a CountryBlockedError from a 403 country_blocked
// body, extracting countryCode/countryBlockReason/ipPrivacySignals
// best-effort (absent fields are tolerated).
func parseCountryBlock(body string) error {
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

// NextPacificMidnight returns the upcoming 00:00 Pacific Time in UTC
// (which is 07:00 UTC during PDT / 08:00 UTC during PST).
func NextPacificMidnight() time.Time {
	loc, err := time.LoadLocation("America/Los_Angeles")
	now := time.Now()
	if err != nil {
		return pacificMidnightFallback(now)
	}
	nowLoc := now.In(loc)
	nextDay := time.Date(nowLoc.Year(), nowLoc.Month(), nowLoc.Day()+1, 0, 0, 0, 0, loc)
	return nextDay.UTC()
}

// pacificMidnightFallback approximates the upcoming Pacific midnight without
// the IANA tzdata database: America/Los_Angeles is UTC-7 during PDT
// (roughly March-November) and UTC-8 during PST (roughly November-March).
// The month range is the documented approximation; the exact DST transition
// dates require tzdata.
func pacificMidnightFallback(now time.Time) time.Time {
	hour := 7 // PDT
	if m := now.UTC().Month(); m < time.March || m > time.November {
		hour = 8 // PST: December, January, February
	}
	t := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), hour, 0, 0, 0, time.UTC)
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

func getNumber(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch n := v.(type) {
			case float64:
				return n, true
			case int:
				return float64(n), true
			case int64:
				return float64(n), true
			case string:
				if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
					return f, true
				}
			}
		}
	}
	return 0, false
}

func getTime(m map[string]any, keys ...string) (time.Time, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch val := v.(type) {
			case string:
				val = strings.TrimSpace(val)
				if t, err := time.Parse(time.RFC3339Nano, val); err == nil {
					return t, true
				}
				if t, err := time.Parse(time.RFC3339, val); err == nil {
					return t, true
				}
			case float64:
				if val > 1e11 { // milliseconds
					return time.UnixMilli(int64(val)).UTC(), true
				} else if val > 0 {
					return time.Unix(int64(val), 0).UTC(), true
				}
			}
		}
	}
	return time.Time{}, false
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
			ice.RetryAfter = time.Duration(ms) * time.Millisecond
		} else if sec, ok := getNumber(target, "retryAfter", "retry_after"); ok && sec > 0 {
			ice.RetryAfter = time.Duration(sec * float64(time.Second))
		}

		if n, ok := getNumber(target, "activeUsersForIp", "active_users_for_ip"); ok {
			ice.ActiveUsersForIP = int(n)
		}
		if lim, ok := getNumber(target, "limit"); ok {
			ice.Limit = lim
		}
	}

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

// parseRateLimit builds a RateLimitError from a 429 body, extracting
// retryAfterMs/resetAt/limit/recentCount best-effort across multiple JSON schemas.
// Falls back to the Retry-After header or automatically computes the upcoming
// Pacific midnight (07:00 UTC) when no explicit timestamp is provided.
func parseRateLimit(body string, headerRetryAfter time.Duration) error {
	rle := &RateLimitError{Body: truncate(body, 200), RetryAfter: headerRetryAfter}

	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err == nil {
		target := raw
		if errObj, ok := raw["error"].(map[string]any); ok {
			target = errObj
		}

		if ms, ok := getNumber(target, "retryAfterMs", "retry_after_ms"); ok && ms > 0 {
			rle.RetryAfter = time.Duration(ms) * time.Millisecond
		} else if sec, ok := getNumber(target, "retryAfter", "retry_after"); ok && sec > 0 {
			rle.RetryAfter = time.Duration(sec * float64(time.Second))
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
		if st, ok := target["status"].(string); ok {
			rle.Status = st
		}
	}

	if !rle.ResetAt.IsZero() && rle.ResetAt.After(time.Now()) {
		if rle.RetryAfter <= 0 {
			rle.RetryAfter = time.Until(rle.ResetAt)
		}
	} else if rle.RetryAfter <= 0 {
		// When rate-limited without a specific retry delay or timestamp,
		// auto-detect the next Pacific reset window (07:00 UTC).
		nextReset := NextPacificMidnight()
		rle.ResetAt = nextReset
		rle.RetryAfter = time.Until(nextReset)
	}

	if rle.RetryAfter <= 0 {
		rle.RetryAfter = 60 * time.Second
	}
	// T7 ledger window, computed after ResetAt/RetryAfter are finalized
	// (the Pacific-midnight fallback above sets ResetAt, so the window is
	// "reset" for timestamp-less 429s).
	rle.Window = rateLimitWindow(body, rle)
	return rle
}

// parseBan builds a BanError from a 403 banned body, extracting the
// resumes_at timestamp best-effort. resumes_at may be RFC3339, unix seconds,
// or unix milliseconds (parseFlexTime).
func parseBan(body string) error {
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

func retryDetail(retryAfter time.Duration) string {
	if retryAfter > 0 {
		return fmt.Sprintf(" (Retry-After %s)", retryAfter)
	}
	return ""
}

func containsAny(lower string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

// parseRetryAfter reads the Retry-After header (seconds or HTTP date).
func parseRetryAfter(hdr http.Header) time.Duration {
	raw := hdr.Get("Retry-After")
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		return time.Until(t)
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

func unixFrom(secs int64) time.Time {
	// Heuristic: milliseconds if 10^12 or larger, else seconds.
	if secs >= 100_000_000_000 {
		return time.Unix(0, secs*int64(time.Millisecond))
	}
	return time.Unix(secs, 0)
}

// generateClientID mints the SDK-faithful 13-char base36 client id
// (Math.random().toString(36).substring(2, 15)).
func generateClientID() string {
	var b [16]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable in practice; fall back to a
		// time-seeded value rather than panicking mid-request. UnixNano in
		// base36 is only 12 digits today, so pad to the SDK's 13-char length
		// (the old [:13] slice panicked on short values).
		return padBase36(strconv.FormatInt(time.Now().UnixNano(), 36))
	}
	n := new(big.Int).SetBytes(b[:])
	mod := new(big.Int).Exp(big.NewInt(36), big.NewInt(13), nil)
	return padBase36(n.Mod(n, mod).Text(36))
}

// padBase36 left-pads a base36 string with '0' to the SDK-faithful 13-char
// client id length. Both the crypto/rand draw and the time-seeded fallback
// need it: the latter is 12 digits, which would otherwise come out shorter
// than the JS substring(2, 15) equivalent.
func padBase36(id string) string {
	for len(id) < 13 {
		id = "0" + id
	}
	return id
}

func drainBody(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 51200))
	return string(data)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// truncateRunes truncates s to at most max runes without an ellipsis. The
// CLI's FINISH errorMessage cap is 5000 chars (truncateString in
// reference/freebuff/sdk/src/impl/database.ts), applied on the whole
// payload — a full Go stack trace must not blow the cap.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// dump writes a debug record to dump/ when enabled.
func (c *Client) dump(kind string, req *http.Request, status int, body string) {
	if !c.debugDump {
		return
	}
	name := fmt.Sprintf("%s-%d-%s.dump", kind, time.Now().UnixNano(), sanitizeName(req.URL.Path))
	path := filepath.Join("dump", name)
	_ = os.MkdirAll("dump", 0o755)
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s %s\n", req.Method, req.URL.String())
	for k, vs := range req.Header {
		for _, v := range vs {
			if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "x-codebuff-api-key") {
				v = "[redacted]"
			}
			fmt.Fprintf(&buf, "%s: %s\n", k, v)
		}
	}
	fmt.Fprintf(&buf, "\n[status %d]\n%s\n", status, truncate(body, 20000))
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		// T18: the write was previously swallowed (`_ = os.WriteFile`) —
		// surface the failure so a broken dump dir is not silent.
		slog.Warn("debug dump write failed", "path", path, "err", err)
	}
}

func sanitizeName(p string) string {
	p = strings.ReplaceAll(p, "/", "_")
	p = strings.ReplaceAll(p, ".", "_")
	if len(p) > 60 {
		p = p[:60]
	}
	return p
}
