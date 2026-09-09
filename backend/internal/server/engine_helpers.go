package server

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/phasetiming"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/store"
)

// traceChat records a structured "chat trace" entry for the dashboard
// traces page (the page filters the shared log ring by msg == "chat trace").
// phases carries the per-request latency phases (#89); the map is ordered
// deterministically for stable log output. st carries the retry-once
// attempt history (nil-safe: a refusal before any chat attempt passes a
// zero state).
func (s *Server) traceChat(lease *pool.Lease, model string, ms int64, status, errClass string, phases map[string]int64, st *chatTraceState) {
	attrs := []any{"model", model, "status", status, "ms", ms}
	if st != nil {
		if st.reqID != "" {
			attrs = append(attrs, "req_id", st.reqID)
		}
		if st.clientRequestID != "" {
			attrs = append(attrs, "client_request_id", st.clientRequestID)
		}
		if st.attempts > 0 {
			attrs = append(attrs, "attempts", st.attempts)
		}
		if seen := st.statusesSeen(); seen != "" {
			attrs = append(attrs, "statuses_seen", seen)
		}
		if st.retried {
			attrs = append(attrs, "retried", true, "backoff_ms", st.backoffMs)
		}
	}
	if lease != nil {
		attrs = append(attrs,
			"token", tokenLabel(lease),
			"agent", lease.AgentID,
			"trace_session_id", lease.Run.TraceSessionID,
		)
	}
	if errClass != "" {
		attrs = append(attrs, "error", errClass)
	}
	for _, name := range []string{
		phasetiming.AcquireMS,
		phasetiming.SessionRefreshMS,
		phasetiming.RunAcquireMS,
		phasetiming.UpstreamTTFBMS,
		phasetiming.TotalMS,
	} {
		if v, ok := phases[name]; ok {
			attrs = append(attrs, name, v)
		}
	}
	s.logger.Info("chat trace", attrs...)
	s.recordRequestOutcome(lease, model, status, errClass, phases, st)
}

// recordRequestOutcome persists one /v1 inference outcome to the history
// store for the Logs console view. It runs on the chat path but performs a
// single indexed upsert and never fails the request: a nil store skips the
// write (live-only), a missing req_id skips it (the PRIMARY KEY cannot
// distinguish pre-attempt refusals — the ring log still carries them), and
// insert errors only warn. Raw client tokens never reach the store: the
// lease's token index (bridge = -1) is the only token signal recorded.
func (s *Server) recordRequestOutcome(lease *pool.Lease, model string, status, errClass string, phases map[string]int64, st *chatTraceState) {
	if s.hist == nil || st == nil || st.reqID == "" {
		return
	}
	tokenIdx := -1
	if lease != nil {
		tokenIdx = lease.Token
	}
	var ttfb int64
	if phases != nil {
		ttfb = phases[phasetiming.UpstreamTTFBMS]
	}
	if err := s.hist.RecordRequest(store.RequestRecord{
		ReqID:    st.reqID,
		TS:       store.Millis(time.Now()),
		Endpoint: "/v1/chat/completions",
		Model:    model,
		TokenIdx: tokenIdx,
		Status:   status,
		TTFBms:   ttfb,
		Err:      errClass,
	}); err != nil {
		s.logger.Warn("request record failed", "err", err, "req_id", st.reqID)
	}
}

// chatDoneAttrs builds the structured log attributes for a completed chat,
// including reasoning effort when the client requested it.
func chatDoneAttrs(reqID, model, agent string, stream bool, ms int64, chunks, bytes int, reasoningEffort string) []any {
	attrs := []any{
		"req_id", reqID,
		"model", model,
		"agent", agent,
		"stream", stream,
		"ms", ms,
		"bytes", bytes,
	}
	if stream {
		attrs = append(attrs, "chunks", chunks)
	}
	if reasoningEffort != "" {
		attrs = append(attrs, "reasoning_effort", reasoningEffort)
	}
	return attrs
}

// chatTraceState accumulates the per-request attempt history for the chat
// trace line: how many upstream chat attempts fired, the HTTP statuses
// observed per attempt (success = 200), whether the retry-once recovery
// re-acquired a lease, and the measured re-acquire wait before the retry.
// Created in chatCore (which owns the req_id), filled by chatAttempt's
// retry loop.
type chatTraceState struct {
	reqID           string
	clientRequestID string
	attempts        int
	statuses        []int
	retried         bool
	backoffMs       int64
}

// statusesSeen renders the observed attempt statuses comma-joined
// ("409,200"), or "" when no attempt status was observed.
func (st *chatTraceState) statusesSeen() string {
	if len(st.statuses) == 0 {
		return ""
	}
	parts := make([]string, len(st.statuses))
	for i, s := range st.statuses {
		parts[i] = strconv.Itoa(s)
	}
	return strings.Join(parts, ",")
}

// tokenLabel renders the lease's token for logging: "bridge" for bridge
// leases, the 1-based fixed-token index otherwise.
func tokenLabel(lease *pool.Lease) string {
	if lease == nil || lease.Bridge != nil {
		return "bridge"
	}
	return fmt.Sprintf("%d", lease.Token+1)
}
