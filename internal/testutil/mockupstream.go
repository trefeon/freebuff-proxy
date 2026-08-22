// Package testutil provides a scriptable mock of the codebuff.com API for
// tests. It records every request for envelope assertions and supports
// failure injection (queued sessions, error statuses, auth rejection) plus
// client-disconnect detection for abort tests.
package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FinishedRun records an agent-run FINISH payload received by the mock.
type FinishedRun struct {
	RunID        string `json:"runId"`
	Status       string `json:"status"`
	TotalSteps   int    `json:"totalSteps"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	// Steps mirrors the CLI step wire shape, captured from the FINISH
	// payload (issue #114): steps are batched and sent WITH FINISH.
	Steps []RecordedStep `json:"steps,omitempty"`
}

// StartRequest records one agent-runs START payload (issue #91).
type StartRequest struct {
	AgentID        string   `json:"agentId"`
	AncestorRunIDs []string `json:"ancestorRunIds"`
}

// RecordedStep mirrors one agent-run step as received in a FINISH payload
// (issue #114): steps are batched and sent WITH FINISH — the CLI has no
// /steps endpoint.
type RecordedStep struct {
	ID          string   `json:"id"`
	StepNumber  int      `json:"stepNumber"`
	Credits     int      `json:"credits,omitempty"`
	ChildRunIDs []string `json:"childRunIds,omitempty"`
	MessageID   *string  `json:"messageId"`
	Status      string   `json:"status,omitempty"`
	StartTime   string   `json:"startTime"`
}

// MockUpstream is a scriptable codebuff.com stand-in.
type MockUpstream struct {
	srv *httptest.Server

	mu sync.Mutex

	// SessionMode is the mode returned by create/poll unless SessionSequence
	// is non-empty, in which case the next value in the sequence is used.
	SessionMode     string
	SessionSequence []string
	seqIdx          int

	QueuePosition   int
	QueueDepth      int
	EstimatedWaitMs int

	InstanceID string
	ExpiresIn  time.Duration

	// RateLimitsByModel, when non-empty, is embedded in the active session
	// body as the rateLimitsByModel map (official CLI wire shape, keyed by
	// model id) so tests can exercise quota parsing end-to-end.
	RateLimitsByModel map[string]any

	// GlmPromo, when non-nil, is embedded in the active session body as the
	// glmPromo block ({dailySessions, endsAt}; issue #178) so tests can
	// exercise the promo-quota data chain end-to-end.
	GlmPromo map[string]any

	// CountryCode / CountryBlockReason, when non-empty, are embedded in the
	// active session body (wire shape parsed by upstream's sessionCall) so
	// tests can exercise region reporting end-to-end.
	CountryCode        string
	CountryBlockReason string

	// Standing, when non-empty, is embedded in the session create/poll
	// response body's "standing" key (issue #96) so dashboard e2e tests can
	// exercise the account-standing data chain end-to-end.
	Standing map[string]any

	// RunIDs is the queue of run ids returned by agent-runs START.
	RunIDs []string
	runIdx int

	// StartRequests records every agent-runs START payload (agentId +
	// ancestorRunIds) so tests can assert the run-tree wiring.
	StartRequests []StartRequest

	ChatStatus    int    // 200 by default
	ChatBody      string // SSE body served on 200
	ChatErrorBody string // body served on ChatStatus >= 400
	ChatDelay     time.Duration
	ChatBlocks    bool // block until the request context is canceled
	// FinishDelay delays each FINISH response by this duration. Eviction
	// tests use it to hold a FINISH in flight while asserting bridge
	// operations do not block on it.
	FinishDelay time.Duration
	// FinishesStarted counts FINISH requests received, incremented before
	// FinishDelay elapses (tests poll it to detect an in-flight FINISH).
	FinishesStarted int
	AbortDetected   atomic.Bool
	ChatHandler     func(w http.ResponseWriter, r *http.Request) // optional full override
	SessionHandler  func(w http.ResponseWriter, r *http.Request) // optional full override

	SessionCreateDelay time.Duration // delay/create-block on session create+get

	AuthReject bool // 401 on every route

	// RateLimit makes every route return a 429 rate_limited quota-exhaustion
	// body (daily session quota, Pacific-day reset contract).
	RateLimit bool
	// RateLimitRetryAfterMs overrides retryAfterMs in the rate-limit body
	// when > 0 (default 48549499). Tests use it to serve DIFFERENT windows
	// per mock so best-window selection actually compares values.
	RateLimitRetryAfterMs int

	// Ban makes every route return a 403 {"status":"banned"} with a
	// resumes_at timestamp ~1h in the future.
	Ban bool

	// FinishFailures fails the next N agent-runs FINISH requests with a 500
	// (the body is recorded as usual otherwise). Tests use it to exercise
	// the runs package's FINISH-failure re-drain path.
	FinishFailures int

	RecordedChatHeaders []http.Header
	RecordedChatBodies  []string
	SessionCreates      int
	SessionPolls        int
	SessionProbes       int // token-level probes: GET session without x-freebuff-instance-id
	SessionEnds         int
	StartedRuns         []string
	FinishedRuns        []FinishedRun
	// Requests is the total number of HTTP requests the mock has served
	// (any route). Tests assert it stays unchanged when a pass must not
	// touch the upstream at all.
	Requests int

	// AuthCLICodeStatus is the status served by POST /api/auth/cli/code
	// (issue #62/#66); 200 by default. AuthCLICodeBody overrides the JSON
	// body served; the default is a valid code response with LoginURL.
	AuthCLICodeStatus int
	AuthCLICodeBody   string
	// AuthCLICodeRequests counts POST /api/auth/cli/code hits.
	AuthCLICodeRequests int
	// LastAuthFingerprintID records the fingerprintId from the most recent
	// POST /api/auth/cli/code body so tests can assert the login request
	// carries the stable machine-derived fingerprint. Read it only after
	// the request has settled (same convention as the counters above).
	LastAuthFingerprintID string
	// AuthCLIStatusBody is the JSON body served by GET
	// /api/auth/cli/status. When empty, the response is 401 (pending).
	// AuthCLIStatusRequests counts status polls.
	AuthCLIStatusBody     string
	AuthCLIStatusRequests int
	// AuthCLIHandler fully overrides both /api/auth/cli/* routes when set
	// (route dispatch falls through to it after the request counters).
	AuthCLIHandler func(w http.ResponseWriter, r *http.Request)
}

// NewMock starts the mock server. Call Close when done.
func NewMock() *MockUpstream {
	m := &MockUpstream{
		SessionMode:     "active",
		InstanceID:      "inst-abc-123",
		ExpiresIn:       30 * time.Minute,
		ChatStatus:      200,
		QueuePosition:   3,
		QueueDepth:      7,
		EstimatedWaitMs: 0,
		RunIDs:          []string{"run-0001", "run-0002", "run-0003"},
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	return m
}

// URL returns the mock base URL (http://127.0.0.1:port).
func (m *MockUpstream) URL() string { return m.srv.URL }

// Close shuts the mock server down.
func (m *MockUpstream) Close() { m.srv.Close() }

// SSEEvent renders a single OpenAI-style SSE chunk.
func SSEEvent(data string) string { return "data: " + data + "\n\n" }

func (m *MockUpstream) handle(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.Requests++
	authReject := m.AuthReject
	rateLimit := m.RateLimit
	ban := m.Ban
	retryAfterMsOverride := m.RateLimitRetryAfterMs
	m.mu.Unlock()
	if authReject {
		writeJSON(w, 401, `{"error":{"message":"unauthorized","type":"authentication_error"}}`)
		return
	}
	if rateLimit {
		// writeRaw (not writeJSON): the body must reach the client verbatim
		// so parseRateLimit can unmarshal the quota fields — writeJSON would
		retryAfterMs := 48549499
		if retryAfterMsOverride > 0 {
			retryAfterMs = retryAfterMsOverride
		}
		writeRaw(w, 429, fmt.Sprintf(`{"model":"deepseek/deepseek-v4-flash","entitlementBreakdown":{"base":6,"referral":0,"streak":0},"limit":6,"period":"pacific_day","resetTimeZone":"America/Los_Angeles","resetAt":"2026-08-12T07:00:00.000Z","windowHours":24,"recentCount":6.6,"status":"rate_limited","accessTier":"limited","retryAfterMs":%d}`, retryAfterMs))
		return
	}
	if ban {
		resumesAt := time.Now().Add(time.Hour).UTC().Format(rfc3339Millis)
		writeRaw(w, 403, `{"status":"banned","resumes_at":"`+resumesAt+`"}`)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/freebuff/session"):
		if m.SessionHandler != nil {
			m.SessionHandler(w, r)
			return
		}
		switch r.Method {
		case http.MethodPost:
			m.mu.Lock()
			m.SessionCreates++
			m.mu.Unlock()
			m.handleSession(w, r)
		case http.MethodGet:
			if r.Header.Get("x-freebuff-instance-id") == "" {
				// Token-level probe: a GET with no instance header claims no
				// session slot. Serve a zero-cost account state (status +
				// rateLimitsByModel) instead of the session poll body.
				m.mu.Lock()
				m.SessionProbes++
				m.mu.Unlock()
				m.handleProbe(w)
				return
			}
			m.mu.Lock()
			m.SessionPolls++
			m.mu.Unlock()
			m.handleSession(w, r)
		case http.MethodDelete:
			m.mu.Lock()
			m.SessionEnds++
			m.mu.Unlock()
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"status":"ended"}`)
		default:
			writeJSON(w, 405, `{"error":"method not allowed"}`)
		}
	case r.URL.Path == "/api/v1/agent-runs" && r.Method == http.MethodPost:
		m.handleAgentRuns(w, r)
	case r.URL.Path == "/api/v1/chat/completions" && r.Method == http.MethodPost:
		m.handleChat(w, r)
	case r.URL.Path == "/api/auth/cli/code" && r.Method == http.MethodPost:
		m.mu.Lock()
		m.AuthCLICodeRequests++
		status, body := m.AuthCLICodeStatus, m.AuthCLICodeBody
		handler := m.AuthCLIHandler
		m.mu.Unlock()
		// Record the request's fingerprintId (best-effort) so tests can
		// assert the login carries the stable machine fingerprint. The body
		// is restored afterwards so custom AuthCLIHandler implementations
		// can still read it.
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		var fp struct {
			FingerprintID string `json:"fingerprintId"`
		}
		if json.Unmarshal(raw, &fp) == nil && fp.FingerprintID != "" {
			m.mu.Lock()
			m.LastAuthFingerprintID = fp.FingerprintID
			m.mu.Unlock()
		}
		if handler != nil {
			handler(w, r)
			return
		}
		if status == 0 {
			status = http.StatusOK
		}
		if body == "" {
			body = `{"fingerprintId":"enhanced-test","fingerprintHash":"fp-hash-1","loginUrl":"https://github.com/login/oauth/authorize?auth_code=abc","expiresAt":` + strconv.FormatInt(time.Now().Add(5*time.Minute).UnixMilli(), 10) + `}`
		}
		writeRaw(w, status, body)
	case r.URL.Path == "/api/auth/cli/status" && r.Method == http.MethodGet:
		m.mu.Lock()
		m.AuthCLIStatusRequests++
		statusBody := m.AuthCLIStatusBody
		handler := m.AuthCLIHandler
		m.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		if statusBody == "" {
			w.WriteHeader(401)
			return
		}
		writeRaw(w, 200, statusBody)
	default:
		writeJSON(w, 404, `{"error":"not found"}`)
	}
}

// nextMode returns the next session mode from the sequence, else SessionMode.
func (m *MockUpstream) nextMode() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.SessionSequence) > 0 && m.seqIdx < len(m.SessionSequence) {
		mode := m.SessionSequence[m.seqIdx]
		m.seqIdx++
		return mode
	}
	return m.SessionMode
}

// rfc3339Millis renders timestamps with millisecond precision. Second-only
// RFC3339 truncation made short pollAt windows land in the past, which the
// session manager correctly treated as "queue advanced".
const rfc3339Millis = "2006-01-02T15:04:05.000Z07:00"

func (m *MockUpstream) handleSession(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	delay := m.SessionCreateDelay
	m.mu.Unlock()
	if delay > 0 {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(delay):
		}
	}

	mode := m.nextMode()
	m.mu.Lock()
	position, depth, waitMs := m.QueuePosition, m.QueueDepth, m.EstimatedWaitMs
	instanceID, expiresIn := m.InstanceID, m.ExpiresIn
	limits := m.RateLimitsByModel
	glmPromo := m.GlmPromo
	countryCode, countryBlockReason := m.CountryCode, m.CountryBlockReason
	standing := m.Standing
	m.mu.Unlock()

	switch mode {
	case "404":
		writeJSON(w, 404, map[string]any{"error": "session not found"})
	case "active":
		expiresAt := time.Now().Add(expiresIn).UTC().Format(rfc3339Millis)
		body := map[string]any{
			"status":     "active",
			"instanceId": instanceID,
			"expiresAt":  expiresAt,
		}
		if len(limits) > 0 {
			body["rateLimitsByModel"] = limits
		}
		if len(glmPromo) > 0 {
			body["glmPromo"] = glmPromo
		}
		if countryCode != "" {
			body["countryCode"] = countryCode
		}
		if countryBlockReason != "" {
			body["countryBlockReason"] = countryBlockReason
		}
		if len(standing) > 0 {
			body["standing"] = standing
		}
		writeJSON(w, 200, body)
	case "queued":
		pollAt := time.Now().Add(time.Duration(waitMs) * time.Millisecond).UTC().Format(rfc3339Millis)
		writeJSON(w, 200, map[string]any{
			"status":          "queued",
			"instanceId":      instanceID,
			"position":        position,
			"queueDepth":      depth,
			"estimatedWaitMs": waitMs,
			"pollAt":          pollAt,
		})
	case "disabled":
		writeJSON(w, 200, map[string]any{"status": "disabled"})
	case "ended":
		writeJSON(w, 200, map[string]any{"status": "ended"})
	case "superseded":
		writeJSON(w, 200, map[string]any{"status": "superseded"})
	case "none":
		writeJSON(w, 200, map[string]any{"status": "none"})
	case "banned":
		writeJSON(w, 200, map[string]any{"status": "banned"})
	case "country_blocked":
		// 403 with the region-block wire shape: classifyError maps it to
		// upstream.CountryBlockedError (S1 contract), so pool tests can
		// exercise the country-blocked failover bucket end-to-end.
		writeJSON(w, 403, map[string]any{
			"status":             "country_blocked",
			"countryCode":        "CN",
			"countryBlockReason": "region_restricted",
			"ipPrivacySignals":   []string{"vpn"},
		})
	case "model_locked":
		writeJSON(w, 200, map[string]any{"status": "model_locked"})
	default:
		writeJSON(w, 500, map[string]any{"error": "unknown mock session mode " + mode})
	}
}

// defaultProbeQuota is the rateLimitsByModel map served on a token-level
// probe (GET /api/v1/freebuff/session without x-freebuff-instance-id) when
// the test configured none. Small, realistic pacific_day quota so probe
// flows can assert parsed quota without wiring their own map.
var defaultProbeQuota = map[string]any{
	"deepseek/deepseek-v4-flash": map[string]any{
		"model":         "deepseek/deepseek-v4-flash",
		"limit":         6,
		"recentCount":   2,
		"period":        "pacific_day",
		"resetTimeZone": "America/Los_Angeles",
		"resetAt":       "2026-08-17T07:00:00.000Z",
	},
}

// handleProbe serves a token-level probe: 200 with an active account state,
// an instanceId, and the configured (or default) rateLimitsByModel. The
// probe is zero-cost — no session slot is claimed — so a valid token always
// succeeds regardless of SessionMode; tests needing a different probe
// response install a custom SessionHandler.
func (m *MockUpstream) handleProbe(w http.ResponseWriter) {
	m.mu.Lock()
	instanceID := m.InstanceID
	limits := m.RateLimitsByModel
	glmPromo := m.GlmPromo
	countryCode, countryBlockReason := m.CountryCode, m.CountryBlockReason
	m.mu.Unlock()
	if len(limits) == 0 {
		limits = defaultProbeQuota
	}
	body := map[string]any{
		"status":            "active",
		"instanceId":        instanceID,
		"rateLimitsByModel": limits,
	}
	if len(glmPromo) > 0 {
		body["glmPromo"] = glmPromo
	}
	// The probe mirrors the full admission shape (region state included —
	// the vendored CLI's session GET serves the same without any
	// fingerprint header, #140).
	if countryCode != "" {
		body["countryCode"] = countryCode
	}
	if countryBlockReason != "" {
		body["countryBlockReason"] = countryBlockReason
	}
	writeJSON(w, 200, body)
}

func (m *MockUpstream) handleAgentRuns(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var payload struct {
		Action         string         `json:"action"`
		AgentID        string         `json:"agentId"`
		RunID          string         `json:"runId"`
		Status         string         `json:"status"`
		TotalSteps     int            `json:"totalSteps"`
		ErrorMessage   string         `json:"errorMessage"`
		StepList       []RecordedStep `json:"steps"`
		AncestorRunIDs []string       `json:"ancestorRunIds"`
	}
	_ = json.Unmarshal(body, &payload)

	switch payload.Action {
	case "START":
		m.mu.Lock()
		m.StartRequests = append(m.StartRequests, StartRequest{
			AgentID:        payload.AgentID,
			AncestorRunIDs: append([]string(nil), payload.AncestorRunIDs...),
		})
		idx := m.runIdx
		if idx >= len(m.RunIDs) {
			m.mu.Unlock()
			writeJSON(w, 500, map[string]any{"error": "no mock run ids left"})
			return
		}
		m.runIdx++
		runID := m.RunIDs[idx]
		m.StartedRuns = append(m.StartedRuns, payload.AgentID)
		m.mu.Unlock()
		writeJSON(w, 200, map[string]any{"runId": runID})
	case "FINISH":
		m.mu.Lock()
		m.FinishesStarted++
		delay := m.FinishDelay
		fail := m.FinishFailures > 0
		if fail {
			m.FinishFailures--
		}
		m.mu.Unlock()
		if delay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}
		if fail {
			writeJSON(w, 500, map[string]any{"error": "mock FINISH failure"})
			return
		}
		m.mu.Lock()
		m.FinishedRuns = append(m.FinishedRuns, FinishedRun{
			RunID:        payload.RunID,
			Status:       payload.Status,
			TotalSteps:   payload.TotalSteps,
			ErrorMessage: payload.ErrorMessage,
			Steps:        append([]RecordedStep(nil), payload.StepList...),
		})
		m.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 400, map[string]any{"error": "unknown action " + payload.Action})
	}
}

// StartRequestsSnapshot returns a locked copy of the START payloads.
func (m *MockUpstream) StartRequestsSnapshot() []StartRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]StartRequest(nil), m.StartRequests...)
}

func (m *MockUpstream) handleChat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.RecordedChatHeaders = append(m.RecordedChatHeaders, r.Header.Clone())
	m.RecordedChatBodies = append(m.RecordedChatBodies, string(body))
	status, errBody := m.ChatStatus, m.ChatErrorBody
	blocks, delay := m.ChatBlocks, m.ChatDelay
	handler := m.ChatHandler
	m.mu.Unlock()

	if handler != nil {
		handler(w, r)
		return
	}

	if status >= 400 {
		if errBody == "" {
			errBody = `{"error":"mock upstream error"}`
		}
		writeRaw(w, status, errBody)
		return
	}

	if blocks {
		select {
		case <-r.Context().Done():
			m.AbortDetected.Store(true)
			return
		case <-time.After(30 * time.Second):
			return
		}
	}

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			m.AbortDetected.Store(true)
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	if m.ChatBody != "" {
		_, _ = io.WriteString(w, m.ChatBody)
	}
}

// WriteJSON is a helper for scripted ChatHandler implementations.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// writeRaw writes a pre-formatted JSON body verbatim (no re-encoding).
func writeRaw(w http.ResponseWriter, status int, raw string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, raw)
}

// BodyContains reports whether any recorded chat body contains substr.
func (m *MockUpstream) BodyContains(substr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.RecordedChatBodies {
		if strings.Contains(b, substr) {
			return true
		}
	}
	return false
}

// StartedRunsSnapshot returns a locked copy of the started-run agent ids.
// Use this instead of reading StartedRuns directly: the mock server goroutine
// appends to the field while requests are in flight, so raw reads race under
// -race whenever the test polls asynchronously (e.g. in eventually loops).
func (m *MockUpstream) StartedRunsSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.StartedRuns...)
}

// FinishedRunsSnapshot returns a locked copy of the finished runs. Use this
// instead of reading FinishedRuns directly (see StartedRunsSnapshot).
func (m *MockUpstream) FinishedRunsSnapshot() []FinishedRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]FinishedRun(nil), m.FinishedRuns...)
}

// FinishesStartedSnapshot returns a locked copy of the FINISH-received
// counter (see StartedRunsSnapshot). Tests poll it to detect an in-flight
// FINISH before FinishDelay has elapsed.
func (m *MockUpstream) FinishesStartedSnapshot() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.FinishesStarted
}

// RequestsSnapshot returns a locked copy of the total-request counter (see
// StartedRunsSnapshot). Tests assert it stays unchanged while a pass must
// not touch the upstream at all.
func (m *MockUpstream) RequestsSnapshot() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Requests
}

// SetFinishDelay sets FinishDelay under the mock's lock. Tests must use it
// instead of a plain field write: the agent-runs handler reads FinishDelay
// under the lock while a FINISH may already be in flight, so an unlocked
// write races under -race.
func (m *MockUpstream) SetFinishDelay(d time.Duration) {
	m.mu.Lock()
	m.FinishDelay = d
	m.mu.Unlock()
}

// SetFinishFailures sets FinishFailures under the mock's lock (see
// SetFinishDelay).
func (m *MockUpstream) SetFinishFailures(n int) {
	m.mu.Lock()
	m.FinishFailures = n
	m.mu.Unlock()
}

// SetAuthReject updates AuthReject under the mock's mutex to avoid
// write races under -race.
func (m *MockUpstream) SetAuthReject(b bool) {
	m.mu.Lock()
	m.AuthReject = b
	m.mu.Unlock()
}

// SetRateLimit updates RateLimit under the mock's mutex to avoid
// write races under -race.
func (m *MockUpstream) SetRateLimit(b bool) {
	m.mu.Lock()
	m.RateLimit = b
	m.mu.Unlock()
}

// SetBan updates Ban under the mock's mutex to avoid
// write races under -race.
func (m *MockUpstream) SetBan(b bool) {
	m.mu.Lock()
	m.Ban = b
	m.mu.Unlock()
}

// SessionCreatesSnapshot returns a locked copy of the session-create
// counter (see StartedRunsSnapshot). Tests poll it while an admission is in
// flight without racing the mock server goroutine.
func (m *MockUpstream) SessionCreatesSnapshot() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.SessionCreates
}

// SessionProbesSnapshot returns a locked copy of the token-probe (GET
// session without x-freebuff-instance-id) counter (see StartedRunsSnapshot).
func (m *MockUpstream) SessionProbesSnapshot() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.SessionProbes
}

// LastChatBody returns the most recently recorded chat request body, or "".
func (m *MockUpstream) LastChatBody() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.RecordedChatBodies) == 0 {
		return ""
	}
	return m.RecordedChatBodies[len(m.RecordedChatBodies)-1]
}

// RecordedChatBodiesSnapshot returns a locked copy of the recorded chat bodies.
func (m *MockUpstream) RecordedChatBodiesSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.RecordedChatBodies...)
}
