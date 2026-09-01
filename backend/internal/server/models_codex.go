package server

import (
	"net/http"

	"freebuff-proxy/backend/internal/modelcat"
)

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
