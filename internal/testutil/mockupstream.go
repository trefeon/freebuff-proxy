// Package testutil provides a scriptable mock of the codebuff.com API for
// tests. It records every request for envelope assertions and supports
// failure injection (queued sessions, error statuses, auth rejection) plus
// client-disconnect detection for abort tests.
package testutil

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FinishedRun records an agent-run FINISH payload received by the mock.
type FinishedRun struct {
	RunID      string `json:"runId"`
	Status     string `json:"status"`
	TotalSteps int    `json:"totalSteps"`
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

	// RunIDs is the queue of run ids returned by agent-runs START.
	RunIDs []string
	runIdx int

	ChatStatus     int    // 200 by default
	ChatBody       string // SSE body served on 200
	ChatErrorBody  string // body served on ChatStatus >= 400
	ChatDelay      time.Duration
	ChatBlocks     bool // block until the request context is canceled
	AbortDetected  atomic.Bool
	ChatHandler    func(w http.ResponseWriter, r *http.Request) // optional full override
	SessionHandler func(w http.ResponseWriter, r *http.Request) // optional full override

	SessionCreateDelay time.Duration // delay/create-block on session create+get

	AuthReject bool // 401 on every route

	// RateLimit makes every route return a 429 rate_limited quota-exhaustion
	// body (daily session quota, Pacific-day reset contract).
	RateLimit bool

	// Ban makes every route return a 403 {"status":"banned"} with a
	// resumes_at timestamp ~1h in the future.
	Ban bool

	RecordedChatHeaders []http.Header
	RecordedChatBodies  []string
	SessionCreates      int
	SessionPolls        int
	SessionEnds         int
	StartedRuns         []string
	FinishedRuns        []FinishedRun
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
	if m.AuthReject {
		writeJSON(w, 401, `{"error":{"message":"unauthorized","type":"authentication_error"}}`)
		return
	}
	if m.RateLimit {
		// writeRaw (not writeJSON): the body must reach the client verbatim
		// so parseRateLimit can unmarshal the quota fields — writeJSON would
		// re-encode it as a quoted JSON string.
		writeRaw(w, 429, `{"model":"deepseek/deepseek-v4-flash","entitlementBreakdown":{"base":6,"referral":0,"streak":0},"limit":6,"period":"pacific_day","resetTimeZone":"America/Los_Angeles","resetAt":"2026-08-12T07:00:00.000Z","windowHours":24,"recentCount":6.6,"status":"rate_limited","accessTier":"limited","retryAfterMs":48549499}`)
		return
	}
	if m.Ban {
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
	m.mu.Unlock()

	switch mode {
	case "404":
		writeJSON(w, 404, map[string]any{"error": "session not found"})
	case "active":
		expiresAt := time.Now().Add(expiresIn).UTC().Format(rfc3339Millis)
		writeJSON(w, 200, map[string]any{
			"status":     "active",
			"instanceId": instanceID,
			"expiresAt":  expiresAt,
		})
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
	case "model_locked":
		writeJSON(w, 200, map[string]any{"status": "model_locked"})
	default:
		writeJSON(w, 500, map[string]any{"error": "unknown mock session mode " + mode})
	}
}

func (m *MockUpstream) handleAgentRuns(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var payload struct {
		Action  string `json:"action"`
		AgentID string `json:"agentId"`
		RunID   string `json:"runId"`
		Status  string `json:"status"`
		Steps   int    `json:"totalSteps"`
	}
	_ = json.Unmarshal(body, &payload)

	switch payload.Action {
	case "START":
		m.mu.Lock()
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
		m.FinishedRuns = append(m.FinishedRuns, FinishedRun{
			RunID:      payload.RunID,
			Status:     payload.Status,
			TotalSteps: payload.Steps,
		})
		m.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 400, map[string]any{"error": "unknown action " + payload.Action})
	}
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
