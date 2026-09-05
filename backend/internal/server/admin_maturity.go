package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// maturityParams carries the per-token maturity settings from a dashboard
// form post or a JSON API call.
type maturityParams struct {
	enabled bool
	hasOn   bool
	target  int
	hasGoal bool
	mode    string
}

// maturityParamsFromRequest reads enabled/target/mode from form fields first,
// then a JSON body (the SPA posts JSON via postAPI, whose FormValue never
// parses — same fallback as parseTokenIndex/spawnModelFromRequest).
func maturityParamsFromRequest(w http.ResponseWriter, r *http.Request) maturityParams {
	var p maturityParams
	if v := strings.TrimSpace(r.FormValue("enabled")); v != "" {
		p.hasOn = true
		p.enabled = parseMaturityBool(v)
	}
	if v := strings.TrimSpace(r.FormValue("target")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.hasGoal = true
			p.target = n
		}
	}
	if v := strings.TrimSpace(r.FormValue("mode")); v != "" {
		p.mode = v
	}
	if p.hasOn && p.hasGoal && p.mode != "" {
		return p
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		return p
	}
	var jreq map[string]json.RawMessage
	if jerr := json.Unmarshal(body, &jreq); jerr != nil {
		return p
	}
	if !p.hasOn {
		if raw, ok := jreq["enabled"]; ok {
			var b bool
			if uerr := json.Unmarshal(raw, &b); uerr == nil {
				p.hasOn, p.enabled = true, b
			} else {
				var s string
				if serr := json.Unmarshal(raw, &s); serr == nil {
					p.hasOn, p.enabled = true, parseMaturityBool(s)
				}
			}
		}
	}
	if !p.hasGoal {
		if raw, ok := jreq["target"]; ok {
			var n int
			if uerr := json.Unmarshal(raw, &n); uerr == nil {
				p.hasGoal, p.target = true, n
			} else {
				var s string
				if serr := json.Unmarshal(raw, &s); serr == nil {
					if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
						p.hasGoal, p.target = true, n
					}
				}
			}
		}
	}
	if p.mode == "" {
		if raw, ok := jreq["mode"]; ok {
			var s string
			if serr := json.Unmarshal(raw, &s); serr == nil {
				p.mode = strings.TrimSpace(s)
			}
		}
	}
	return p
}

func parseMaturityBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on", "enable", "enabled":
		return true
	}
	return false
}

// handleTokenMaturity sets per-token streak-maturity automation
// (POST /admin/tokens/{id}/maturity). Enabling also applies the
// administrative lock so the warming account leaves rotation; hitting the
// streak target auto-releases it. Disabling never unlocks.
func (a *adminHandlers) handleTokenMaturity(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	var params maturityParams
	if err == nil {
		params = maturityParamsFromRequest(w, r)
		if !params.hasOn {
			err = errors.New("missing enabled (true/false)")
		}
	}
	if err == nil && params.hasGoal && (params.target < 1 || params.target > 28) {
		err = errors.New("target must be between 1 and 28")
	}
	if err == nil && params.mode != "" && params.mode != "unmetered" && params.mode != "premium-short" {
		err = errors.New("mode must be unmetered or premium-short")
	}
	if err == nil {
		err = a.pool.SetMaturity(id, params.enabled, params.target, params.mode)
	}
	if err != nil {
		a.dash.RenderConfigResult(w, r, false, "Maturity update failed: "+err.Error())
		return
	}
	if params.enabled {
		a.logfunc().Info("dashboard token maturity enabled", "token", id, "target", params.target, "mode", params.mode)
		a.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" maturity on — locked for warming; auto-releases at its streak target.")
		return
	}
	a.logfunc().Info("dashboard token maturity disabled", "token", id)
	a.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" maturity off — automation stopped (lock unchanged).")
}

// handleTokenMaturityTouch fires one manual maturity touch outside the daily
// slot (POST /admin/tokens/{id}/maturity/touch): the §3 validation-protocol
// lever. Health gates, streak freshness, and todayUsed still apply — only
// the slot wait and the 6h throttle are bypassed.
func (a *adminHandlers) handleTokenMaturityTouch(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	var action, result string
	if err == nil {
		action, result, err = a.pool.MaturityTouchNow(r.Context(), id)
	}
	if err != nil {
		a.dash.RenderConfigResult(w, r, false, "Maturity touch failed: "+err.Error())
		return
	}
	a.logfunc().Info("dashboard token maturity touched", "token", id, "action", action, "result", result)
	a.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" touch: "+action+" → "+result+".")
}
