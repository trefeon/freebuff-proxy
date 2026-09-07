package registry

import (
	"fmt"
	"strings"

	"freebuff-proxy/backend/internal/config"
)

// ResolveModel resolves an alias (e.g. "gpt-4o") to its real model ID if mapped
// in cfg.ModelAliases, and strips reasoning-effort / context suffixes (e.g.
// "(max)", "(high)", ":max") so the bare upstream id is sent on the wire.
// The proxy NEVER auto-upgrades base models to their -max extended-context
// variants: those are per-account upstream provisions (unprovisioned accounts
// are coerced upstream), so a client that holds a -max grant requests the id
// literally.
func (r *Registry) ResolveModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}

	if strings.HasSuffix(model, ")") {
		if idx := strings.LastIndex(model, "("); idx > 0 {
			tag := strings.ToLower(strings.TrimSpace(model[idx+1 : len(model)-1]))
			switch tag {
			case "max", "high", "medium", "low", "minimal", "xhigh", "ultra":
				model = strings.TrimSpace(model[:idx])
			}
		}
	} else if idx := strings.LastIndex(model, ":"); idx > 0 {
		tag := strings.ToLower(strings.TrimSpace(model[idx+1:]))
		switch tag {
		case "max", "high", "medium", "low", "minimal", "xhigh", "ultra":
			model = strings.TrimSpace(model[:idx])
		}
	}

	// Claude Code's extended-context marker (reference/agents/claude-code):
	// the CLI appends "[1m]" to models it believes support the 1M-context beta
	// and mirrors the capability in anthropic-beta. The marker is a client-side
	// context hint, not part of the upstream model id — strip it so alias
	// lookup and the served-model gate resolve the bare id. Mirrors 9router's
	// stripModelContextMarker (reference/agents/9router).
	if strings.HasSuffix(model, "]") {
		if idx := strings.LastIndex(model, "["); idx > 0 {
			tag := strings.ToLower(strings.TrimSpace(model[idx+1 : len(model)-1]))
			switch tag {
			case "1m", "200k":
				model = strings.TrimSpace(model[:idx])
			}
		}
	}

	var cfg *config.Config
	if r != nil {
		cfg = r.cfg.Load()
	}
	if cfg != nil && len(cfg.ModelAliases) > 0 {
		if realModel, ok := cfg.ModelAliases[model]; ok && realModel != "" {
			model = realModel
		}
	}

	return model
}

// AgentForModel returns the agent id that serves model (after resolving aliases),
// or an ErrModelNotFound-wrapped error.
func (r *Registry) AgentForModel(model string) (string, error) {
	model = r.ResolveModel(model)
	r.mu.RLock()
	defer r.mu.RUnlock()
	agent, ok := r.modelToAgent[model]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	return agent, nil
}
