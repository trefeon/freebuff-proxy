package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CreateSession POSTs /api/v1/freebuff/session with no body.
func (c *Client) CreateSession(ctx context.Context) (*SessionState, error) {
	return c.CreateSessionForModel(ctx, "")
}

// CreateSessionForModel POSTs /api/v1/freebuff/session with the requested
// model header. The POST carries NO body and therefore no Content-Type
// (#120): the CLI's session POST is a bare fetch with Authorization + the
// optional x-freebuff-model header only (reference/freebuff
// freebuff-session-api.ts callFreebuffSession, codebuff-api.ts sets

// mockTokenState tracks per-token usage for dummy tokens so the mock behaves
// like a real upstream session: recentCount rises per model, standing score
// degrades on cooldown, and quota resets at Pacific midnight.
type mockTokenState struct {
	mu           sync.Mutex
	recentCounts map[string]float64 // model -> recentCount
	standing     float64            // current standing score (0-100)
}

var mockStates sync.Map // token -> *mockTokenState

// getMockState returns the (lazily created) usage state for a dummy token.
func getMockState(token string) *mockTokenState {
	v, _ := mockStates.LoadOrStore(token, &mockTokenState{
		recentCounts: map[string]float64{},
		standing:     95.0,
	})
	return v.(*mockTokenState)
}

// mockQuotaLimit mirrors the real tier caps for the served models.
func mockQuotaLimit(model string) float64 {
	switch model {
	case "openai/gpt-5.6-luna", "deepseek/deepseek-v4-pro":
		return 5
	case "z-ai/glm-5.3-flash":
		return 2
	case "z-ai/glm-5.2":
		return 1
	default: // mimo, ox-alpha, deepseek-flash: unmetered
		return 9999
	}
}

func isDummyToken(token string) bool {
	t := strings.ToLower(strings.TrimSpace(token))
	return strings.HasPrefix(t, "cb_dummy") || strings.HasPrefix(t, "dummy-") || strings.HasPrefix(t, "mock-")
}

func mockSessionState(token string, requestedModel string, consume bool) *SessionState {
	if requestedModel == "" {
		requestedModel = "mimo/mimo-v2.5"
	}
	now := time.Now()
	pacific := pacificLoc()
	pacNow := now.In(pacific)
	pacMidnight := time.Date(pacNow.Year(), pacNow.Month(), pacNow.Day(), 0, 0, 0, 0, pacific)
	if !pacNow.Before(pacMidnight) {
		pacMidnight = pacMidnight.AddDate(0, 0, 1)
	}

	st := getMockState(token)
	st.mu.Lock()
	defer st.mu.Unlock()

	limit := mockQuotaLimit(requestedModel)
	if consume {
		st.recentCounts[requestedModel]++
	}
	recent := st.recentCounts[requestedModel]
	score := st.standing

	// Status transitions: active while quota remains; cooldown once recent >= limit.
	status := "active"
	var retryAfterMs int64
	if limit <= 9999 && recent >= limit {
		status = "cooldown"
		retryAfterMs = int64(time.Until(pacMidnight).Seconds() * 1000)
		if score > 60 {
			score -= 10
			st.standing = score
		}
	}
	if status == "active" && score < 95 {
		score++
		if score > 95 {
			score = 95
		}
		st.standing = score
	}

	instanceID := fmt.Sprintf("a%08x-b%04x-4%03x-8%03x-e%012x",
		now.UnixMilli()&0xFFFFFFFF, now.UnixMilli()&0xFFFF,
		now.UnixMilli()&0x0FFF, now.UnixMilli()&0x0FFF,
		now.UnixMilli()&0xFFFFFFFFFFFF)

	unlimited := float64(9999)

	state := &SessionState{
		Status:          status,
		InstanceID:      instanceID,
		Model:           requestedModel,
		CurrentModel:    requestedModel,
		RequestedModel:  requestedModel,
		ExpiresAt:       now.Add(24 * time.Hour),
		AdmittedAt:      now,
		Position:        0,
		QueueDepth:      0,
		EstimatedWaitMs: 0,
		PollAt:          now.Add(30 * time.Second),
		Limit:           limit,
		RecentCount:     recent,
		ResetAt:         pacMidnight,
		RetryAfterMs:    retryAfterMs,
		RateLimitsByModel: map[string]ModelQuota{
			"openai/gpt-5.6-luna": {
				Model:       "openai/gpt-5.6-luna",
				Limit:       5,
				RecentCount: st.recentCounts["openai/gpt-5.6-luna"],
				ResetAt:     pacMidnight,
				Period:      "pacific_day",
				Pool:        "premium",
				PoolLabel:   "Premium",
				Entitlement: map[string]float64{"base": 5},
			},
			"deepseek/deepseek-v4-pro": {
				Model:       "deepseek/deepseek-v4-pro",
				Limit:       5,
				RecentCount: st.recentCounts["deepseek/deepseek-v4-pro"],
				ResetAt:     pacMidnight,
				Period:      "pacific_day",
				Pool:        "premium",
				PoolLabel:   "Premium",
				Entitlement: map[string]float64{"base": 5},
			},
			"z-ai/glm-5.3-flash": {
				Model:       "z-ai/glm-5.3-flash",
				Limit:       2,
				RecentCount: st.recentCounts["z-ai/glm-5.3-flash"],
				ResetAt:     pacMidnight,
				Period:      "pacific_day",
				Pool:        "premium",
				PoolLabel:   "Premium",
				Entitlement: map[string]float64{"base": 2},
			},
			"mimo/mimo-v2.5": {
				Model:       "mimo/mimo-v2.5",
				Limit:       unlimited,
				RecentCount: st.recentCounts["mimo/mimo-v2.5"],
				ResetAt:     pacMidnight,
				Period:      "pacific_day",
				Pool:        "unlimited",
				PoolLabel:   "Unlimited",
			},
			"stealth/ox-alpha": {
				Model:       "stealth/ox-alpha",
				Limit:       unlimited,
				RecentCount: st.recentCounts["stealth/ox-alpha"],
				ResetAt:     pacMidnight,
				Period:      "pacific_day",
				Pool:        "unlimited",
				PoolLabel:   "Unlimited",
			},
			"deepseek/deepseek-v4-flash": {
				Model:       "deepseek/deepseek-v4-flash",
				Limit:       unlimited,
				RecentCount: st.recentCounts["deepseek/deepseek-v4-flash"],
				ResetAt:     pacMidnight,
				Period:      "pacific_day",
				Pool:        "unlimited",
				PoolLabel:   "Unlimited",
			},
			"z-ai/glm-5.2": {
				Model:       "z-ai/glm-5.2",
				Limit:       1,
				RecentCount: st.recentCounts["z-ai/glm-5.2"],
				ResetAt:     pacMidnight,
				Period:      "promo",
				Pool:        "glm-promo",
				PoolLabel:   "GLM Referral",
				Entitlement: map[string]float64{"referral": 1},
			},
		},
		Standing: &SessionStanding{
			Level:       "trusted",
			Label:       "Trusted",
			Score:       score,
			NextLevel:   "",
			CappedBy:    "third_party_client",
			Blurb:       "Your account is in good standing. Full access to all models.",
			NextLevelAt: time.Time{},
		},
		Referral: &SessionReferral{
			Code:                    "CB-MOCK0",
			ReferrerName:            "",
			QualifiedCount:          2,
			WeeklySessionsRemaining: 3,
			ResetAt:                 pacMidnight,
			GithubLinked:            true,
		},
	}

	if requestedModel == "z-ai/glm-5.2" {
		state.GlmPromo = fmt.Sprintf("{\"dailySessions\":%d,\"endsAt\":%q}",
			int(limit), pacMidnight.UTC().Format(time.RFC3339))
	}

	return state
}

func (c *Client) CreateSessionForModel(ctx context.Context, model string) (*SessionState, error) {
	if isDummyToken(c.token) {
		return mockSessionState(c.token, model, true), nil
	}
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
	if isDummyToken(c.token) {
		return mockSessionState(c.token, "", false), nil
	}
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
	if isDummyToken(c.token) {
		return mockSessionState(c.token, "", false), nil
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/freebuff/session", nil)
	if err != nil {
		return nil, err
	}
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
	if isDummyToken(c.token) {
		return nil
	}
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
	if isDummyToken(c.token) {
		return fmt.Sprintf("run-%s-%d", c.token, time.Now().UnixMilli()), nil
	}
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
	if isDummyToken(c.token) {
		return nil
	}
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
	return c.parseSessionResponse(req, resp, body)
}
