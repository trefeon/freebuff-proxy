package pool

import (
	"fmt"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/registry"
)

// Model-allowlist routing (MODEL_LOCKS, issue #325): each pool slot may be
// pinned to a set of model ids. Slots without an entry serve any model
// (today's behavior, fully backward compatible).

// lockedOutByModel reports whether the slot at idx must not serve model:
// the slot has a lock entry and neither the requested id nor its registry
// canonical form is listed. A nil registry (or nil config) disables
// matching to exact strings; a nil/empty lock map never locks anything out.
func lockedOutByModel(cfg *config.Config, reg *registry.Registry, idx int, model string) bool {
	if cfg == nil || len(cfg.ModelLocks) == 0 {
		return false
	}
	allowed, ok := cfg.ModelLocks[idx]
	if !ok || len(allowed) == 0 {
		return false
	}
	canon := model
	if reg != nil {
		canon = reg.ResolveModel(model)
	}
	for _, entry := range allowed {
		if model == entry {
			return false
		}
		if reg != nil && canon == reg.ResolveModel(entry) {
			return false
		}
	}
	return true
}

// allLockedOut reports whether every slot is locked away from model (each
// slot carries a lock entry and none covers the model). Callers fail fast
// with lockFailFastError instead of burning quota on doomed admissions.
func allLockedOut(toks *[]*tokenEntry, cfg *config.Config, reg *registry.Registry, model string) bool {
	if toks == nil || len(*toks) == 0 || cfg == nil || len(cfg.ModelLocks) == 0 {
		return false
	}
	for idx := range *toks {
		if !lockedOutByModel(cfg, reg, idx, model) {
			return false
		}
	}
	return true
}

// lockFailFastError is the client-facing error when no pool account is
// locked to the requested model. It names the model and states that no
// upstream admission was attempted, so operators recognize a routing
// misconfiguration instead of an upstream outage.
func lockFailFastError(model string, slots int) error {
	return fmt.Errorf("pool: no account locked to model %q (all %d pool slot(s) allowlist other models); no upstream admission attempted — adjust MODEL_LOCKS", model, slots)
}
