package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/modelcat"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func tokenActionID(r *http.Request) (int, error) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 0 {
		return 0, errors.New("invalid token id")
	}
	return id, nil
}

// parseTokenIndex reads a 0-based token index from the request: a form field
// first, then a JSON body (number or quoted string) for SPA postAPI callers
// whose application/json FormValue never parses. keys lists the accepted
// parameter names in priority order ("token", then "index").
//
// It returns idx=-1, ok=true when the parameter is absent (legacy
// last-token behavior for callers that send none), and ok=false when present
// but unparsable or out of [0, count).
func parseTokenIndex(w http.ResponseWriter, r *http.Request, keys []string, count int) (idx int, ok bool) {
	raw := ""
	for _, k := range keys {
		if v := strings.TrimSpace(r.FormValue(k)); v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
		if err == nil && len(bytes.TrimSpace(body)) > 0 {
			var jreq map[string]json.RawMessage
			if jerr := json.Unmarshal(body, &jreq); jerr == nil {
				for _, k := range keys {
					msg, found := jreq[k]
					if !found || len(msg) == 0 {
						continue
					}
					var n int
					if uerr := json.Unmarshal(msg, &n); uerr == nil {
						raw = strconv.Itoa(n)
					} else {
						var s string
						if serr := json.Unmarshal(msg, &s); serr == nil {
							raw = strings.TrimSpace(s)
						}
					}
					if raw != "" {
						break
					}
				}
			}
		}
	}
	if raw == "" {
		return -1, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 || n >= count {
		return -1, false
	}
	return n, true
}

// removeAtCopy returns a copy of s with element i dropped. The input is never
// mutated: appending on s[:i] in place would clobber the shared backing array
// (e.g. the live config's AUTH_TOKENS slice).
func removeAtCopy(s []string, i int) []string {
	out := append([]string{}, s...)
	if i < 0 || i >= len(out) {
		return out
	}
	return append(out[:i], out[i+1:]...)
}

func (a *adminHandlers) handleTokenUnlock(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err == nil {
		err = a.pool.UnlockToken(id)
	}
	if err != nil {
		a.dash.RenderConfigResult(w, r, false, "Unlock failed: "+err.Error())
		return
	}
	a.logfunc().Info("dashboard token unlocked", "token", id)
	a.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" unlocked — no cooldown or ban window remains.")
}

func (a *adminHandlers) handleTokenLock(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err == nil {
		err = a.pool.LockToken(id)
	}
	if err != nil {
		a.dash.RenderConfigResult(w, r, false, "Lock failed: "+err.Error())
		return
	}
	a.logfunc().Info("dashboard token locked", "token", id)
	a.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" locked — it will not be used for new requests.")
}

func (a *adminHandlers) handleTokenUnlockLock(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err == nil {
		err = a.pool.UnlockLockToken(id)
	}
	if err != nil {
		a.dash.RenderConfigResult(w, r, false, "Unlock failed: "+err.Error())
		return
	}
	a.logfunc().Info("dashboard token unlocked (admin)", "token", id)
	a.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" unlocked — it is available for requests again.")
}

func (a *adminHandlers) handleTokenFinish(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err == nil {
		err = a.pool.FinishTokenRuns(r.Context(), id)
	}
	if err != nil {
		a.dash.RenderConfigResult(w, r, false, "Finish failed: "+err.Error())
		return
	}
	a.logfunc().Info("dashboard token runs finished", "token", id)
	a.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" runs finished.")
}

func (a *adminHandlers) handleTokenDropSession(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		err = a.pool.DropTokenSession(ctx, id)
	}
	if err != nil {
		a.dash.RenderConfigResult(w, r, false, "Drop session failed: "+err.Error())
		return
	}
	a.logfunc().Info("dashboard token session dropped", "token", id)
	a.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" session dropped — next request will re-admit fresh.")
}

// spawnModelFromRequest reads the spawn model id from a form field or a JSON
// body (SessionSpawnPanel posts JSON via postAPI; Go FormValue never parses
// a JSON body, so without the fallback the picker was silently ignored and
// every spawn fell back to the default model).
func spawnModelFromRequest(r *http.Request) string {
	model := strings.TrimSpace(r.FormValue("model"))
	if model != "" {
		return model
	}
	var req struct {
		Model string `json:"model"`
	}
	if body, err := io.ReadAll(r.Body); err == nil {
		_ = json.Unmarshal(body, &req)
	}
	return strings.TrimSpace(req.Model)
}

func (a *adminHandlers) handleTokenSpawnSession(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err != nil {
		a.dash.RenderConfigResult(w, r, false, "Invalid token ID: "+err.Error())
		return
	}
	// Cap the body before FormValue: ParseForm would otherwise slurp the
	// entire request into memory. The form field is a model id, a few bytes.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	model := spawnModelFromRequest(r)
	if model == "" {
		model = modelcat.FallbackModelID
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	instanceID, err := a.pool.EnsureTokenSession(ctx, id, model)
	if err != nil {
		a.logfunc().Warn("dashboard token session create failed", "token", id, "model", model, "err", err)
		a.dash.RenderConfigResult(w, r, false, fmt.Sprintf("Token #%d session failed for %s: %s", id, model, err.Error()))
		return
	}
	a.logfunc().Info("dashboard token session created", "token", id, "model", model, "instance", instanceID)
	a.dash.RenderConfigResult(w, r, true, fmt.Sprintf("Token #%d session created for %s (instance: %s).", id, model, instanceID))
}

func (a *adminHandlers) handleModeSwitch(w http.ResponseWriter, r *http.Request) {
	// Cap the body before FormValue: ParseForm would otherwise slurp the
	// entire request into memory before the JSON fallback's 4KB cap applies.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		Mode string `json:"mode"`
	}
	req.Mode = r.FormValue("mode")
	if req.Mode == "" {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
		if err != nil {
			a.dash.RenderConfigResult(w, r, false, "Failed to read request: "+err.Error())
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			a.dash.RenderConfigResult(w, r, false, "Invalid request: "+err.Error())
			return
		}
	}
	cfg := a.cfgLoad()
	switch strings.ToLower(strings.TrimSpace(req.Mode)) {
	case "bridge":
		if cfg.BridgeMode() {
			a.dash.RenderConfigResult(w, r, false, "Already in bridge mode.")
			return
		}
		// adminSaveMu serializes the persist → verify → rollback sequence
		// with the other .env writers (config editor, token add/remove) so a
		// concurrent save cannot interleave between the write and the reload.
		// The live-pool drain stays outside the lock, after the reload is
		// verified (persist → verify → drain).
		a.adminSaveMu.Lock()
		defer a.adminSaveMu.Unlock()
		// Persist AUTH_TOKENS= (explicit empty) and
		// reload, verifying the effective config actually lands in bridge
		// mode before touching the live pool. Roll the .env back on failure.
		// Dual-layer persist: AUTH_TOKENS= (explicit empty) to .env, token
		// marker dropped from settings (bridge = no pooled tokens).
		set, del := tokenMarkerDelta(nil)
		newCfg, err := a.dualWrite(
			[]config.EnvUpdate{{Key: "AUTH_TOKENS", Value: ""}},
			set, del,
			func(newCfg config.Config) error {
				if !newCfg.BridgeMode() {
					// A higher-precedence source (e.g. AUTH_TOKENS in a -config JSON
					// file or the real environment) still supplies tokens — .env alone
					// cannot clear it, so the switch cannot succeed.
					return errors.New("Could not switch to bridge mode: AUTH_TOKENS is still set by a -config JSON file or the environment, which overrides .env. Clear it there, or run without -config, then retry.")
				}
				return nil
			},
		)
		if err != nil {
			a.dash.RenderConfigResult(w, r, false, dualPersistMessage(err))
			return
		}
		a.applyReloadedConfig(&newCfg)
		a.pool.RemoveAllTokens(r.Context())
		a.logfunc().Info("dashboard switched to bridge mode")
		a.dash.RenderConfigResult(w, r, true, "Switched to bridge mode — AUTH_TOKENS cleared; clients now send their own token.")
	case "pooled":
		if !cfg.BridgeMode() && !cfg.HybridBridgeMode() {
			a.dash.RenderConfigResult(w, r, false, "Already in pooled mode.")
			return
		}
		if cfg.BridgeMode() {
			a.dash.RenderConfigResult(w, r, false, "Pooled mode needs tokens — add one via the Add-token form first.")
			return
		}
		// Hybrid → pure pooled: disable the bridge relay (BRIDGE_ENABLED=0)
		// and verify the effective config lands in pooled mode before
		// touching the live pool. Dual-layer persist: .env is the boot
		// seed/export, the settings overlay is runtime truth — both roll
		// back on failure.
		a.adminSaveMu.Lock()
		defer a.adminSaveMu.Unlock()
		set, del := tokenMarkerDelta(cfg.AuthTokens)
		set[config.OverlayRowKey("BRIDGE_ENABLED")] = "0"
		newCfg, err := a.dualWrite(
			[]config.EnvUpdate{{Key: "AUTH_TOKENS", Value: strings.Join(cfg.AuthTokens, ",")}, {Key: "BRIDGE_ENABLED", Value: "0"}},
			set, del,
			func(newCfg config.Config) error {
				if newCfg.HybridBridgeMode() {
					return errBridgeStillEnabled{pooled: true}
				}
				return nil
			},
		)
		if err != nil {
			var blocked errBridgeStillEnabled
			if errors.As(err, &blocked) {
				// A higher-precedence source still enables the bridge — the
				// file just written cannot clear it. Name the true blocker:
				// a DB overlay row beats the file (ADR-0019), so blaming
				// the environment then would send the operator to the
				// wrong place.
				if a.overlayShadows("BRIDGE_ENABLED") {
					a.dash.RenderConfigResult(w, r, false, "Could not switch to pooled mode: BRIDGE_ENABLED is still set by the DB settings overlay, which overrides .env (DELETE /admin/api/settings/BRIDGE_ENABLED to reset), then retry.")
					return
				}
				a.dash.RenderConfigResult(w, r, false, "Could not switch to pooled mode: BRIDGE_ENABLED is still set by a -config JSON file or the environment, which overrides .env. Clear it there, then retry.")
				return
			}
			a.dash.RenderConfigResult(w, r, false, dualPersistMessage(err))
			return
		}
		a.applyReloadedConfig(&newCfg)
		a.logfunc().Info("dashboard switched to pooled mode")
		a.dash.RenderConfigResult(w, r, true, "Switched to pooled mode — bridge relay disabled.")
	case "hybrid":
		if cfg.BridgeMode() {
			a.dash.RenderConfigResult(w, r, false, "Hybrid mode needs tokens — add one via the Add-token form first.")
			return
		}
		if cfg.HybridBridgeMode() {
			a.dash.RenderConfigResult(w, r, false, "Already in hybrid mode.")
			return
		}
		// Pure pooled → hybrid: enable the bridge relay alongside the pool.
		// Dual-layer persist like the pooled branch above.
		a.adminSaveMu.Lock()
		defer a.adminSaveMu.Unlock()
		set, del := tokenMarkerDelta(cfg.AuthTokens)
		set[config.OverlayRowKey("BRIDGE_ENABLED")] = "1"
		newCfg, err := a.dualWrite(
			[]config.EnvUpdate{{Key: "AUTH_TOKENS", Value: strings.Join(cfg.AuthTokens, ",")}, {Key: "BRIDGE_ENABLED", Value: "1"}},
			set, del,
			func(newCfg config.Config) error {
				if !newCfg.HybridBridgeMode() {
					return errBridgeStillEnabled{pooled: false}
				}
				return nil
			},
		)
		if err != nil {
			var blocked errBridgeStillEnabled
			if errors.As(err, &blocked) {
				if a.overlayShadows("BRIDGE_ENABLED") {
					a.dash.RenderConfigResult(w, r, false, "Could not switch to hybrid mode: BRIDGE_ENABLED is still set to 0 by the DB settings overlay, which overrides .env (DELETE /admin/api/settings/BRIDGE_ENABLED to reset), then retry.")
					return
				}
				a.dash.RenderConfigResult(w, r, false, "Could not switch to hybrid mode: BRIDGE_ENABLED is still set to 0 by a -config JSON file or the environment, which overrides .env. Clear it there, then retry.")
				return
			}
			a.dash.RenderConfigResult(w, r, false, dualPersistMessage(err))
			return
		}
		a.applyReloadedConfig(&newCfg)
		a.logfunc().Info("dashboard switched to hybrid mode")
		a.dash.RenderConfigResult(w, r, true, "Switched to hybrid mode — pooled + bridge active.")
	default:
		a.dash.RenderConfigResult(w, r, false, "Mode must be 'bridge', 'pooled', or 'hybrid'.")
	}
}

// handleBridgeTokenLock locks a bridge token by its key hash (#187).
func (a *adminHandlers) handleBridgeTokenLock(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	if err := a.pool.LockBridgeEntry(key); err != nil {
		a.dash.RenderConfigResult(w, r, false, "Lock failed: "+err.Error())
		return
	}
	a.logfunc().Info("bridge token locked", "key", key)
	a.dash.RenderConfigResult(w, r, true, "Bridge token "+shortKey(key)+" locked.")
}

// handleBridgeTokenUnlock clears the admin lock on a bridge token (#187).
func (a *adminHandlers) handleBridgeTokenUnlock(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	if err := a.pool.UnlockBridgeEntry(key); err != nil {
		a.dash.RenderConfigResult(w, r, false, "Unlock failed: "+err.Error())
		return
	}
	a.logfunc().Info("bridge token unlocked", "key", key)
	a.dash.RenderConfigResult(w, r, true, "Bridge token "+shortKey(key)+" unlocked.")
}

// shortKey returns the first 8 chars of a bridge key hash for display.
func shortKey(key string) string {
	if len(key) > 8 {
		return key[:8] + "…"
	}
	return key
}
