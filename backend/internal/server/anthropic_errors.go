package server

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

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

// writeAnthropicError writes an Anthropic-formatted error response:
// {"type": "error", "error": {"type": "...", "message": "...", "code": "..."}}
func (s *Server) writeAnthropicError(w http.ResponseWriter, r *http.Request, status int, message, code string, retryAfter time.Duration) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	version := "2023-06-01"
	if r != nil {
		if reqVer := r.Header.Get("anthropic-version"); reqVer != "" {
			version = reqVer
		}
	}
	h.Set("anthropic-version", version)
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
	}
	w.WriteHeader(status)
	errType := anthropicErrorType(status, code)
	errMap := map[string]any{
		"type":    errType,
		"message": message,
	}
	if code != "" {
		errMap["code"] = code
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": errMap,
	})
}
