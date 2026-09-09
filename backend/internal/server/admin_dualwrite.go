package server

import (
	"errors"
	"fmt"
	"os"

	"freebuff-proxy/backend/internal/config"
)

// tokenMarkerKey is the settings-table presence marker for the AUTH_TOKENS
// pool. Raw tokens never reach the store: the marker carries zero secret
// material ("true" while a pooled .env persists, absent in bridge mode), so
// token add/remove/swap/mode-switch still participate in the dual-layer
// persist without violating the no-raw-tokens rule. AUTH_TOKENS and
// ADMIN_TOKEN themselves stay .env-only and never enter the settings table.
const tokenMarkerKey = "auth/tokens_configured"

// dualPhase names which layer of a dualWrite failed, so callers keep their
// existing per-phase messages (persist vs reload vs divergence) while the
// snapshot/write/rollback sequence lives in exactly one place.
type dualPhase int

const (
	dualPersistPhase dualPhase = iota
	dualSettingsPhase
	dualReloadPhase
)

// dualError wraps a persist/settings/reload failure. Divergence rejections
// from verify pass through unwrapped so the caller's conflict text survives.
type dualError struct {
	phase dualPhase
	err   error
}

func (e *dualError) Error() string { return e.err.Error() }
func (e *dualError) Unwrap() error { return e.err }

// dualPhaseOf reports the failing layer of a dualWrite error (verify
// rejections report ok=false — they are caller errors, not layer failures).
func dualPhaseOf(err error) (dualPhase, bool) {
	var de *dualError
	if errors.As(err, &de) {
		return de.phase, true
	}
	return 0, false
}

// dualWrite persists .env line edits AND settings-table rows as one
// write-through unit, then reloads through loadConfig (file + overlay + env)
// and runs verify against the reloaded config. Any failure restores BOTH
// layers: the .env bytes and every snapshotted settings row.
//
//   - set maps full settings row keys (config.OverlayRowKey(...) or the
//     auth/ marker namespace) to their new values; del lists row keys to
//     drop. Both are skipped when no settings store is wired (degraded
//     live-only boot keeps pure-.env behavior).
//   - verify may reject the reloaded config (divergence guard); the
//     rejection rolls both layers back and returns unwrapped.
//
// Callers must hold adminSaveMu.
func (a *adminHandlers) dualWrite(env []config.EnvUpdate, set map[string]string, del []string, verify func(config.Config) error) (config.Config, error) {
	oldEnv, oldEnvErr := os.ReadFile(config.EnvFileForWrite())

	// Snapshot the settings rows we are about to touch, so a later failure
	// restores both layers instead of leaving settings ahead of .env.
	var snap map[string]*string
	if a.settings != nil {
		snap = make(map[string]*string, len(set)+len(del))
		for k := range set {
			v, ok, err := a.settings.GetSetting(k)
			if err != nil {
				return config.Config{}, &dualError{dualSettingsPhase, fmt.Errorf("read setting %s: %w", k, err)}
			}
			if ok {
				v := v
				snap[k] = &v
			} else {
				snap[k] = nil
			}
		}
		for _, k := range del {
			if _, done := snap[k]; done {
				continue
			}
			v, ok, err := a.settings.GetSetting(k)
			if err != nil {
				return config.Config{}, &dualError{dualSettingsPhase, fmt.Errorf("read setting %s: %w", k, err)}
			}
			if ok {
				v := v
				snap[k] = &v
			} else {
				snap[k] = nil
			}
		}
	}

	if _, err := updateEnvKeys(env); err != nil {
		return config.Config{}, &dualError{dualPersistPhase, err}
	}
	if a.settings != nil {
		for k, v := range set {
			if err := a.settings.SetSetting(k, v); err != nil {
				restoreEnvFile(oldEnv, oldEnvErr)
				return config.Config{}, &dualError{dualSettingsPhase, fmt.Errorf("persist setting %s: %w", k, err)}
			}
		}
		for _, k := range del {
			if err := a.settings.DeleteSetting(k); err != nil {
				restoreEnvFile(oldEnv, oldEnvErr)
				a.restoreSettingRows(snap)
				return config.Config{}, &dualError{dualSettingsPhase, fmt.Errorf("delete setting %s: %w", k, err)}
			}
		}
	}
	newCfg, err := a.loadConfig()
	if err != nil {
		restoreEnvFile(oldEnv, oldEnvErr)
		a.restoreSettingRows(snap)
		return config.Config{}, &dualError{dualReloadPhase, err}
	}
	if verify != nil {
		if err := verify(newCfg); err != nil {
			restoreEnvFile(oldEnv, oldEnvErr)
			a.restoreSettingRows(snap)
			return config.Config{}, err
		}
	}
	return newCfg, nil
}

// restoreSettingRows rolls settings rows back to their dualWrite snapshot
// (nil store or nil snapshot = nothing was written, no-op). Restore is
// best-effort: the .env is already restored first, so a row that fails to
// roll back leaves the overlay shadowing the file — loud, and clearable via
// DELETE /admin/api/settings/:key, instead of a silently diverged file.
func (a *adminHandlers) restoreSettingRows(snap map[string]*string) {
	if a.settings == nil || len(snap) == 0 {
		return
	}
	for k, prior := range snap {
		var err error
		if prior == nil {
			err = a.settings.DeleteSetting(k)
		} else {
			err = a.settings.SetSetting(k, *prior)
		}
		if err != nil {
			a.logfunc().Warn("dual-write settings rollback failed", "key", k, "err", err)
		}
	}
}

// tokenMarkerDelta maps a post-mutation AUTH_TOKENS list to its settings
// write-through: marker set while pooled, marker dropped in bridge mode.
func tokenMarkerDelta(tokens []string) (set map[string]string, del []string) {
	set = map[string]string{}
	if len(tokens) > 0 {
		set[tokenMarkerKey] = "true"
		return set, nil
	}
	return set, []string{tokenMarkerKey}
}

// errBridgeStillEnabled is the divergence rejection when a mode switch
// cannot move the effective BRIDGE_ENABLED: pooled=true means hybrid→pooled
// left the bridge on, pooled=false means pooled→hybrid left it off. It
// carries no message text — the caller renders the overlay-aware conflict
// (overlay row vs environment/JSON) exactly like before.
type errBridgeStillEnabled struct{ pooled bool }

func (e errBridgeStillEnabled) Error() string {
	if e.pooled {
		return "bridge still enabled"
	}
	return "bridge still disabled"
}

// dualPersistMessage maps a dualWrite layer failure to the historic
// persist/reload response text, so converted handlers keep byte-identical
// diagnostics for the .env and reload phases.
func dualPersistMessage(err error) string {
	if phase, ok := dualPhaseOf(err); ok {
		switch phase {
		case dualReloadPhase:
			return "Reload rejected: " + err.Error()
		case dualSettingsPhase:
			return "Failed to persist settings: " + err.Error()
		default:
			return "Failed to persist .env: " + err.Error()
		}
	}
	return err.Error()
}

// errAdminTokenOverridden is the divergence rejection when a password change
// cannot move the effective ADMIN_TOKEN (process env or -config JSON wins).
// The handler renders the full conflict text; the sentinel only threads the
// branch through dualWrite's verify.
var errAdminTokenOverridden = errors.New("admin token overridden")

// errRequireLoginShadowed is the divergence rejection when a require-login
// toggle cannot move the effective DASHBOARD_REQUIRE_LOGIN. The handler
// renders the overlay-aware conflict; the sentinel only threads the branch.
var errRequireLoginShadowed = errors.New("require login shadowed")
