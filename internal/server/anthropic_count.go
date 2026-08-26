package server

// Anthropic /v1/messages/count_tokens surface: the token-estimation handler
// with the tokenizer integration (tokenestimate).

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/tokenestimate"
)

func (s *Server) handleMessagesCountTokens(w http.ResponseWriter, r *http.Request) {
	version := "2023-06-01"
	if reqVer := r.Header.Get("anthropic-version"); reqVer != "" {
		version = reqVer
	}
	w.Header().Set("anthropic-version", version)

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.writeAnthropicError(w, r, http.StatusRequestEntityTooLarge,
				"request body exceeds the 32MB limit", "content_too_large", 0)
		} else {
			s.writeAnthropicError(w, r, http.StatusBadRequest,
				"failed to read request body: "+err.Error(), "invalid_json", 0)
		}
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		s.writeAnthropicError(w, r, http.StatusBadRequest,
			"request body must be a valid JSON object", "invalid_json", 0)
		return
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		s.writeAnthropicError(w, r, http.StatusBadRequest,
			"request body must be a valid JSON object", "invalid_json", 0)
		return
	}
	rawModel, _ := raw["model"].(string)
	if rawModel == "" {
		s.writeAnthropicError(w, r, http.StatusBadRequest,
			"missing required field \"model\"; available: "+strings.Join(s.servedModels(), ", "),
			"model_not_found", 0)
		return
	}
	model := s.reg.ResolveModel(rawModel)
	// Paused ids stay RECOGNIZED here (issue #140 drift): count_tokens is a
	// local estimate with no upstream admission, so a released client probing
	// its still-listed picker id gets a number, not a refusal. Only the chat
	// surfaces refuse paused models.
	if !s.modelAllowed(model) && !registry.IsPausedModel(model) {
		s.writeAnthropicError(w, r, http.StatusBadRequest,
			ModelUnavailableMessage(rawModel), "invalid_request_error", 0)
		return
	}
	if s.tokenEstimator == nil {
		s.writeAnthropicError(w, r, http.StatusServiceUnavailable, "token estimation unavailable", "upstream_unavailable", 0)
		return
	}
	count, err := s.tokenEstimator.CountAnthropicRequest(raw)
	if err != nil {
		code := "invalid_request_error"
		if errors.Is(err, tokenestimate.ErrDocument) || strings.Contains(err.Error(), "document") {
			code = "unsupported_content"
		}
		s.writeAnthropicError(w, r, http.StatusBadRequest, err.Error(), code, 0)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"input_tokens": count})
}
