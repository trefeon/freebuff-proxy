package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"freebuff-proxy/internal/convert"
)

// completions, Responses, embeddings, model catalog) onto the mux. The
// Anthropic-compatible surface registers separately (registerAnthropicRoutes,
// anthropic.go); shared routes (healthz/metrics/admin) live in
// server.go's Handler.
func (s *Server) registerOpenAIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", s.requireAuth(s.handleChat))
	mux.HandleFunc("POST /v1/responses", s.requireAuth(s.handleResponses))
	mux.HandleFunc("POST /v1/embeddings", s.requireAuth(s.handleEmbeddings))
	mux.HandleFunc("GET /v1/models", s.requireAuth(s.handleModels))
	mux.HandleFunc("GET /v1/models/{model...}", s.requireAuth(s.handleModelRetrieve))
}

// handleChat is the OpenAI chat-completions entry point: sanitize the
// request, then route through chatCore with the chat wire format.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.writeJSONError(w, http.StatusRequestEntityTooLarge,
				"request body exceeds the 32MB limit", "invalid_request_error", "content_too_large", 0)
		} else {
			s.writeJSONError(w, http.StatusBadRequest,
				"failed to read request body: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		}
		return
	}

	// The raw map decides the response mode (stream) before sanitization.
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object", "invalid_request_error", "invalid_json", 0)
		return
	}
	rawModel, _ := raw["model"].(string)
	if rawModel == "" {
		s.writeJSONError(w, http.StatusBadRequest,
			"missing required field \"model\"; available: "+strings.Join(s.servedModels(), ", "),
			"invalid_request_error", "model_not_found", 0)
		return
	}
	model := s.reg.ResolveModel(rawModel)
	if !s.modelAllowed(model) {
		s.writeJSONError(w, http.StatusBadRequest,
			ModelUnavailableMessage(rawModel), "invalid_request_error", "model_unavailable", 0)
		return
	}
	stream := false
	if v, ok := raw["stream"].(bool); ok {
		stream = v
	}
	normalized, err := convert.NormalizeRequest(body, model)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	var relay relayFunc
	if stream {
		relay = s.relayStream
	} else {
		relay = s.relayJSON
	}
	s.chatCore(w, r, model, stream, normalized, convert.ExtractReasoningEffort(raw), "chat", relay)
}

// handleEmbeddings answers POST /v1/embeddings with a structured
// unsupported-endpoint error: the proxy serves chat completions only, and
// the error body points clients at /v1/chat/completions and the live model
// list so a picker/fallback client can self-correct. 400 with the
// documented "unsupported_endpoint" code (distinct from the mux's bare 404,
// which gives an embeddings client no actionable signal).
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.logger.Warn("unsupported endpoint requested", "path", r.URL.Path, "remote", remoteHost(r), "status", http.StatusBadRequest)
	s.writeJSONError(w, http.StatusBadRequest,
		"this proxy serves chat completions only; embeddings are not supported. Use POST /v1/chat/completions with one of: "+strings.Join(s.servedModels(), ", "),
		"unsupported_endpoint", "unsupported_endpoint", 0)
}

// handleModelRetrieve answers GET /v1/models/{model} for clients querying a single model.
func (s *Server) handleModelRetrieve(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("model")
	if modelName == "" {
		s.writeJSONError(w, http.StatusBadRequest, "missing model name in path", "invalid_request_error", "model_not_found", 0)
		return
	}
	model := s.reg.ResolveModel(modelName)
	if !s.modelAllowed(model) {
		s.writeJSONError(w, http.StatusNotFound, "The model '"+modelName+"' does not exist", "invalid_request_error", "model_not_found", 0)
		return
	}
	created := s.started.Unix()
	snaps := s.pool.Snapshot()
	available, status := modelAvailability(model, snaps)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":        modelName,
		"object":    "model",
		"created":   created,
		"owned_by":  "freebuff",
		"available": available,
		"status":    status,
	})
}
