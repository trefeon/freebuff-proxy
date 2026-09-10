package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/dashboard"
)

// DB settings overlay endpoints (ADR-0019): per-key UI-persisted knobs that
// beat the .env file without rewriting it.
//
//	GET  /admin/api/settings       effective values + source tier per key
//	POST /admin/api/settings       {key, value}: validate, persist, hot-apply
//	DELETE /admin/api/settings/:key drop the overlay row (falls back)
//
// Every mutation reloads through loadConfig (file + overlay + env) and fans
// out via applyReloadedConfig — the same machinery /admin/reload and the
// .env editor use, never a fork. RestartOnly catalog keys report
// restart_only so the UI can say "saved, needs a restart" honestly.

// settingsEntry is one GET /admin/api/settings row (dashboard.SettingsEntry):
// the effective display value plus the precedence tier that provides it.
type settingsEntry = dashboard.SettingsEntry

// settingsRows reads the raw DB settings dump (key -> raw value). A nil
// store or a read failure degrades to empty (file/env/default only);
// mutations 503 instead (a write must not silently land nowhere).
func (a *adminHandlers) settingsRows() map[string]string {
	if a.settings == nil {
		return map[string]string{}
	}
	rows, err := a.settings.ListSettings()
	if err != nil {
		a.logfunc().Warn("settings overlay unreadable; serving file/env/default", "err", err)
		return map[string]string{}
	}
	return rows
}

// settingsOverlay reads the live DB overlay (canonical key -> raw value).
// A nil store or a read failure degrades to empty (file/env/default only);
// mutations 503 instead (a write must not silently land nowhere).
func (a *adminHandlers) settingsOverlay() map[string]string {
	return config.OverlayFromRows(a.settingsRows())
}

// migrateStatusInfo builds the boot smart-migration report for the settings
// payload from the store's in-memory Open report plus the already-fetched
// settings rows (marker presence): read-cheap, no per-request migration
// work and no extra DB round trip beyond the rows the handler already
// reads. Nil with a nil store (live-only boots carry no migration facts).
func (a *adminHandlers) migrateStatusInfo(rows map[string]string) *dashboard.MigrateStatusInfo {
	if a.settings == nil {
		return nil
	}
	ms := a.settings.MigrateStatus()
	_, marker := rows[config.MigrationMarkerRow]
	applied := ms.Applied
	if applied == nil {
		applied = []int{}
	}
	return &dashboard.MigrateStatusInfo{
		FromVersion: ms.FromVersion,
		ToVersion:   ms.ToVersion,
		Applied:     applied,
		Fresh:       ms.Fresh,
		Marker:      marker,
		Noop:        ms.Noop && marker,
	}
}

// loadConfig is the single overlay-aware reload every admin mutation and
// /admin/reload funnels through: built-in defaults < JSON -config < .env <
// DB overlay < process env (ADR-0019). It replaces bare config.Load on the
// admin surface so a persisted UI knob can never be photo-shopped out by a
// later reload.
func (a *adminHandlers) loadConfig() (config.Config, error) {
	return config.LoadOpts(a.configPath, config.LoadOptions{Overlay: a.settingsOverlay()})
}
func (a *adminHandlers) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	// A nil store keeps the gateway live on file/env/default only (mutations
	// 503): say so with degraded:true so the UI can banner the read-only
	// state honestly. Status stays 200 with the full catalog — every row
	// still reports its effective value and tier.
	degraded := a.settings == nil
	// One rows fetch feeds both the overlay and the migrate report (marker
	// presence): no extra DB round trip beyond what the overlay already did.
	rows := a.settingsRows()
	overlay := config.OverlayFromRows(rows)
	cfg := a.cfgLoad()
	sources := config.SettingSources(a.configPath, overlay)
	values := make(map[string]config.DataEntry, len(config.Catalog()))
	for _, entry := range cfg.Data() {
		values[entry.Key] = entry
	}
	entries := make([]settingsEntry, 0, len(config.Catalog()))
	for _, def := range config.Catalog() {
		source := sources[def.Key]
		if source == "" {
			source = "default"
		}
		entries = append(entries, settingsEntry{
			Key:         def.Key,
			Value:       values[def.Key].Value,
			Source:      source,
			RestartOnly: def.RestartOnly,
			Secret:      def.Secret,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dashboard.SettingsListResponse{Degraded: degraded, Settings: entries, Migrate: a.migrateStatusInfo(rows)})
}

func (a *adminHandlers) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	if a.settings == nil {
		a.dash.RenderResult(w, http.StatusServiceUnavailable, false,
			"Settings store unavailable — the dashboard runs live-only (the DB failed to open at boot).", "settings_unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req dashboard.SettingsPostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "Invalid JSON body (want {key, value}).", "bad_request")
		return
	}
	key := config.NormalizeSettingKey(req.Key)
	// AUTH_TOKENS and ADMIN_TOKEN have dedicated mutation endpoints that
	// converge the overlay alongside the other layers they touch: the Tokens
	// page and mode switch reconcile the live pool and the presence marker,
	// Change-password refreshes the session cookie and forces
	// require-login. A direct knob write would bypass that reconciliation
	// (the pool adopts additions but never removals), so it 400s with a
	// pointer instead of persisting a row the pool cannot honor.
	if key == "AUTH_TOKENS" {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "AUTH_TOKENS is managed on the Tokens page and mode switch, not as a knob (the pool needs reconciling).", "invalid_setting")
		return
	}
	if key == "ADMIN_TOKEN" {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "ADMIN_TOKEN is changed via Change password, not as a knob (the session cookie needs refreshing).", "invalid_setting")
		return
	}
	val, ok := settingsValueString(req.Value)
	if !ok {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "Value must be a string, number, or boolean.", "bad_value")
		return
	}
	// Numeric coercion for int keys: JSON numbers decode as float64. An
	// integral float is exactly the int the caller meant (3.0 -> "3"); a
	// non-integral float can never parse as an int, so reject it here with
	// a message that names the number instead of falling through to the
	// generic "must be an integer" shape error below.
	if f, isNum := req.Value.(float64); isNum {
		if def, known := config.LookupSetting(key); known && def.Kind == "int" {
			if math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f {
				a.dash.RenderResult(w, http.StatusBadRequest, false,
					fmt.Sprintf("%s must be an integer, got non-integral number %s", key,
						strconv.FormatFloat(f, 'f', -1, 64)), "invalid_setting")
				return
			}
			// Within the exact-integer float64 range the conversion is
			// lossless; beyond it keep the decimal expansion so the int
			// gate below still rejects what cannot be an int.
			if f >= -9007199254740992 && f <= 9007199254740992 {
				val = strconv.FormatInt(int64(f), 10)
			}
		}
	}
	if err := config.ValidateSettingValue(key, val); err != nil {
		a.dash.RenderResult(w, http.StatusBadRequest, false, err.Error(), "invalid_setting")
		return
	}

	a.adminSaveMu.Lock()
	defer a.adminSaveMu.Unlock()

	overlay := a.settingsOverlay()
	overlay[key] = strings.TrimSpace(val)
	// Validate through the existing config validation: the candidate overlay
	// loads exactly like boot would, so a bad duration/enum/lock map 400s
	// here instead of persisting a row that can never apply.
	newCfg, err := config.LoadOpts(a.configPath, config.LoadOptions{Overlay: overlay})
	if err != nil {
		a.logfunc().Warn("dashboard setting rejected", "key", key, "err", err)
		a.dash.RenderResult(w, http.StatusBadRequest, false, "Setting rejected: "+err.Error(), "invalid_setting")
		return
	}
	if err := a.settings.SetSetting(config.OverlayRowKey(key), strings.TrimSpace(val)); err != nil {
		a.logfunc().Warn("dashboard setting persist failed", "key", key, "err", err)
		a.dash.RenderResult(w, http.StatusInternalServerError, false, "Failed to persist setting: "+err.Error(), "persist_failed")
		return
	}
	oldCfg := a.cfgLoad()
	a.applyReloadedConfig(&newCfg)
	a.logfunc().Info("dashboard setting saved and applied",
		"remote", remoteHost(r), "key", key, "changed_keys", changedConfigKeys(oldCfg, &newCfg))

	def, _ := config.LookupSetting(key)
	restartOnly := []string{}
	message := key + " saved to the DB overlay and applied live."
	code := "setting_saved"
	if def.RestartOnly {
		restartOnly = []string{key}
		message = key + " saved to the DB overlay. It applies after restart (restart-only key)."
		code = "setting_restart_only"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dashboard.SettingsPostResponse{
		Code:        code,
		Message:     message,
		OK:          true,
		RestartOnly: restartOnly,
	})
}

func (a *adminHandlers) handleSettingsDelete(w http.ResponseWriter, r *http.Request) {
	if a.settings == nil {
		a.dash.RenderResult(w, http.StatusServiceUnavailable, false,
			"Settings store unavailable — the dashboard runs live-only (the DB failed to open at boot).", "settings_unavailable")
		return
	}
	key := config.NormalizeSettingKey(r.PathValue("key"))
	if key == "" {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "Setting key is required.", "bad_request")
		return
	}
	if _, known := config.LookupSetting(key); !known {
		a.dash.RenderResult(w, http.StatusNotFound, false, "Unknown setting "+key+".", "unknown_setting")
		return
	}

	a.adminSaveMu.Lock()
	defer a.adminSaveMu.Unlock()

	if _, ok, err := a.settings.GetSetting(config.OverlayRowKey(key)); err != nil {
		a.dash.RenderResult(w, http.StatusInternalServerError, false, "Failed to read setting: "+err.Error(), "persist_failed")
		return
	} else if !ok {
		a.dash.RenderResult(w, http.StatusNotFound, false, "No DB override for "+key+" (nothing to reset).", "no_override")
		return
	}
	overlay := a.settingsOverlay()
	delete(overlay, key)
	newCfg, err := config.LoadOpts(a.configPath, config.LoadOptions{Overlay: overlay})
	if err != nil {
		a.logfunc().Warn("dashboard setting reset reload failed", "key", key, "err", err)
		a.dash.RenderResult(w, http.StatusInternalServerError, false, "Failed to reload configuration: "+err.Error(), "reload_failed")
		return
	}
	if err := a.settings.DeleteSetting(config.OverlayRowKey(key)); err != nil {
		a.dash.RenderResult(w, http.StatusInternalServerError, false, "Failed to delete setting: "+err.Error(), "persist_failed")
		return
	}
	oldCfg := a.cfgLoad()
	a.applyReloadedConfig(&newCfg)
	source := config.SettingSources(a.configPath, overlay)[key]
	if source == "" {
		source = "default"
	}
	a.logfunc().Info("dashboard setting override reset",
		"remote", remoteHost(r), "key", key, "source", source,
		"changed_keys", changedConfigKeys(oldCfg, &newCfg))
	a.dash.RenderResult(w, http.StatusOK, true,
		"DB override for "+key+" removed — effective value now comes from "+source+".", "setting_reset")
}

// settingsValueString coerces the POSTed value to its raw overlay string.
// The SPA always sends strings; numbers/booleans are accepted so scripted
// clients need no string-wrapping ritual. Objects/arrays and null reject.
func settingsValueString(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	default:
		return "", false
	}
}

// settingsOverlayNote names the DB-overridden keys for the full-.env save
// response: the file write succeeded, but those keys keep their DB value
// until the overlay row is deleted. Empty when no overlay exists, so the
// classic save message is byte-identical without a store.
func (a *adminHandlers) settingsOverlayNote() string {
	overlay := a.settingsOverlay()
	if len(overlay) == 0 {
		return ""
	}
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return " DB overrides still win for: " + strings.Join(keys, ", ") +
		" (DELETE /admin/api/settings/:key to reset)."
}

// overlayShadows reports whether key's effective value currently comes from
// the DB overlay (ADR-0019): the overlay beats the .env file, so a .env write
// for that key cannot take effect until the row is deleted. The .env-backed
// writers whose knobs are also overlay-addressable (the mode switch for
// BRIDGE_ENABLED, require-login for DASHBOARD_REQUIRE_LOGIN) use it on their
// shadow-error paths to name the true blocker: SettingSources resolves the
// actual winning tier, so an env-pinned key still blames the environment and
// only a db-pinned key names the overlay.
func (a *adminHandlers) overlayShadows(key string) bool {
	if a.settings == nil {
		return false
	}
	return config.SettingSources(a.configPath, a.settingsOverlay())[key] == "db"
}
