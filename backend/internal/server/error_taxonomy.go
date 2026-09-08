package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
)

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
		return "Upstream refused the request (rate limit). Honor Retry-After before retrying; persistent refusals mean the account's upstream pool is spent."
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

// chatErrClass buckets an upstream error into the trace error column. A
// canceled downstream client gets its own bucket: the access line keeps a
// 200 default when nothing was (or could be) written, so a generic "error"
// would render a context-free "ERROR 200" on the dashboard.
func chatErrClass(err error) string {
	if errors.Is(err, context.Canceled) {
		return "client_canceled"
	}
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

// quotaSummary renders the live per-model session quota from a probe's
// isAnthropicRequest reports whether the incoming request is destined for the
// Anthropic Messages surface (/v1/messages) or carries Anthropic headers.
func isAnthropicRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		return true
	}
	if r.Header.Get("anthropic-version") != "" || r.Header.Get("anthropic-api-key") != "" {
		return true
	}
	return false
}

// anthropicErrorType maps HTTP status code and internal error code to standard
// Anthropic error types per reference/protocols/anthropic-sdk-typescript.
func anthropicErrorType(status int, code string) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusServiceUnavailable && (code == "waiting_room_queued" || code == "waiting_room_required" || code == "capacity_deferred"):
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}
