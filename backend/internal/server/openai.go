package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"freebuff-proxy/backend/internal/convert"
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
	if msg := validateChatUnsupportedParams(raw); msg != "" {
		s.writeJSONError(w, http.StatusBadRequest, msg, "invalid_request_error", "invalid_request_error", 0)
		return
	}
	stream := false
	if v, ok := raw["stream"].(bool); ok {
		stream = v
	}
	normalized, _, err := convert.NormalizeRequestMappedOpts(body, model, s.convertOptions())
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	r = r.WithContext(withOriginalBody(r.Context(), body)) // #140: response-side restore map
	var relay relayFunc
	if stream {
		relay = s.relayStream
	} else {
		relay = s.relayJSON
	}
	s.chatCore(w, r, model, stream, normalized, convert.ExtractReasoningEffort(raw), "chat", relay)
}

// validateChatUnsupportedParams returns an error message (or "") for
// chat-completions parameters the gateway cannot honor. Feature-flagged
// parameters with no upstream mapping MUST fail loudly instead of being
// dropped silently by the whitelist — the client asked for behavior the
// gateway does not implement.
//
//   - n: the upstream generates exactly one choice; n>1 would need
//     multi-choice fan-out the gateway cannot provide.
//   - audio: audio output has no chat-completion analogue upstream.
//   - web_search_options / moderation: built-in web search and moderation
//     are not implemented by the upstream chat endpoint.
//   - allowed_tools / non-function tools / non-function tool_choice: tool
//     allow-listing and custom/built-in tool types have no upstream
//     mapping; silently dropping allowed_tools would lift a client-side
//     restriction, so all three fail loudly.
//
// Params that stay whitelisted passthrough (mapped, documented): logit_bias,
// logprobs, top_logprobs, response_format, seed, store, stream_options,
// service_tier, modalities, metadata, penalties — every one is an OpenAI
// chat param the FreeBuff chat endpoint accepts.
func validateChatUnsupportedParams(raw map[string]any) string {
	if n, ok := raw["n"].(float64); ok && n != 1 {
		return fmt.Sprintf("unsupported parameter \"n\": only n=1 is supported (got n=%v); the upstream generates exactly one choice", n)
	}
	if v, ok := raw["audio"]; ok && v != nil {
		return "unsupported parameter \"audio\": audio output is not supported by this gateway"
	}
	if v, ok := raw["web_search_options"]; ok && v != nil {
		return "unsupported parameter \"web_search_options\": built-in web search is not supported by this gateway"
	}
	if v, ok := raw["moderation"]; ok && v != nil {
		return "unsupported parameter \"moderation\": request moderation is not supported by this gateway"
	}
	if v, ok := raw["allowed_tools"]; ok && v != nil {
		return "unsupported parameter \"allowed_tools\": tool allow-listing is not supported by this gateway; declare only the tools the model may call"
	}
	if tools, ok := raw["tools"].([]any); ok {
		for _, item := range tools {
			if tm, ok := item.(map[string]any); ok {
				if typ, _ := tm["type"].(string); typ != "" && typ != "function" {
					return fmt.Sprintf("unsupported parameter \"tools\": tool type %q is not supported by this gateway (only function tools translate)", typ)
				}
			}
		}
	}
	switch tc := raw["tool_choice"].(type) {
	case nil:
		// Absent: model decides.
	case string:
		switch tc {
		case "none", "auto", "required":
			// Valid ChatCompletionToolChoiceOption strings.
		default:
			return fmt.Sprintf("unsupported parameter \"tool_choice\": value %q is not supported by this gateway (want none, auto, required, or a named function choice)", tc)
		}
	case map[string]any:
		if typ, _ := tc["type"].(string); typ != "" && typ != "function" {
			return fmt.Sprintf("unsupported parameter \"tool_choice\": tool choice type %q is not supported by this gateway (only named function choices translate)", typ)
		}
	default:
		return "unsupported parameter \"tool_choice\": must be a string or a named function choice object"
	}
	return ""
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
	row := map[string]any{
		"id":        modelName,
		"object":    "model",
		"created":   created,
		"owned_by":  "freebuff",
		"available": available,
		"status":    status,
	}
	if tier := currentAccessTier(snaps); tier != "" {
		row["current_access_tier"] = tier
	}
	_ = json.NewEncoder(w).Encode(row)
}
