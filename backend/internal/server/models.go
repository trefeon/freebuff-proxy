package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"freebuff-proxy/backend/internal/modelcat"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/session"
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

// probeModel returns a default model for smoke-test paths: the guaranteed
// fallback (deepseek-v4-flash — the model every account gets) when registered
// and served, else the catalog default (modelcat.DefaultModelID, the picker
// lead the upstream CLI resolves a blank pick to), else the first SERVED
// model. Never alphabetical models[0] alone: that would pick
// anthropic/claude-fable-5, a capacity-gated offer model that makes smoke
// tests fail on most accounts. The served gating means probes never target
// an id the gateway itself would refuse.
func probeModel(reg *registry.Registry) string {
	models := reg.Models()
	if len(models) == 0 {
		return ""
	}
	for _, id := range models {
		if id == session.DefaultFallbackModel && registry.ServedModels[id] {
			return id
		}
	}
	for _, id := range models {
		if id == modelcat.DefaultModelID && registry.ServedModels[id] {
			return id
		}
	}
	for _, id := range models {
		if registry.ServedModels[id] {
			return id
		}
	}
	return ""
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
// signals without probing. A Codex client (request carrying a client_version
// query param) instead gets strict ModelInfo rows under the {"models": […]}
// envelope — see codexClientVersion/codexModelRow and WIRE-NOTES.md §8.
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
	codexReq := codexClientVersion(r)
	hideUnavailable := s.cfg.Load().ModelsHideUnavailable
	tier := currentAccessTier(snaps)
	data := make([]map[string]any, 0, len(models))
	listed := make([]string, 0, len(models))
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
		row := map[string]any{
			"id":        id,
			"object":    "model",
			"created":   created,
			"owned_by":  "freebuff",
			"available": available,
			"status":    status,
		}
		if tier != "" {
			row["current_access_tier"] = tier
		}
		data = append(data, row)
		listed = append(listed, id)
	}
	w.Header().Set("Content-Type", "application/json")
	if codexReq {
		// Codex deserializes strict ModelInfo rows under the {"models": […]}
		// envelope (codex-rs codex-api/src/endpoint/models.rs:70 with the
		// protocol ModelsResponse wrapper, protocol/src/openai_models.rs:745-
		// 750); the {"object":"list","data":…} OpenAI shape errors on the
		// missing `models` field and codex silently falls back to its bundled
		// catalog (reference/harnesses/codex/WIRE-NOTES.md §8).
		rows := make([]codexModelInfo, 0, len(listed))
		for _, id := range listed {
			rows = append(rows, codexModelRow(id))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": rows})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// codexClientVersion reports whether a /v1/models request comes from Codex:
// codex-rs appends client_version=… to the models URL
// (codex-rs/model-provider/src/models_endpoint.rs:20,31-35 →
// codex-api/src/endpoint/models.rs:31-35), and only that client understands
// the strict ModelInfo shape below. Everything else keeps the OpenAI shape.
func codexClientVersion(r *http.Request) bool {
	return r.URL.Query().Get("client_version") != ""
}

// codexReasoningEffortPreset mirrors codex-rs protocol/src/openai_models.rs
// ReasoningEffortPreset (lines 187-190): an effort rung plus a short human
// description.
type codexReasoningEffortPreset struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

// codexTruncationPolicy mirrors protocol/src/openai_models.rs
// TruncationPolicyConfig (lines 352-360): mode is only bytes/tokens — there
// is no "disabled" variant.
type codexTruncationPolicy struct {
	Mode  string `json:"mode"`
	Limit int64  `json:"limit"`
}

// codexModelInfo mirrors the serde-required ModelInfo fields of codex-rs
// protocol/src/openai_models.rs:392-483: every non-Option field WITHOUT
// #[serde(default)] must be present, plus base_instructions — the legacy-base
// deserializer wrapper rejects any row missing both base_instructions and
// model_messages.instructions_template (openai_models.rs:791-822). A minimal
// {id,…} row fails serde and codex silently falls back to its bundled catalog
// (reference/harnesses/codex/WIRE-NOTES.md §8), so each fixed value below is
// the minimal honest choice.
type codexModelInfo struct {
	Slug                       string                       `json:"slug"`
	DisplayName                string                       `json:"display_name"`
	SupportedReasoningLevels   []codexReasoningEffortPreset `json:"supported_reasoning_levels"`
	ShellType                  string                       `json:"shell_type"`
	Visibility                 string                       `json:"visibility"`
	SupportedInAPI             bool                         `json:"supported_in_api"`
	Priority                   int                          `json:"priority"`
	SupportVerbosity           bool                         `json:"support_verbosity"`
	TruncationPolicy           codexTruncationPolicy        `json:"truncation_policy"`
	ExperimentalSupportedTools []string                     `json:"experimental_supported_tools"`
	InputModalities            []string                     `json:"input_modalities"`
	BaseInstructions           string                       `json:"base_instructions"`
}

// codexKnownEfforts is the effort-ladder filter set: the fixed rungs
// ReasoningEffort::from_str maps without falling into Custom
// (openai_models.rs:131-146). "max" is a known value — codex's own bundled
// catalog advertises it (app-server/tests/suite/v2/model_list.rs:172-176).
var codexKnownEfforts = map[string]bool{
	"minimal": true, "low": true, "medium": true, "high": true,
	"xhigh": true, "max": true, "ultra": true,
}

// codexReasoningEfforts maps a catalog effort ladder to Codex wire presets,
// filtered to codexKnownEfforts. Models with no catalog ladder (routes that
// accept and ignore reasoning_effort) follow the convert package's default
// ladder (backend/internal/convert/effort.go:120-126). The result is never
// nil so it marshals as [] and not null — the Vec is serde-required.
func codexReasoningEfforts(id string) []codexReasoningEffortPreset {
	ladder := modelcat.Efforts(id)
	if ladder == nil {
		ladder = []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
	}
	out := make([]codexReasoningEffortPreset, 0, len(ladder))
	for _, e := range ladder {
		if !codexKnownEfforts[e] {
			continue
		}
		out = append(out, codexReasoningEffortPreset{Effort: e, Description: e})
	}
	return out
}

// codexModelRow renders one served model id as a Codex ModelInfo row.
// Fixed-value ground truth: shell_type "unified_exec" (ConfigShellToolType
// has no bash variant — unified_exec/disabled with aliases default/local/
// shell_command, openai_models.rs:302-316), visibility "list"
// (ModelVisibility is list/hide/none — no "managed", openai_models.rs:279-
// 290), support_verbosity is a bool (openai_models.rs:421), truncation_policy
// limit is the catalog context window (ContextWindow falls back to
// DefaultContextWindow), input_modalities text+image matches the client's own
// default (openai_models.rs:168-182), base_instructions "" (we ship no model
// instructions).
func codexModelRow(id string) codexModelInfo {
	return codexModelInfo{
		Slug:                       id,
		DisplayName:                modelcat.DisplayName(id),
		SupportedReasoningLevels:   codexReasoningEfforts(id),
		ShellType:                  "unified_exec",
		Visibility:                 "list",
		SupportedInAPI:             true,
		Priority:                   0,
		SupportVerbosity:           true,
		TruncationPolicy:           codexTruncationPolicy{Mode: "tokens", Limit: int64(modelcat.ContextWindow(id))},
		ExperimentalSupportedTools: []string{},
		InputModalities:            []string{"text", "image"},
		BaseInstructions:           "",
	}
}

// currentAccessTier resolves the effective access tier across the pool snapshots.
// Returns "full", "limited", "free", or "" if no token has reported an access tier yet.
func currentAccessTier(snaps []pool.TokenSnapshot) string {
	hasLimited := false
	for _, snap := range snaps {
		switch snap.AccessTier {
		case "full":
			return "full"
		case "free":
			return "free"
		case "limited":
			hasLimited = true
		}
	}
	if hasLimited {
		return "limited"
	}
	return ""
}

// isModelAllowedForTier reports whether id can be served under tier.
// On limited tier, only limited-tier models (mimo-v2.5) or GLM 5.2 with active referral quota
// can be admitted.
func isModelAllowedForTier(id, tier string, snaps []pool.TokenSnapshot) bool {
	if tier != "limited" {
		return true
	}
	if modelcat.IsLimitedTierAllowed(id) {
		return true
	}
	if id == modelcat.Glm52ModelID {
		for _, snap := range snaps {
			if q, ok := snap.QuotaByModel[id]; ok && q.Limit > 0 && q.RecentCount < q.Limit {
				return true
			}
		}
	}
	return false
}

// modelAvailability derives the advisory per-model annotation from the pool
// token snapshots. The snapshot does not carry the model of a live session,
// so the signal set is: accessTier (limited tier marks non-mimo models as
// region_limited), quotaByModel presence (the session admitted this
// model), quota exhaustion (recent >= limit), and session-level locks.
// available defaults to true when no signal exists, so a working model is
// never hidden.
func modelAvailability(id string, snaps []pool.TokenSnapshot) (available bool, status string) {
	available = true
	status = "unknown"
	quotaHit := false
	quotaExhausted := false
	locked := false
	tier := currentAccessTier(snaps)
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
	case tier == "limited" && !isModelAllowedForTier(id, tier, snaps):
		available = false
		status = "region_limited"
	case quotaExhausted:
		status = "quota_exhausted"
	case locked:
		status = "locked"
	case quotaHit:
		status = "available"
	}
	return available, status
}
