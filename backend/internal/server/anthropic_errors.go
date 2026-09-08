package server

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"
)

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
