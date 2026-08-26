package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/session"
)

// ModelUnavailableMessage formats the rejection error message for
// unserved/disabled models (issue #189). Withdrawn-but-recognized upstream ids
// (registry.PausedModels, e.g. minimax/minimax-m3) get upstream's own
// withdrawn-model copy naming the replacement (freebuffWithdrawnModelMessage)
// instead of the generic supported-list dump: the client that sends that id is
// a released binary whose picker still lists it, and "not available" alone
// leaves the user staring at a row that looks fine and does not work.
func ModelUnavailableMessage(rawModel string) string {
	if registry.IsPausedModel(rawModel) {
		return registry.WithdrawnModelMessage(rawModel)
	}
	return fmt.Sprintf("Model '%s' is not available. Supported models: %s", rawModel, registry.SupportedModelsHelpText)
}

// probeModel returns the safest model to default a smoke test to: the
// fallback default (deepseek-v4-flash — the model every account gets) when
// it is in the catalog, else the first catalog model.
// Alphabetical models[0] would otherwise pick anthropic/claude-fable-5, a
// capacity-gated offer model that makes smoke tests fail on most accounts.
// servedModels returns the registry catalog filtered to the ServedModels
// gate: the ids this gateway actually serves (blocked -max variants and any
// future non-gated registry row excluded). Used for the /v1/models
// surface-equivalent counts (/healthz, /metrics) and the "available:" model
// hints in error bodies, so nothing advertises an id that 404s.
func (s *Server) servedModels() []string {
	all := s.reg.Models()
	out := make([]string, 0, len(all))
	for _, id := range all {
		if registry.ServedModels[id] {
			out = append(out, id)
		}
	}
	return out
}

// servedModelCount reports how many registry models pass the ServedModels
// gate. Mirrors the /v1/models row count (modulo ModelsHideUnavailable).
func (s *Server) servedModelCount() int {
	return len(s.servedModels())
}

// probeModel returns a default model for smoke-test paths: the configured
// fallback when listed, else the first served model.
func probeModel(reg *registry.Registry) string {
	models := reg.Models()
	if len(models) == 0 {
		return ""
	}
	for _, id := range models {
		if id == session.DefaultFallbackModel {
			return id
		}
	}
	return models[0]
}

// modelAllowed reports whether a model may be served. Every model must first
// pass the hardcoded ServedModels gate; then, when MODELS_ALLOW is non-empty,
// the well-known (alias/suffix-resolved) model id must be listed exactly.
func (s *Server) modelAllowed(model string) bool {
	if !registry.ServedModels[model] {
		return false
	}
	cfg := s.cfg.Load()
	allow := cfg.ModelsAllow
	if len(allow) == 0 {
		return true
	}
	for _, id := range allow {
		if id == model {
			return true
		}
	}
	return false
}

// modelListed is the strict listing filter for /v1/models: like modelAllowed
// it gates on ServedModels first; when MODELS_ALLOW is set the catalog
// surface is exactly the allowlist.
func (s *Server) modelListed(model string) bool {
	if !registry.ServedModels[model] {
		return false
	}
	allow := s.cfg.Load().ModelsAllow
	if len(allow) == 0 {
		return true
	}
	for _, id := range allow {
		if id == model {
			return true
		}
	}
	return false
}

// handleModels serves the OpenAI model-list shape with the registry's
// current models; created is pinned to server start so every entry matches.
// Each row carries an advisory availability annotation derived from the pool
// token snapshots (available/status) so clients can surface quota or lock
// signals without probing.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	created := s.started.Unix()
	snaps := s.pool.Snapshot()
	models := s.reg.Models()
	if len(models) == 0 {
		// T16: an empty registry is an operational anomaly (the fallback
		// table should always populate at boot) — surface it when a client
		// actually asks, not at startup.
		s.logger.Warn("model list requested with empty registry", "path", r.URL.Path, "remote", remoteHost(r), "model_count", 0)
	}
	hideUnavailable := s.cfg.Load().ModelsHideUnavailable
	data := make([]map[string]any, 0, len(models))
	for _, id := range models {
		available, status := modelAvailability(id, snaps)
		if hideUnavailable && !available {
			// MODELS_HIDE_UNAVAILABLE=true: prune quota/lock-unavailable
			// models so picker clients never auto-select one. Off by default
			// because a stale signal could hide a working model.
			continue
		}
		if !s.modelListed(id) {
			// MODELS_ALLOW: prune ids outside the operator allowlist so
			// picker clients never auto-select a model that would 404. Uses
			// the strict list (base ids only), so PREFER_MAX_MODELS -max
			// variants stay invisible on the catalog surface.
			continue
		}
		data = append(data, map[string]any{
			"id":        id,
			"object":    "model",
			"created":   created,
			"owned_by":  "freebuff",
			"available": available,
			"status":    status,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// modelAvailability derives the advisory per-model annotation from the pool
// token snapshots. The snapshot does not carry the model of a live session,
// so the signal set is: quotaByModel presence (the session admitted this
// model), quota exhaustion (recent >= limit), and session-level locks.
// available defaults to true when no signal exists, so a working model is
// never hidden.
func modelAvailability(id string, snaps []pool.TokenSnapshot) (available bool, status string) {
	available = true
	status = "unknown"
	quotaHit := false
	quotaExhausted := false
	locked := false
	for _, snap := range snaps {
		switch snap.SessionStatus {
		case "model_locked", "disabled":
			locked = true
		}
		if q, ok := snap.QuotaByModel[id]; ok {
			quotaHit = true
			if q.Limit > 0 && q.RecentCount >= q.Limit {
				quotaExhausted = true
			}
		}
	}
	switch {
	case quotaExhausted:
		status = "quota_exhausted"
	case locked:
		status = "locked"
	case quotaHit:
		status = "available"
	}
	return available, status
}
