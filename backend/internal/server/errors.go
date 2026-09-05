package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
)

// quotaSummary renders the live per-model session quota from a probe's
// RateLimitsByModel map (models sorted for determinism), plus glmPromo when
// the response carried it; "" when the upstream response carried no quota
// data (compact responses omit it). Full admissions include the per-model
// breakdown by default — the proxy deliberately does NOT send the Web-only
// x-freebuff-include-unused-rate-limits header (issue #140 P1: a
// third-party-proxy fingerprint the CLI never sends; regression-pinned by
// TestProbeAccountDoesNotSendIncludeUnusedRateLimits).
func quotaSummary(st *upstream.SessionState) string {
	if st == nil || (len(st.RateLimitsByModel) == 0 && st.GlmPromo == "") {
		return ""
	}
	var parts []string
	models := make([]string, 0, len(st.RateLimitsByModel))
	for id := range st.RateLimitsByModel {
		models = append(models, id)
	}
	sort.Strings(models)
	for _, id := range models {
		q := st.RateLimitsByModel[id]
		entry := fmt.Sprintf("%s %s/%s", id, strconv.FormatFloat(q.Limit, 'f', -1, 64), strconv.FormatFloat(q.RecentCount, 'f', -1, 64))
		if q.Period != "" {
			entry += " " + q.Period
		}
		if !q.ResetAt.IsZero() {
			entry += fmt.Sprintf(", resets %s", q.ResetAt.Format(time.RFC3339))
		}
		parts = append(parts, entry)
	}
	if st.GlmPromo != "" {
		parts = append(parts, "glmPromo "+st.GlmPromo)
	}
	return "quota: " + strings.Join(parts, "; ")
}

// openAIError is the OpenAI error body with an optional human-readable hint (#19).
// Per OpenAPI 3.1 specification (reference/protocols/openai-openapi/openapi.yaml), code,
// message, param, and type are standard; param is null when unset.
type openAIError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code,omitempty"`
	Hint    string  `json:"hint,omitempty"`
}

// writeJSONError writes an OpenAI-shaped error response. Retry-After is set
// (in ceil seconds) only when retryAfter > 0.
func (s *Server) writeJSONError(w http.ResponseWriter, status int, message, typ, code string, retryAfter time.Duration) {
	s.writeJSONErrorWithHint(w, status, message, typ, code, "", retryAfter)
}

func (s *Server) writeJSONErrorWithHint(w http.ResponseWriter, status int, message, typ, code, hint string, retryAfter time.Duration) {
	if hint == "" {
		hint = defaultHintForCode(code, message)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": openAIError{Message: message, Type: typ, Code: code, Hint: hint},
	})
}

// openAIErrorType maps an internal error code to the OpenAI error `type`
// field at the call sites that route through writeClientError. The shared
// handler needs a single OpenAI shape; the type is derived from the code
// so every site keeps its historical categorization.
func openAIErrorType(status int, code string) string {
	switch code {
	case "rate_limit_exceeded":
		return "rate_limit_exceeded"
	case "missing_bearer_token":
		return "invalid_request_error"
	default:
		return "upstream_error"
	}
}

// writeClientError writes a client-error response in the envelope the
// request's wire expects: the OpenAI shape for /v1/chat/completions and
// /v1/responses, the Anthropic shape for /v1/messages. The Retry-After
// ceiling is computed once here, so every dispatch site shares one rounding
// rule (issue #253).
func (s *Server) writeClientError(w http.ResponseWriter, r *http.Request, status int, message, code string, retryAfter time.Duration) {
	if retryAfter > 0 {
		retrySec := int(math.Ceil(retryAfter.Seconds()))
		if retrySec < 1 {
			retrySec = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retrySec))
	}
	if isAnthropicRequest(r) {
		// retryAfter is passed as 0: the header is already written above.
		s.writeAnthropicError(w, r, status, message, code, 0)
		return
	}
	s.writeJSONErrorWithHint(w, status, message, openAIErrorType(status, code), code, "", 0)
}

func defaultHintForCode(code, message string) string {
	lowerMsg := strings.ToLower(message)
	switch {
	case code == "free_mode_cli_required" || strings.Contains(lowerMsg, "free_mode_cli_required"):
		return "Upstream free tier gate requires official CLI traffic envelope. See FAQ: https://github.com/trefeon/freebuff-proxy#faq"
	case code == "free_mode_legacy_luna_agent" || strings.Contains(lowerMsg, "free_mode_legacy_luna_agent"):
		return "Retired Luna agent — new session required, retry immediately."
	case code == "free_mode_rate_limited" || strings.Contains(lowerMsg, "free_mode_rate_limited"):
		return "Free-tier sliding window rate limit (30m). Wait for Retry-After or retry with backoff."
	case code == "free_mode_run_fanout" || strings.Contains(lowerMsg, "free_mode_run_fanout"):
		return "Upstream refused the account's concurrent agent runs (proxy-fanout signal). Honor Retry-After; run fewer parallel requests per token, or add another token."
	case code == "free_mode_invalid_agent_model" || strings.Contains(lowerMsg, "free_mode_invalid_agent_model"):
		return "The model is not in upstream's free-mode allowlist (retired id or stale registry). Wait for the registry refresh; if it persists, remove the model from MODELS_ALLOW and update."
	case code == "free_mode_capacity_deferred" || strings.Contains(lowerMsg, "free_mode_capacity_deferred"):
		return "Free tier at capacity — request deferred. Honor Retry-After (approx 2s for 30m window, 10s default) before retrying."
	case code == "account_banned" || strings.Contains(lowerMsg, "banned"):
		return "Account suspended upstream. Token is dead; create a fresh account with an established GitHub login."
	case code == "country_blocked" || strings.Contains(lowerMsg, "country blocked") || strings.Contains(lowerMsg, "country_blocked"):
		return "Your egress IP is in an unsupported region. Route traffic through an allowed country (e.g. US/EU/ID/SG)."
	case code == "out_of_credits" || strings.Contains(lowerMsg, "out of credits"):
		return "Upstream free-tier credits exhausted. Check COST_MODE in .env — valid values are free or unset; any other value fails startup validation."
	case code == "upstream_timeout":
		return "The upstream request exceeded its deadline. Retry, or raise REQUEST_TIMEOUT/SESSION_CALL_TIMEOUT in .env."
	case code == "upstream_auth_rejected" || code == "invalid_api_key" || strings.Contains(lowerMsg, "invalid api key"):
		return "Token invalid or expired. Get a fresh token by running scripts/gen-token.cmd (Windows) or scripts/gen-token.sh (Linux/macOS)"
	case code == "rate_limited":
		return "Session quota exhausted. Switch your coding harness to an unlimited model: z-ai/glm-5.3-flash or deepseek/deepseek-v4-flash, or wait for reset at Pacific midnight (07:00 UTC)."
	case code == "model_ip_limited":
		return "Model restricted on this egress IP/tier. Limited-tier accounts should switch to 'mimo/mimo-v2.5', or route traffic through a Tier-1 country (US/EU/SG)."
	case code == "ip_capped":
		return "Too many distinct users on this egress IP (admission-only). Retry after Retry-After or use a different egress."
	case code == "load_shedding":
		return "Upstream load shedding — transient minutes-scale saturation. Retry after ~90s."
	case code == "peak_hours":
		return "Premium peak-hours window — transient. Retry after ~30m."
	case code == "missing_bearer_token":
		return "Bridge mode active: pass your FreeBuff token in Authorization: Bearer <token>"
	case code == "model_not_found":
		return "Check available models via GET /v1/models"
	default:
		return ""
	}
}

// rateLimitWarnShouldLog reports whether the (token, code, window) log
// should fire for this occurrence, always incrementing the occurrence count.
// The ledger is per-Server (issue #252): two Server instances in one process
// keep separate dedupe state.
func (s *Server) rateLimitWarnShouldLog(key string) bool {
	s.rateLimitDedupe.mu.Lock()
	defer s.rateLimitDedupe.mu.Unlock()
	if s.rateLimitDedupe.m == nil {
		s.rateLimitDedupe.m = make(map[string]int64)
	}
	s.rateLimitDedupe.m[key]++
	n := s.rateLimitDedupe.m[key]
	return n == 1 || n%50 == 0
}

// writeError maps any error from the pool/upstream to the PRD §6 matrix and
// logs it once. Canceled client contexts are logged at debug and dropped (no
// response written). model and lease come from the call site: model is the
// request's effective model, lease the acquired token lease (nil when the
// error fired before acquisition — e.g. an unfit-egress refusal).
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error, model string, lease *pool.Lease) {
	// A dropped client leaves no downstream status (the access wrapper keeps
	// its 200 default), so req_id is the only correlation key — always log
	// it here, or the dashboard renders a context-free "ERROR 200".
	reqID := ""
	if r != nil {
		reqID = reqIDFrom(r.Context())
	}
	if errors.Is(err, context.Canceled) {
		s.logger.Debug("request canceled by client", "req_id", reqID, "err", err)
		return
	}
	if r != nil && r.Context().Err() != nil {
		s.logger.Debug("client context canceled; not writing error", "req_id", reqID, "err", err)
		return
	}

	status := http.StatusBadGateway
	code := "upstream_unavailable"
	message := err.Error()
	var retryAfter time.Duration
	var resetAt time.Time
	window := "" // T7 ledger window; set for rate-limit errors (dedupe key)

	var wr *session.WaitingRoomError
	var uwr *upstream.WaitingRoomError
	var wrr *upstream.WaitingRoomRequiredError
	var sse *upstream.SessionSupersededError
	var ue *upstream.UpstreamError
	var tsle *upstream.TurnSpendLimitError
	var rle *upstream.RateLimitError
	var ice *upstream.IpCappedError
	var sle *upstream.SessionLimitError
	var lie *upstream.LimitedIpError
	var be *upstream.BanError
	var cbe *upstream.CountryBlockedError
	var ce *upstream.CreditsError
	var cde *upstream.CapacityDeferredError
	switch {
	case errors.As(err, &be):
		status, code = http.StatusForbidden, "account_banned"
		message, retryAfter = be.Error(), time.Until(be.ResumesAt)
		resetAt = be.ResumesAt
		if retryAfter < 0 {
			retryAfter = 0
		}
	case errors.As(err, &tsle):
		// turn_spend_limit: upstream killed this turn (loop protection).
		// 429 keeps the overload-family status clients expect, code stays
		// "turn_spend_limited", but — critically — NO Retry-After: the
		// breaker's retryAfterMs does not clear it (live 2026-09-05: 20+
		// min of instant 60s re-trips), and a Retry-After header would
		// hand the harness a futile retry drumbeat. The upstream body
		// (loop warning) goes to the client verbatim so the agent
		// abandons this turn. No cooldown is scheduled in chatAttempt, so
		// a genuinely new turn flows immediately.
		status, code = http.StatusTooManyRequests, "turn_spend_limited"
		message = tsle.Body
		if message == "" {
			message = "upstream turn spend limit exceeded (runaway turn — start a fresh turn, do not retry this one)"
		}
		retryAfter = 0
	case errors.As(err, &rle):
		status, code = http.StatusTooManyRequests, "rate_limited"
		switch rle.Status {
		case "load_shedding":
			// #133: upstream load saturation — minutes-scale transient with
			// a bounded cooldown; surfaced honestly instead of the daily-cap
			// "rate_limited" hint.
			code = "load_shedding"
		case "peak_hours":
			// #133: upstream peak-hours pricing window — bounded cooldown,
			// not a quota lock.
			code = "peak_hours"
		case "free_mode_run_fanout":
			// Upstream concurrent-run/fanout refusal: 429 + bounded
			// Retry-After so the client backs off and the next request
			// lands on another token — never the dead 502 the generic
			// UpstreamError branch used to write.
			code = "free_mode_run_fanout"
		case "free_mode_invalid_agent_model":
			// #140 P1: the (agent, model) pair is not in upstream's
			// allowlist (stale registry serving a retired id). 429 + bounded
			// cooldown rotates the pool instead of amplifying the 403 storm;
			// distinct code so operators can spot it in logs, and the pool's
			// escalation guard alerts at 3 hits/60s.
			code = "free_mode_invalid_agent_model"
		}
		message, retryAfter = rle.Error(), rle.RetryAfter
		resetAt, window = rle.ResetAt, rle.Window
		if !rle.ResetAt.IsZero() && rle.ResetAt.After(time.Now()) {
			retryAfter = time.Until(rle.ResetAt)
		}
		if retryAfter < 0 {
			retryAfter = 0
		}
		if code == "rate_limited" {
			targetModel := rle.Model
			if targetModel == "" {
				targetModel = model
			}
			if targetModel == "z-ai/glm-5.2" || strings.Contains(strings.ToLower(rle.Body), "referral") {
				message = fmt.Sprintf("%s. Model '%s' requires referral entitlement on your account. Please switch your coding harness to an unlimited session model: 'z-ai/glm-5.3-flash' or 'deepseek/deepseek-v4-flash'.", message, targetModel)
			} else {
				resetHint := ""
				if retryAfter > 0 {
					resetHint = fmt.Sprintf(" Resets in %s at Pacific midnight (07:00 UTC).", formatDuration(retryAfter))
				}
				message = fmt.Sprintf("%s. Daily session quota exhausted for '%s'.%s Switch your coding harness to an unlimited session model: 'z-ai/glm-5.3-flash' or 'deepseek/deepseek-v4-flash'.", message, targetModel, resetHint)
			}
		}
	case errors.As(err, &ice):
		// ip_capped: admission-only (too many distinct users on the egress
		// IP) — 429, not the quota 429, with the body's retryAfterMs only.
		status, code = http.StatusTooManyRequests, "ip_capped"
		message, retryAfter = ice.Error(), ice.RetryAfter
		if retryAfter < 0 {
			retryAfter = 0
		}
	case errors.As(err, &sle):
		// session_limit_reached (409): the ACCOUNT is over its concurrent-tab
		// budget; this session's row is fine. Never session-invalid.
		status, code = http.StatusConflict, "session_limit_reached"
		message = sle.Body
		if message == "" {
			message = "session limit reached"
		}
	case errors.As(err, &lie):
		// Issue #74 P2: the egress IP cannot serve the requested model.
		// 409 (not a quota lock): a different egress or token may still
		// serve the model. The body's retryAfterMs is surfaced
		// as Retry-After but does not set the unfit window.
		status, code = http.StatusConflict, "model_ip_limited"
		targetModel := lie.Model
		if targetModel == "" {
			targetModel = model
		}
		message = fmt.Sprintf("%s: model '%s' is unavailable on this egress IP/tier. If your account or IP is on limited tier (non-Tier-1 region), switch your coding harness to 'mimo/mimo-v2.5' (the available model for limited tier), or route traffic via a residential connection in a Tier-1 country (US/UK/EU/SG).", lie.Error(), targetModel)
		retryAfter = lie.RetryAfter
		if retryAfter < 0 {
			retryAfter = 0
		}
	case errors.Is(err, upstream.ErrModelIPLimited):
		status, code = http.StatusConflict, "model_ip_limited"
		message = fmt.Sprintf("%s: model '%s' is unavailable on this egress IP/tier. If your account or IP is on limited tier (non-Tier-1 region), switch your coding harness to 'mimo/mimo-v2.5' (the available model for limited tier), or route traffic via a residential connection in a Tier-1 country (US/UK/EU/SG).", err.Error(), model)
		retryAfter = 0
	case errors.As(err, &wr):
		status, code = http.StatusServiceUnavailable, "waiting_room_queued"
		message, retryAfter = wr.Error(), wr.RetryAfter
	case errors.As(err, &uwr):
		status, code = http.StatusServiceUnavailable, "waiting_room_queued"
		message, retryAfter = uwr.Error(), uwr.RetryAfter
	case errors.As(err, &wrr):
		// #116: 428 waiting_room_required (endsTheSession:true — the seat
		// is gone; chatAttempt already dropped the cached session and
		// re-admitted once). 503 + the refusal's Retry-After — NEVER a bare
		// 502. MUST precede the generic UpstreamError branch.
		status, code = http.StatusServiceUnavailable, "waiting_room_required"
		message, retryAfter = wrr.Error(), wrr.RetryAfter
		if retryAfter < 0 {
			retryAfter = 0
		}
	case errors.As(err, &sse):
		// #119: 503 session_superseded — another instance took over the
		// account. Return 503 + Retry-After (not 409) so 9router retries
		// immediately instead of locking the model for 30s. The session is
		// already invalidated in chatAttempt so the next request re-joins fresh.
		status, code = http.StatusServiceUnavailable, "session_superseded"
		message = sse.Body
		if message == "" {
			message = "session superseded"
		}
		retryAfter = 1 // retry in 1s
	case errors.As(err, &cde):
		// #105 (server half): the client's capacity-deferred retry budget
		// (TRANSIENT_RETRIES) is exhausted, so the free tier's transient
		// capacity queue is surfaced to downstream clients as 429 +
		// Retry-After — they must honor the window, not hammer a 502/503.
		// MUST precede the generic errors.As(err, &ue) branch: the error
		// unwraps to a Retryable UpstreamError, which would otherwise be
		// swallowed as 503 upstream_retryable.
		status, code = http.StatusTooManyRequests, "free_mode_capacity_deferred"
		message = cde.Body
		if message == "" {
			message = cde.Error()
		}
		retryAfter = cde.RetryAfter
		if retryAfter <= 0 {
			retryAfter = 10 * time.Second
		}
	case errors.Is(err, upstream.ErrSessionInvalid):
		// Session invalid (stale/expired/retired agent): surface 502 with
		// a distinct code so callers can distinguish and hint the user to
		// start a new conversation. MUST precede the generic
		// errors.As(err, &ue) branch because ErrSessionInvalid wraps a
		// plain fmt.Errorf, not a concrete type.
		status = http.StatusBadGateway
		msg := err.Error()
		if strings.Contains(msg, "free_mode_legacy_luna_agent") {
			code = "free_mode_legacy_luna_agent"
			message = "Retired Luna agent — start a new conversation."
		} else {
			code = "session_invalid"
			message = "Session expired or model changed — retry immediately."
		}
		retryAfter = 1 * time.Second
	case errors.As(err, &ue):
		if ue.Retryable {
			// deployment_outside_hours etc.: temporarily unavailable, worth
			// a later retry — 503 lets clients/9router back off instead of
			// treating it as a hard failure.
			status, code = http.StatusServiceUnavailable, "upstream_retryable"
		} else {
			status = ue.Status
			if status != http.StatusPaymentRequired && status != http.StatusConflict && status != http.StatusTooManyRequests {
				status = http.StatusBadGateway
			}
		}
		message = ue.Body
		if message == "" {
			message = "upstream error"
		}
		retryAfter = ue.RetryAfter
	case errors.Is(err, registry.ErrModelNotFound):
		status, code = http.StatusBadRequest, "model_not_found"
		message = err.Error() + "; available: " + strings.Join(s.servedModels(), ", ")
	case errors.Is(err, upstream.ErrAuthRejected):
		status, code = http.StatusBadGateway, "upstream_auth_rejected"
		message = err.Error()
	case errors.Is(err, upstream.ErrWaitingRoom):
		status, code = http.StatusServiceUnavailable, "waiting_room_queued"
		message = err.Error()
	case errors.As(err, &cbe):
		status, code = http.StatusForbidden, "country_blocked"
		message = cbe.Error()
	case errors.Is(err, upstream.ErrFreeModeCLIRequired):
		status, code = http.StatusForbidden, "free_mode_cli_required"
		message = err.Error()
	case errors.As(err, &ce):
		// 402 "Out of credits": surfacing the upstream body verbatim keeps
		// the quota detail (limit/recent/reset) for the client.
		status, code = http.StatusPaymentRequired, "out_of_credits"
		message = ce.Body
		if message == "" {
			message = "out of credits"
		}
	case errors.Is(err, context.DeadlineExceeded):
		status, code = http.StatusGatewayTimeout, "upstream_timeout"
		message = "upstream request timed out: " + err.Error()
	}

	attrs := []any{"status", status, "code", code, "err", err}
	if r != nil {
		if reqID := reqIDFrom(r.Context()); reqID != "" {
			attrs = append(attrs, "req_id", reqID)
		}
	}
	if retryAfter > 0 {
		attrs = append(attrs, "retry_after", int(retryAfter.Seconds()))
	}
	if !resetAt.IsZero() {
		attrs = append(attrs, "reset_at", resetAt.UTC().Format(time.RFC3339))
	}
	if lease != nil {
		attrs = append(attrs, "token", tokenLabel(lease))
	}
	if model != "" {
		attrs = append(attrs, "model", model)
	}

	if code == "rate_limited" {
		// D6 dedupe: identical (token, code, window) logs fire on the 1st +
		// every 50th; the counter always increments and the response is
		// always written.
		key := tokenLabel(lease) + "|" + code + "|" + window
		if !s.rateLimitWarnShouldLog(key) {
			s.writeClientError(w, r, status, message, code, retryAfter)
			return
		}
	}
	// Routine client-caused 429 rate_limited (daily session quota) is
	// expected churn, not an operator-actionable fault: log at Info. Every
	// upstream-class failure stays Warn (5xx, upstream_unavailable, bans;
	// the upstream-class 429 variants carry their own codes above).
	if code == "rate_limited" {
		s.logger.Info("request failed", attrs...)
	} else {
		s.logger.Warn("request failed", attrs...)
	}
	s.writeClientError(w, r, status, message, code, retryAfter)
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h >= 24 {
		dd := h / 24
		hr := h % 24
		if hr > 0 {
			return fmt.Sprintf("%dd %dh", dd, hr)
		}
		return fmt.Sprintf("%dd", dd)
	}
	if h > 0 {
		if m > 0 {
			return fmt.Sprintf("%dh %dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	}
	if m > 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
