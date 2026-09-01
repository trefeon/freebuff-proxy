package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// CreateSession POSTs /api/v1/freebuff/session with no body.
func (c *Client) CreateSession(ctx context.Context) (*SessionState, error) {
	return c.CreateSessionForModel(ctx, "")
}

// CreateSessionForModel POSTs /api/v1/freebuff/session with the requested
// model header. The POST carries NO body and therefore no Content-Type
// (#120): the CLI's session POST is a bare fetch with Authorization + the
// optional x-freebuff-model header only (reference/freebuff
// freebuff-session-api.ts callFreebuffSession; codebuff-api.ts sets the
// same request shape).

func (c *Client) CreateSessionForModel(ctx context.Context, model string) (*SessionState, error) {
	if c.mock != nil {
		return c.mock.CreateSession(c.token, model)
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
// GET itself.
func (c *Client) GetSessionWithOpts(ctx context.Context, instanceID string, compact bool) (*SessionState, error) {
	if c.mock != nil {
		return c.mock.GetSession(c.token, "")
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
	if c.mock != nil {
		return c.mock.Probe(c.token)
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
		// Build through banFromBody so the typed error matches the
		// classification matrix (issue #306): Body is the truncated raw body,
		// ResumesAt parsed from resumes_at.
		return nil, banFromBody(state.WireBody)
	case "country_blocked":
		return nil, countryBlockFromBody(state.WireBody)
	}
	return state, nil
}

// EndSession DELETE /api/v1/freebuff/session; 404 is tolerated. The DELETE
// is keyed on the user, not the instance: the CLI releases its slot with
// Authorization only, no x-freebuff-instance-id header (#120,
// reference/freebuff freebuff-session-api.ts releaseFreebuffSlot → DELETE).
func (c *Client) EndSession(ctx context.Context) error {
	if c.mock != nil {
		return c.mock.EndSession(c.token)
	}
	req, err := c.newRequest(ctx, http.MethodDelete, "/api/v1/freebuff/session", nil)
	if err != nil {
		return err
	}

	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return classErr
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == 404 {
		return nil // nothing to end
	}
	if classErr != nil {
		return classErr
	}
	return nil
}

// StartRun POSTs /api/v1/agent-runs with action START and returns the run id.
func (c *Client) StartRun(ctx context.Context, agentID string) (string, error) {
	if c.mock != nil {
		return c.mock.StartRun(c.token, agentID)
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
	// Dual-auth parity (current vendor wire): the agent-runtime client sends
	// BOTH Authorization and x-codebuff-api-key (the same raw token) on its
	// agent-runs/run POSTs (reference/freebuff packages/agent-runtime/src/
	// llm-api/codebuff-web-api.ts:70-71,301-302); the shipped CLI confirms
	// it. Applied AFTER newRequest's scrub, so any relayed/downstream
	// x-codebuff-api-key copy is overwritten by the authenticated token
	// (Set, not Add — a foreign value is never forwarded upstream).
	if !c.authOnly {
		req.Header.Set("x-codebuff-api-key", c.token)
	}
	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return "", classErr
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	if classErr != nil {
		return "", classErr
	}
	body := drainBody(resp.Body)
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
	if c.mock != nil {
		return c.mock.FinishRun(c.token)
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
	// Dual-auth parity (current vendor wire): agent-runs POSTs carry BOTH
	// Authorization and x-codebuff-api-key (the same raw token), mirroring
	// the agent-runtime's agent-runs/run POSTs (reference/freebuff
	// packages/agent-runtime/src/llm-api/codebuff-web-api.ts:70-71,301-302)
	// and the shipped CLI. Set after newRequest's scrub overwrites any
	// relayed/downstream x-codebuff-api-key copy with the authenticated
	// token (foreign values are never forwarded).
	if !c.authOnly {
		req.Header.Set("x-codebuff-api-key", c.token)
	}

	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return classErr
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	if classErr != nil {
		return classErr
	}
	return nil
}

// --- internals ---

// sessionCall performs a session control call: parse the JSON body into a
// SessionState; errors are classified through the standard matrix.
func (c *Client) sessionCall(req *http.Request) (*SessionState, error) {
	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return nil, classErr
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	body := drainBody(resp.Body)
	// A session control call always tries to parse the body first: a
	// structured 4xx/5xx carries the session status the callers switch on
	// (model_locked/model_unavailable/ip_capped/spend_limited/...), and a
	// 404 maps through parseSessionResponse (create -> disabled, poll/probe
	// -> ended). Only an unparseable body falls back to the classified
	// error do() already produced (issue #305).
	state, perr := c.parseSessionResponse(req, resp, body)
	if perr == nil {
		return state, nil
	}
	if classErr != nil {
		return nil, classErr
	}
	return nil, perr
}
