// Package upstream implements the codebuff.com wire client with the CLI
// request envelope required to pass the free-mode gate
// (403 free_mode_cli_required): x-freebuff-* headers, codebuff_metadata,
// provider.data_collection=deny, forced streaming, and the cb_easp stop
// sentinel. Error handling mirrors proxy-freebuff's recovery matrix: typed
// sentinels let callers refresh sessions, rotate runs, or cool down tokens.
package upstream

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	cryptoRand "crypto/rand"
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
	"time"

	"github.com/andybalholm/brotli"
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
)

// WaitRoom carries queue details for ErrWaitingRoom.
type WaitRoom struct {
	Position   int
	QueueDepth int
	RetryAfter time.Duration
}

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
	RetryAfterMs       int64
	AvailableHours     string
	Message            string
}

// ChatOptions carries the envelope values for a chat completion request.
type ChatOptions struct {
	Model             string
	RunID             string
	SessionInstanceID string // "" when the session is disabled
}

// Client speaks the codebuff.com wire protocol for a single token.
type Client struct {
	token   string
	baseURL string
	http    *http.Client

	requestTimeout     time.Duration
	sessionCallTimeout time.Duration
	requestJitter      time.Duration
	cliVersion         string
	costMode           string
	debugDump          bool
	stealthProfile     *stealth.Profile // nil when fingerprint unset
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

// NewWithIndex builds the client for token at index tokenIndex. SOCKS5Proxies
// (plural) binds each token to an outbound proxy round-robin (#23).
func NewWithIndex(token string, tokenIndex int, cfg *config.Config) (*Client, error) {
	if token == "" {
		return nil, errors.New("upstream: empty token")
	}
	if cfg == nil {
		return nil, errors.New("upstream: nil config")
	}

	socksProxy := cfg.SOCKS5Proxy
	if len(cfg.SOCKS5Proxies) > 0 {
		idx := tokenIndex % len(cfg.SOCKS5Proxies)
		socksProxy = cfg.SOCKS5Proxies[idx]
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	var baseDial func(ctx context.Context, network, addr string) (net.Conn, error)
	if socksProxy != "" {
		socksAddr, err := parseProxyAddr(socksProxy)
		if err != nil {
			return nil, fmt.Errorf("upstream: SOCKS5_PROXY: %w", err)
		}
		dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("upstream: SOCKS5 dialer: %w", err)
		}
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
		baseDial = transport.DialContext
	} else if cfg.HTTPProxy != "" {
		proxyURL, err := url.Parse(cfg.HTTPProxy)
		if err != nil {
			return nil, fmt.Errorf("upstream: HTTP_PROXY: %w", err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	var stealthProf *stealth.Profile
	if cfg.TLSFingerprint != "" {
		profile, ok := stealth.Lookup(cfg.TLSFingerprint)
		if !ok {
			return nil, fmt.Errorf("upstream: unknown TLS_FINGERPRINT %q", cfg.TLSFingerprint)
		}
		// baseDial is nil when no SOCKS5 proxy is set; Dialer uses its
		// internal default net.Dialer in that case.
		// NOTE: for HTTP_PROXY (CONNECT tunnel), Go uses Proxy + DialTLSContext
		// transparently; the stealth dialer replaces the TLS layer.
		transport.DialTLSContext = stealth.Dialer(profile, baseDial, false)
		stealthProf = profile
	}

	cliVer := cfg.CLIVersion
	if cliVer == "" {
		cliVer = "0.10.7"
	}

	return &Client{
		token:              token,
		baseURL:            cfg.UpstreamBaseURL,
		requestTimeout:     cfg.RequestTimeout,
		sessionCallTimeout: cfg.SessionCallTimeout,
		requestJitter:      cfg.RequestJitter,
		cliVersion:         cliVer,
		costMode:           cfg.CostMode,
		debugDump:          cfg.DebugDump,
		stealthProfile:     stealthProf,
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
	}, nil
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

// GetSession polls /api/v1/freebuff/session for the given instance. A 404
// maps to Status "disabled" (proxy-freebuff treats it as disabled).
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
	drainBody(resp.Body)
	if resp.StatusCode == 404 {
		return nil // nothing to end
	}
	if resp.StatusCode >= 400 {
		return classifyError(resp.StatusCode, drainCaptured(resp), resp.Header)
	}
	return nil
}

// StartRun POSTs /api/v1/agent-runs with action START and returns the run id.
func (c *Client) StartRun(ctx context.Context, agentID string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"action":  "START",
		"agentId": agentID,
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
		return &SessionState{Status: "disabled"}, nil
	}

	c.dump("session", req, resp.StatusCode, body)

	var raw struct {
		Status                 string   `json:"status"`
		InstanceID             string   `json:"instanceId"`
		Model                  string   `json:"model"`
		CurrentModel           string   `json:"currentModel"`
		RequestedModel         string   `json:"requestedModel"`
		ExpiresAt              any      `json:"expiresAt"`
		AdmittedAt             any      `json:"admittedAt"`
		GracePeriodEndsAt      any      `json:"gracePeriodEndsAt"`
		GracePeriodRemainingMs int64    `json:"gracePeriodRemainingMs"`
		Position               int      `json:"position"`
		QueueDepth             int      `json:"queueDepth"`
		EstimatedWaitMs        int      `json:"estimatedWaitMs"`
		PollAt                 any      `json:"pollAt"`
		AccessTier             string   `json:"accessTier"`
		CountryCode            string   `json:"countryCode"`
		CountryBlockReason     string   `json:"countryBlockReason"`
		IpPrivacySignals       []string `json:"ipPrivacySignals"`
		ActiveUsersForIP       int      `json:"activeUsersForIp"`
		Limit                  float64  `json:"limit"`
		RecentCount            float64  `json:"recentCount"`
		ResetAt                any      `json:"resetAt"`
		RetryAfterMs           int64    `json:"retryAfterMs"`
		AvailableHours         string   `json:"availableHours"`
		Message                string   `json:"message"`
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
		return state, nil
	}

	if resp.StatusCode >= 400 {
		return nil, classifyError(resp.StatusCode, body, resp.Header)
	}

	return nil, fmt.Errorf("upstream: unparseable session response %q", truncate(body, 200))
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
	req.Header.Set("Content-Type", "application/json")
	if c.stealthProfile != nil {
		connProf := stealth.GetProfileForConnection(c.stealthProfile)
		stealth.SanitizeAndApply(req.Header, connProf)
	} else {
		ver := c.cliVersion
		if ver == "" {
			ver = "0.10.7"
		}
		req.Header.Set("User-Agent", fmt.Sprintf("ai-sdk/openai-compatible/%s/codebuff", ver))
	}
	return req, nil
}

// do executes req, enforcing the given timeout unless ctx already carries an
// earlier deadline. The returned cancel must be released once the caller is
// done with the response BODY: canceling the request context aborts in-flight
// body reads, so it must outlive body streaming. cancel is nil when no
// timeout was applied. Failures are wrapped so errors.Is works both ways.
func (c *Client) do(req *http.Request, timeout time.Duration) (*http.Response, context.CancelFunc, error) {
	ctx := req.Context()
	start := time.Now()
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		req = req.WithContext(ctx)
	}
	resp, err := c.http.Do(req)
	if err != nil {
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
	if err := wrapDecompress(resp); err != nil {
		_ = resp.Body.Close()
		if cancel != nil {
			cancel()
		}
		return nil, nil, fmt.Errorf("upstream: %s %s: %w", req.Method, req.URL.Path, err)
	}
	slog.Debug("upstream ok", "method", req.Method, "path", req.URL.Path,
		"status", resp.StatusCode, "ms", time.Since(start).Milliseconds())
	return resp, cancel, nil
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

// injectEnvelope merges the CLI fingerprint into the request body without
// disturbing client-supplied fields: codebuff_metadata, provider
// data_collection=deny, stream=true, and the cb_easp stop sentinel when the
// request has no stop of its own.
func injectEnvelope(body []byte, costMode string, opts ChatOptions) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse request body: %w", err)
	}

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
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %d %s", ErrAuthRejected, status, truncate(body, 200))
	case status == http.StatusServiceUnavailable:
		return &WaitingRoomError{RetryAfter: retryAfter, Detail: truncate(body, 200)}
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

// parseRateLimit builds a RateLimitError from a 429 body, extracting
// retryAfterMs/resetAt/limit/recentCount best-effort. Falls back to the
// Retry-After header duration when the body has no JSON quota fields.
func parseRateLimit(body string, headerRetryAfter time.Duration) error {
	rle := &RateLimitError{Body: truncate(body, 200), RetryAfter: headerRetryAfter}
	var parsed struct {
		RetryAfterMs int64     `json:"retryAfterMs"`
		Limit        float64   `json:"limit"`
		RecentCount  float64   `json:"recentCount"`
		ResetAt      time.Time `json:"resetAt"`
		Status       string    `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		if parsed.RetryAfterMs > 0 {
			rle.RetryAfter = time.Duration(parsed.RetryAfterMs) * time.Millisecond
		}
		if !parsed.ResetAt.IsZero() {
			rle.ResetAt = parsed.ResetAt
		}
		rle.Limit, rle.RecentCount = parsed.Limit, parsed.RecentCount
	}
	return rle
}

// parseBan builds a BanError from a 403 banned body, extracting the
// resumes_at timestamp best-effort.
func parseBan(body string) error {
	be := &BanError{Body: truncate(body, 200)}
	var parsed struct {
		ResumesAt time.Time `json:"resumes_at"`
		Status    string    `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil && !parsed.ResumesAt.IsZero() {
		be.ResumesAt = parsed.ResumesAt
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
		// time-seeded value rather than panicking mid-request.
		return strconv.FormatInt(time.Now().UnixNano(), 36)[:13]
	}
	n := new(big.Int).SetBytes(b[:])
	mod := new(big.Int).Exp(big.NewInt(36), big.NewInt(13), nil)
	id := n.Mod(n, mod).Text(36)
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

func drainBody(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 51200))
	return string(data)
}

func drainCaptured(r *http.Response) string { return drainBody(r.Body) }

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
			if strings.EqualFold(k, "Authorization") {
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
