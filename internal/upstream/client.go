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
	"context"
	cryptoRand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/net/proxy"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/stealth"
)

// Typed error sentinels. Callers use errors.Is against these; the concrete
// error values wrap an UpstreamError where applicable.
var (
	// ErrSessionInvalid: the free session is stale/superseded/expired or a
	// waiting room / update is required. Refresh the session and retry once.
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
	Body        string // truncated upstream body
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
	// LimitedModelOffers carries the limited-tier per-model allowances from
	// limitedModelOffers (present on limited-tier admissions, absent on
	// full-tier and compact poll responses; never required).
	LimitedModelOffers []LimitedModelOffer
	// RateLimitsByModel carries the live per-model session quotas from the
	// admission/poll response (key = model id). Absent on compact polls and
	// pre-join (none) responses; never required.
	RateLimitsByModel map[string]ModelQuota
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
	cliVersion         string
	costMode           string
	debugDump          bool

	// transientRetriesLimit is TRANSIENT_RETRIES: the maximum number of
	// additional attempts after a transient transport failure (0 disables
	// retries entirely). Only transport-level failures (dial/TLS/reset/EOF)
	// retry; classified upstream errors never do.
	transientRetriesLimit int

	// stealthProfile is the active TLS fingerprint. profileMu guards swaps
	// made by the retry loop (rotating the pinned profile before a retry);
	// newRequest and the dialer read it per request/connection. nil means
	// the plain Go transport.
	profileMu      sync.Mutex
	stealthProfile *stealth.Profile

	// socksProxies is the normalized SOCKS5 proxy list when SOCKS5_PROXIES
	// is configured (host:port each), with one prebuilt SOCKS5 dialer per
	// entry in socksDialers. The proxy for a request is chosen per request
	// by proxyIndex() and stashed in the request context; the transport
	// dialer reads the stash so the chosen proxy is the one actually dialed
	// (PROXY_ROTATION: per-token | round-robin | random). Empty when the
	// legacy single SOCKS5_PROXY or no proxy is configured.
	socksProxies  []string
	socksDialers  []proxy.Dialer
	proxyRotation string
	proxyCounter  atomic.Uint64 // round-robin cursor (per token)

	// Counters surfaced via the pool snapshot for /metrics.
	transientRetries     atomic.Int64 // transient transport failures retried
	fingerprintRotations atomic.Int64 // pinned fingerprint swaps ahead of a retry

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

// cliUserAgent mirrors the official CLI / SDK user agent. The upstream
// free-tier gate (403 free_mode_cli_required) requires requests to carry the
// AI-SDK user agent; random browser UAs are rejected. Kept as a fixed
// constant so the envelope is identical on every request.
const cliUserAgent = "ai-sdk/openai-compatible/0.10.7/codebuff"

// New builds the client for one token.
func New(token string, cfg *config.Config) (*Client, error) {
	return NewWithIndex(token, 0, cfg)
}

// NewWithIndex builds the client for token at tokenIndex. SOCKS5Proxies
// (plural) selects the outbound proxy per request per ProxyRotation: the
// legacy per-token binding pins token tokenIndex to proxy
// tokenIndex % len(proxies); round-robin advances a per-token atomic
// cursor; random draws via crypto/rand (#23).
func NewWithIndex(token string, tokenIndex int, cfg *config.Config) (*Client, error) {
	if token == "" {
		return nil, errors.New("upstream: empty token")
	}
	if cfg == nil {
		return nil, errors.New("upstream: nil config")
	}

	cliVer := cfg.CLIVersion
	if cliVer == "" {
		cliVer = "0.10.7"
	}

	c := &Client{
		token:                 token,
		tokenIndex:            tokenIndex,
		baseURL:               cfg.UpstreamBaseURL,
		requestTimeout:        cfg.RequestTimeout,
		sessionCallTimeout:    cfg.SessionCallTimeout,
		requestJitter:         cfg.RequestJitter,
		cliVersion:            cliVer,
		costMode:              cfg.CostMode,
		debugDump:             cfg.DebugDump,
		transientRetriesLimit: cfg.TransientRetries,
		proxyRotation:         cfg.ProxyRotation,
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

	switch {
	case len(cfg.SOCKS5Proxies) > 0:
		// PROXY_ROTATION: the proxy is chosen per request (newRequest stashes
		// the selected index) and this dialer reads the stash, so round-robin
		// and random actually rotate the outbound connection. per-token is
		// the default binding (token tokenIndex → proxy tokenIndex % n).
		// The DefaultTransport clone inherits http.ProxyFromEnvironment;
		// disable it so an operator HTTP_PROXY/HTTPS_PROXY env var never
		// double-routes SOCKS5 traffic through a second proxy.
		transport.Proxy = nil
		for _, raw := range cfg.SOCKS5Proxies {
			addr, err := parseProxyAddr(raw)
			if err != nil {
				return nil, fmt.Errorf("upstream: SOCKS5_PROXIES: %w", err)
			}
			dialer, err := proxy.SOCKS5("tcp", addr, nil, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("upstream: SOCKS5 dialer: %w", err)
			}
			c.socksProxies = append(c.socksProxies, addr)
			c.socksDialers = append(c.socksDialers, dialer)
		}
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return c.socksDialers[c.proxyIndexFor(ctx)].Dial(network, addr)
		}
		if len(c.socksProxies) > 1 {
			// Rotation is defeated by connection reuse: Go's transport serves
			// pooled idle connections (keyed on origin only) without re-invoking
			// DialContext, so the per-request proxy choice would never be
			// re-dialed on the typical single-stream workload. Disable
			// keep-alives so every request dials through its assigned proxy;
			// the single-proxy path keeps pooled connections.
			transport.DisableKeepAlives = true
		}
		baseDial = transport.DialContext
	case cfg.SOCKS5Proxy != "":
		socksAddr, err := parseProxyAddr(cfg.SOCKS5Proxy)
		if err != nil {
			return nil, fmt.Errorf("upstream: SOCKS5_PROXY: %w", err)
		}
		dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("upstream: SOCKS5 dialer: %w", err)
		}
		transport.Proxy = nil // same env-proxy isolation as SOCKS5_PROXIES
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
		baseDial = transport.DialContext
	case cfg.HTTPProxy != "":
		proxyURL, err := url.Parse(cfg.HTTPProxy)
		if err != nil {
			return nil, fmt.Errorf("upstream: HTTP_PROXY: %w", err)
		}
		if stealthProf != nil {
			// Go's transport ignores DialTLSContext for proxied HTTPS requests:
			// it invokes the TLS dialer with the PROXY's address (not the
			// origin), so transport.Proxy + DialTLSContext would hand the
			// stealth ClientHello to the plain CONNECT proxy and break the
			// tunnel. Instead, dial the proxy ourselves with CONNECT and let
			// the stealth dialer wrap the origin TLS over the tunnel. Plain-HTTP
			// upstreams in this combination go direct — a TLS fingerprint is
			// meaningless without TLS, and the default upstream is HTTPS.
			transport.Proxy = nil
			baseDial = httpConnectDial(proxyURL)
		} else {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}

	if stealthProf != nil {
		// Resolve the profile per request (instead of capturing it) so a
		// transient retry can swap the pinned fingerprint without rebuilding
		// the transport: rotateStealthProfileForRetry swaps c.stealthProfile
		// and the next dial picks it up. For auto/random, newRequest resolves
		// a concrete profile and stashes it so the browser headers and the
		// ClientHello always match; dialProfileFor prefers that stash.
		// baseDial is the configured outbound path (SOCKS5 dialer or HTTP
		// CONNECT tunnel); nil falls back to the default net.Dialer.
		transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return stealth.Dialer(c.dialProfileFor(ctx), baseDial, false)(ctx, network, addr)
		}
	}
	c.stealthProfile = stealthProf
	c.http = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			// Go strips Authorization/Cookie on cross-host redirects but not
			// x-codebuff-api-key, which carries the same raw token. Drop both
			// when the redirect target is a different host so the token never
			// leaks to a redirect target; same-host redirects (e.g. CDN or
			// bare-host -> www) keep their credentials.
			if !strings.EqualFold(via[0].URL.Host, req.URL.Host) {
				req.Header.Del("Authorization")
				req.Header.Del("x-codebuff-api-key")
			}
			return nil
		},
	}
	return c, nil
}

// ChatCompletions POSTs an OpenAI-shaped request to the upstream chat
// endpoint, injecting the CLI envelope, and returns the raw SSE body reader
// on 2xx. On error status it drains (up to 500 chars), classifies, and
// returns a typed error. The returned reader must be closed; closing it
// releases the connection.
func (c *Client) ChatCompletions(ctx context.Context, opts ChatOptions, body []byte) (io.ReadCloser, error) {
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
	req.Header.Set("x-freebuff-model", opts.Model)
	if opts.SessionInstanceID != "" {
		req.Header.Set("x-freebuff-instance-id", opts.SessionInstanceID)
	}
	resp, _, err := c.do(req, 0)
	if err != nil {
		releaseCancel(cancel)
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		bodyText := drainBody(resp.Body)
		releaseCancel(cancel)
		c.dump("chat", req, resp.StatusCode, bodyText)
		return nil, classifyError(resp.StatusCode, bodyText, resp.Header)
	}
	return &cancelBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

// CreateSession POSTs /api/v1/freebuff/session with an empty object.
func (c *Client) CreateSession(ctx context.Context) (*SessionState, error) {
	return c.CreateSessionForModel(ctx, "")
}

// CreateSessionForModel POSTs /api/v1/freebuff/session with the requested model header.
func (c *Client) CreateSessionForModel(ctx context.Context, model string) (*SessionState, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/freebuff/session", []byte("{}"))
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
	return c.GetSessionWithOpts(ctx, instanceID, false, false)
}

// GetSessionWithOpts polls /api/v1/freebuff/session with optional compact or heartbeat headers.
func (c *Client) GetSessionWithOpts(ctx context.Context, instanceID string, compact, heartbeat bool) (*SessionState, error) {
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
	if heartbeat {
		req.Header.Set("x-freebuff-heartbeat", "1")
	}
	return c.sessionCall(req)
}

// EndSession DELETE /api/v1/freebuff/session; 404 is tolerated.
func (c *Client) EndSession(ctx context.Context, instanceID string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/api/v1/freebuff/session", nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-freebuff-instance-id", instanceID)

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
		return classifyError(resp.StatusCode, bodyStr, resp.Header)
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
		return "", classifyError(resp.StatusCode, body, resp.Header)
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

// FinishRun POSTs /api/v1/agent-runs with action FINISH, marking the run
// completed with step accounting (mirrors the official CLI payload).
func (c *Client) FinishRun(ctx context.Context, runID string, totalSteps int) error {
	payload, _ := json.Marshal(map[string]any{
		"action":        "FINISH",
		"runId":         runID,
		"status":        "completed",
		"totalSteps":    totalSteps,
		"directCredits": 0,
		"totalCredits":  0,
	})
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/agent-runs", payload)
	if err != nil {
		return err
	}

	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return err
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	body := drainBody(resp.Body)
	if resp.StatusCode >= 400 {
		return classifyError(resp.StatusCode, body, resp.Header)
	}
	return nil
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
		LimitedModelOffers     []rawLimitedModelOffer   `json:"limitedModelOffers"`
		RateLimitsByModel      map[string]rawModelQuota `json:"rateLimitsByModel"`
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
		return state, nil
	}

	if resp.StatusCode >= 400 {
		return nil, classifyError(resp.StatusCode, body, resp.Header)
	}

	return nil, fmt.Errorf("upstream: unparseable session response %q", truncate(body, 200))
}

// requestProfileKey stashes the concrete stealth profile resolved for one
// request in its context, so the transport dialer builds the ClientHello
// from the SAME profile whose browser headers were applied (auto/random
// must not draw twice — headers and TLS fingerprint would mismatch).
type requestProfileKey struct{}

// proxyIndexKey stashes the per-request SOCKS5 proxy choice (PROXY_ROTATION)
// in the request context, so the transport dialer uses the proxy selected
// for this request.
type proxyIndexKey struct{}

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
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("x-codebuff-api-key", c.token)
	req.Header.Set("Content-Type", "application/json")
	ctx = req.Context()
	if profile := c.currentStealthProfile(); profile != nil {
		// Resolve the concrete profile ONCE per request and stash it: the
		// dialer reads the stash for the ClientHello, so the browser headers
		// applied here always match the TLS fingerprint. Pinned profiles
		// resolve to themselves; auto/random get one concrete draw.
		connProf := stealth.GetProfileForConnection(profile)
		ctx = withStealthProfile(ctx, connProf)
		stealth.SanitizeAndApply(req.Header, connProf)
	} else {
		ver := c.cliVersion
		if ver == "" {
			ver = "0.10.7"
		}
		req.Header.Set("User-Agent", fmt.Sprintf("ai-sdk/openai-compatible/%s/codebuff", ver))
	}
	if len(c.socksProxies) > 0 {
		ctx = context.WithValue(ctx, proxyIndexKey{}, c.proxyIndex())
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
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && timeout > 0 {
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
			slog.Debug("upstream ok", "method", req.Method, "path", req.URL.Path,
				"status", resp.StatusCode, "ms", time.Since(start).Milliseconds())
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
					"token", c.tokenIndex+1, "attempt", attempt, "err", bodyErr)
			} else {
				// Count the retry only once the replay succeeded: the counter
				// reflects retries that actually fired, not aborted ones.
				c.transientRetries.Add(1)
				req.Body = body
				req.Close = true // fresh connection for the retry
				slog.Debug("upstream transient failure, retrying",
					"token", c.tokenIndex+1, "attempt", attempt, "reason", err.Error(),
					"path", req.URL.Path)
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
			"ms", time.Since(start).Milliseconds(), "err", err)
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
// profile stashed by newRequest wins, so the ClientHello matches the browser
// headers applied to that request; a bare context (no stash) resolves per
// connection as before. For pinned profiles the current c.stealthProfile is
// authoritative: the retry loop swaps it (and re-applies headers) ahead of a
// retry, so the stash would be stale.
func (c *Client) dialProfileFor(ctx context.Context) *stealth.Profile {
	profile := c.currentStealthProfile()
	if profile != nil && (profile.ID == stealth.ProfileIDAuto || profile.ID == stealth.ProfileIDRandom) {
		if stashed := stealthProfileFrom(ctx); stashed != nil {
			return stashed
		}
	}
	return profile
}

// proxyIndexFor returns the SOCKS5 proxy index to dial for a request. The
// per-request choice stashed by newRequest wins; a bare context (e.g. a dial
// not preceded by newRequest) falls back to the per-token binding.
func (c *Client) proxyIndexFor(ctx context.Context) int {
	if idx, ok := ctx.Value(proxyIndexKey{}).(int); ok && idx >= 0 && idx < len(c.socksProxies) {
		return idx
	}
	return c.tokenIndex % len(c.socksProxies)
}

// proxyIndex selects the SOCKS5 proxy index for a new request according to
// PROXY_ROTATION: per-token (default) pins the token to its index,
// round-robin advances a per-token atomic cursor, random draws via
// crypto/rand. Unknown rotation values behave as per-token.
func (c *Client) proxyIndex() int {
	n := len(c.socksProxies)
	if n == 0 {
		return 0
	}
	switch c.proxyRotation {
	case "round-robin":
		return int((c.proxyCounter.Add(1) - 1) % uint64(n))
	case "random":
		return cryptoRandN(n)
	default:
		return c.tokenIndex % n
	}
}

// cryptoRandN returns a crypto-random integer in [0, n).
func cryptoRandN(n int) int {
	if n <= 0 {
		return 0
	}
	var b [8]byte
	_, _ = cryptoRand.Read(b[:])
	u := binary.BigEndian.Uint64(b[:])
	return int(u % uint64(n))
}

// TransientRetries returns how many transient transport failures were
// retried by this client (pool snapshot /metrics aggregation).
func (c *Client) TransientRetries() int64 { return c.transientRetries.Load() }

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
// different profile before a retry and re-applies its browser headers to req,
// so the retried connection does not repeat the fingerprint that just failed.
// random/auto already rotate per connection and are left alone. No-op when
// retries are disabled or no fingerprint is pinned.
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
	stealth.SanitizeAndApply(req.Header, next)
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
		zr := flate.NewReader(underlying)
		resp.Body = &decompressCloser{Reader: zr, underlying: underlying}
	case "br":
		resp.Body = &decompressCloser{Reader: brotli.NewReader(underlying), underlying: underlying}
	case "zstd":
		// The stealth profiles advertise zstd in Accept-Encoding, so the
		// upstream may legitimately respond with it.
		zr, err := zstd.NewReader(underlying, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return fmt.Errorf("zstd: %w", err)
		}
		resp.Body = &decompressCloser{Reader: zr, underlying: underlying}
	default:
		return fmt.Errorf("unsupported Content-Encoding %q", enc)
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	return nil
}

// decompressCloser bridges a decompressing reader back to the underlying
// response body so Close always reaches the socket.
type decompressCloser struct {
	io.Reader
	underlying io.ReadCloser
}

func (d *decompressCloser) Close() error { return d.underlying.Close() }

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
			if content, ok := msg["content"].(string); ok && strings.Contains(content, cliSystemMarkerPhrase) {
				return // already present
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
			if content, ok := msg["content"].(string); ok {
				if content == "" {
					msg["content"] = cliSystemMarker
				} else {
					msg["content"] = cliSystemMarker + "\n\n" + content
				}
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

	metadata := map[string]any{
		"run_id":    opts.RunID,
		"client_id": generateClientID(),
	}
	if opts.SessionInstanceID != "" {
		metadata["freebuff_instance_id"] = opts.SessionInstanceID
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

// classifyError maps an upstream error response to the recovery matrix.
func classifyError(status int, body string, hdr http.Header) error {
	lower := strings.ToLower(body)
	retryAfter := parseRetryAfter(hdr)

	switch {
	case status == http.StatusForbidden && (strings.Contains(lower, `"status":"banned"`) || strings.Contains(lower, "banned")):
		return parseBan(body)
	case strings.Contains(lower, "deployment_outside_hours"):
		// Free tier is outside its operating hours: temporarily unavailable
		// but worth a later retry. Checked before the status-driven 503/429
		// cases because upstream can attach it to any status (reference:
		// freebuff-reverse adapter.go classifies it Retryable by body first).
		return &UpstreamError{Status: status, Body: truncate(body, 500), RetryAfter: retryAfter, Retryable: true}
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %d %s", ErrAuthRejected, status, truncate(body, 200))
	case status == http.StatusServiceUnavailable:
		return &WaitingRoomError{RetryAfter: retryAfter, Detail: truncate(body, 200)}
	case status == http.StatusPaymentRequired:
		return &CreditsError{Status: status, Body: truncate(body, 200)}
	case status == http.StatusForbidden && strings.Contains(lower, "free_mode_cli_required"):
		return fmt.Errorf("%w: %d %s", ErrFreeModeCLIRequired, status, truncate(body, 200))
	case status == http.StatusForbidden && strings.Contains(lower, "country_blocked"):
		return parseCountryBlock(body)
	case containsAny(lower, "freebuff_update_required", "waiting_room_required", "waiting_room_queued",
		"session_superseded", "session_expired", "session_model_mismatch", "model_locked"):
		return fmt.Errorf("%w: %s%s", ErrSessionInvalid, truncate(body, 200), retryDetail(retryAfter))
	case status == http.StatusBadRequest && containsAny(lower, "runid not found", "runid not running"):
		return fmt.Errorf("%w: %s", ErrRunInvalid, truncate(body, 200))
	case status == http.StatusTooManyRequests || containsAny(lower, "rate_limited", "ip_capped", "spend_limited"):
		return parseRateLimit(body, parseRetryAfter(hdr))
	default:
		return &UpstreamError{Status: status, Body: truncate(body, 500), RetryAfter: retryAfter}
	}
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

// parseProxyAddr strips any scheme/userinfo from a proxy URL, returning
// host:port for the SOCKS5 dialer.
func parseProxyAddr(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty proxy URL")
	}
	if !strings.Contains(raw, "://") {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("proxy URL %q has no host", raw)
	}
	return u.Host, nil
}

// httpConnectDial returns a dial function that reaches addr through an HTTP
// CONNECT proxy: it dials the proxy, issues "CONNECT addr", and returns the
// tunneled connection. Used when TLS_FINGERPRINT is pinned: Go's transport
// ignores DialTLSContext for proxied HTTPS requests (it invokes the TLS
// dialer with the proxy's address), so routing the origin TLS through
// transport.Proxy would hand the stealth ClientHello to the plain CONNECT
// proxy instead of the origin.
func httpConnectDial(proxyURL *url.URL) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, network, proxyURL.Host)
		if err != nil {
			return nil, fmt.Errorf("upstream: dial HTTP proxy %s: %w", proxyURL.Host, err)
		}
		req := &http.Request{
			Method: http.MethodConnect,
			URL:    &url.URL{Opaque: addr},
			Host:   addr,
			Header: make(http.Header),
		}
		if proxyURL.User != nil {
			user := proxyURL.User.Username()
			pass, _ := proxyURL.User.Password()
			req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
		}
		if err := req.Write(conn); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("upstream: CONNECT %s: %w", addr, err)
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("upstream: CONNECT %s response: %w", addr, err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = conn.Close()
			return nil, fmt.Errorf("upstream: CONNECT %s: proxy %s", addr, resp.Status)
		}
		// Preserve any bytes the response reader buffered past the headers:
		// the TLS handshake must see them, not lose them.
		return &bufConn{conn: conn, r: br}, nil
	}
}

// bufConn bridges a buffered reader back to the underlying connection so the
// stealth TLS handshake reads exactly the bytes the CONNECT response reader
// left buffered (it would otherwise swallow the first TLS records).
type bufConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error)         { return b.r.Read(p) }
func (b *bufConn) Write(p []byte) (int, error)        { return b.conn.Write(p) }
func (b *bufConn) Close() error                       { return b.conn.Close() }
func (b *bufConn) LocalAddr() net.Addr                { return b.conn.LocalAddr() }
func (b *bufConn) RemoteAddr() net.Addr               { return b.conn.RemoteAddr() }
func (b *bufConn) SetDeadline(t time.Time) error      { return b.conn.SetDeadline(t) }
func (b *bufConn) SetReadDeadline(t time.Time) error  { return b.conn.SetReadDeadline(t) }
func (b *bufConn) SetWriteDeadline(t time.Time) error { return b.conn.SetWriteDeadline(t) }

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
	_ = os.WriteFile(path, buf.Bytes(), 0o600)
}

func sanitizeName(p string) string {
	p = strings.ReplaceAll(p, "/", "_")
	p = strings.ReplaceAll(p, ".", "_")
	if len(p) > 60 {
		p = p[:60]
	}
	return p
}
