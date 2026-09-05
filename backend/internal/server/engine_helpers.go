package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"freebuff-proxy/backend/internal/phasetiming"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
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
}

// chatErrClass buckets an upstream error into the trace error column.
func chatErrClass(err error) string {
	switch err.(type) {
	case *upstream.RateLimitError:
		return "rate_limited"
	case *upstream.BanError:
		return "banned"
	case *upstream.IpCappedError:
		return "ip_capped"
	case *upstream.LimitedIpError:
		return "model_ip_limited"
	case *upstream.SessionLimitError:
		return "session_limit_reached"
	case *upstream.WaitingRoomError, *session.WaitingRoomError, *upstream.WaitingRoomRequiredError:
		return "waiting_room"
	case *upstream.SessionSupersededError:
		return "session_superseded"
	case *upstream.TurnSpendLimitError:
		return "turn_spend_limited"
	case *upstream.UpstreamError:
		return "upstream"
	default:
		return "error"
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

// attemptStatus extracts the upstream HTTP status carried by a chat error,
// or 0 when the error carries none (wrapped sentinels such as
// ErrSessionInvalid/ErrRunInvalid, and transport-level failures). A 0 is
// skipped in statuses_seen — only observed statuses are listed.
func attemptStatus(err error) int {
	switch e := err.(type) {
	case *upstream.UpstreamError:
		return e.Status
	case *upstream.CreditsError:
		return e.Status
	case *upstream.CapacityDeferredError:
		return e.Status
	case *upstream.SessionSupersededError:
		return e.Status
	case *upstream.TurnSpendLimitError:
		return e.Status
	case *upstream.SessionLimitError:
		return e.Status
	case *upstream.WaitingRoomRequiredError:
		// The canonical 428 waiting_room_required (#94); the marker can
		// ride 428/429 alike, 428 is the documented gate. No named
		// net/http constant exists for 428, so spell it out.
		return 428
	case *upstream.RateLimitError:
		// RateLimitError.Status is the upstream "429" string; parse when
		// numeric, else the 429 bucket is implicit.
		if n, perr := strconv.Atoi(e.Status); perr == nil {
			return n
		}
		return http.StatusTooManyRequests
	}
	return 0
}

// tokenLabel renders the lease's token for logging: "bridge" for bridge
// leases, the 1-based fixed-token index otherwise.
func tokenLabel(lease *pool.Lease) string {
	if lease == nil || lease.Bridge != nil {
		return "bridge"
	}
	return fmt.Sprintf("%d", lease.Token+1)
}
