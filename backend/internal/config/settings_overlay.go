// DB settings overlay (ADR-0019): operator knobs persisted in SQLite that
// beat the .env file without rewriting it, so UI edits stick across
// restarts. Precedence, lowest to highest:
//
//	built-in defaults < JSON -config < .env file < DB overlay < process env
//
// The overlay lives in the generic settings table (store package) under
// OverlayRowKey namespacing; this package only defines the key contract and
// the load-time application, so the bottom-layer import rule holds (config
// imports nothing internal — the overlay arrives as a plain map via
// LoadOptions, read from the store by the caller).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SettingsBlockedKeys are never stored as a DB overlay: credentials and
// loopback-sensitive material stay env/.env-only, as does AUTO_DISCOVER_TOKEN
// (env-only: it controls whether the .env file is read at all, so an overlay
// row could never take effect — and the effective view hardcodes its display
// to "true", so storing "false" would be a silent no-op). POST rejects them;
// Load filters them defensively (a tampered row must not flip the instance
// into pooled mode or repoint the upstream).
var SettingsBlockedKeys = map[string]bool{
	"AUTH_TOKENS":         true,
	"ADMIN_TOKEN":         true,
	"API_KEYS":            true,
	"WEBHOOK_URL":         true,
	"UPSTREAM_BASE_URL":   true,
	"DB_PATH":             true,
	"AUTO_DISCOVER_TOKEN": true,
}

// OverlayRowPrefix namespaces config overlays inside the generic settings
// table (store holds other control state under its own keys).
const OverlayRowPrefix = "config:"

// NormalizeSettingKey canonicalizes an operator-supplied key for lookup.
func NormalizeSettingKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

// IsSettingsBlocked reports whether key may never live in the DB overlay.
func IsSettingsBlocked(key string) bool {
	return SettingsBlockedKeys[NormalizeSettingKey(key)]
}

// LookupSetting returns the catalog entry for key (canonical form).
func LookupSetting(key string) (KeyDef, bool) {
	n := NormalizeSettingKey(key)
	for _, def := range keyCatalog {
		if def.Key == n {
			return def, true
		}
	}
	return KeyDef{}, false
}

// OverlayRowKey maps one canonical config key to its settings-table row.
func OverlayRowKey(key string) string {
	return OverlayRowPrefix + NormalizeSettingKey(key)
}

// OverlayFromRows extracts the DB overlay (canonical key -> raw value) from
// a full settings-table dump. Unknown, blocked, or malformed rows are
// skipped: the overlay only ever addresses writable catalog keys, and a value
// that fails ValidateSettingValue could never take effect (it would fail the
// POST gate), so a tampered or stale row must not poison the load.
func OverlayFromRows(rows map[string]string) map[string]string {
	out := map[string]string{}
	for rowKey, value := range rows {
		rest, ok := strings.CutPrefix(rowKey, OverlayRowPrefix)
		if !ok {
			continue
		}
		n := NormalizeSettingKey(rest)
		if n == "" || IsSettingsBlocked(n) {
			continue
		}
		if _, known := LookupSetting(n); !known {
			continue
		}
		if err := ValidateSettingValue(n, value); err != nil {
			continue
		}
		out[n] = value
	}
	return out
}

// ValidateSettingValue checks one overlay write before it touches the DB:
// the key must be a known writable catalog key and the value must parse for
// its kind. Deeper semantic checks (durations, model locks, fallback maps)
// run through the full Load in the POST handler — this gate only rejects
// what could never take effect (unknown keys, secrets, unparseable
// bool/int/float), so a 400 never stores a silent no-op.
func ValidateSettingValue(key, value string) error {
	n := NormalizeSettingKey(key)
	if n == "" {
		return errors.New("setting key cannot be empty")
	}
	if IsSettingsBlocked(n) {
		return fmt.Errorf("%s cannot be stored as a DB overlay (env/.env only)", n)
	}
	def, ok := LookupSetting(n)
	if !ok {
		return fmt.Errorf("unknown setting %q", n)
	}
	if def.Secret {
		return fmt.Errorf("%s cannot be stored as a DB overlay (env/.env only)", n)
	}
	v := strings.TrimSpace(value)
	if v == "" {
		return fmt.Errorf("%s value must not be empty (use DELETE to reset the key)", n)
	}
	switch def.Kind {
	case "bool":
		if _, ok := parseBool(v); !ok {
			return fmt.Errorf("%s must be a bool (true/false, 1/0, on/off, yes/no), got %q", n, value)
		}
	case "int":
		if _, ok := parseIntPtr(v); !ok {
			return fmt.Errorf("%s must be an integer, got %q", n, value)
		}
	}
	// RATE_LIMIT_PER_IP renders as a text knob but parses as a float: an
	// unparseable value would fall through overrideFloat silently, so check
	// it explicitly (every other text/select/list knob fails its Load parse
	// loudly when malformed).
	if n == "RATE_LIMIT_PER_IP" {
		if _, ok := parseFloatPtr(v); !ok {
			return fmt.Errorf("%s must be a number (requests/second, 0 disables), got %q", n, value)
		}
	}
	return nil
}

// applySettingsOverlay layers the DB overlay onto raw between the .env file
// and the process environment (ADR-0019 precedence). It shares
// applyMappedValues with applyDotenv, so every key the loader parses is
// overlay-addressable by construction — no parallel key list to drift.
func applySettingsOverlay(raw *rawConfig, overlay map[string]string) {
	if len(overlay) == 0 {
		return
	}
	// Defense in depth: POST already rejects these, but a tampered row must
	// never repoint secrets or the upstream through the back door.
	filtered := make(map[string]string, len(overlay))
	for k, v := range overlay {
		n := NormalizeSettingKey(k)
		if n == "" || IsSettingsBlocked(n) {
			continue
		}
		if _, known := LookupSetting(n); !known {
			continue
		}
		filtered[n] = v
	}
	if len(filtered) == 0 {
		return
	}
	applyMappedValues(raw, func(name string) string { return filtered[name] })
}

// SettingSources reports, per catalog key, which precedence tier provides
// the effective value: "env" (explicit process environment), "db" (overlay),
// "file" (.env or JSON -config), or "default". It mirrors LoadOpts
// precedence without loading: env presence (AUTH_TOKENS counts even when
// empty, matching the loader) > overlay membership > file membership.
// overlay must use canonical keys (see OverlayFromRows). Membership is
// value-aware on the file tier like the loader is: an empty .env value
// leaves the default in force (except AUTH_TOKENS presence), and an empty
// JSON value (null or ""/whitespace) likewise reports "default", never
// "file". The legacy USER_ID alias attributes to ACTING_USER_ID on every
// tier that resolves it (env, .env, JSON), matching overrideStringAlias and
// the JSON LegacyActingUserID merge.
func SettingSources(configPath string, overlay map[string]string) map[string]string {
	out := make(map[string]string, len(keyCatalog))
	dotenv := readDotenvFile()
	jsonKeys := readJSONKeys(configPath)
	for _, def := range keyCatalog {
		k := def.Key
		if k == "AUTH_TOKENS" {
			if _, ok := os.LookupEnv(k); ok {
				out[k] = "env"
				continue
			}
		} else if k == "ACTING_USER_ID" {
			if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
				out[k] = "env"
				continue
			}
			if v, ok := os.LookupEnv("USER_ID"); ok && strings.TrimSpace(v) != "" {
				out[k] = "env"
				continue
			}
		} else if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			out[k] = "env"
			continue
		}
		if _, ok := overlay[k]; ok {
			out[k] = "db"
			continue
		}
		if k == "AUTH_TOKENS" {
			if _, ok := dotenv[k]; ok {
				out[k] = "file"
				continue
			}
		} else if k == "ACTING_USER_ID" {
			if v, ok := dotenv[k]; ok && strings.TrimSpace(v) != "" {
				out[k] = "file"
				continue
			}
			if v, ok := dotenv["USER_ID"]; ok && strings.TrimSpace(v) != "" {
				out[k] = "file"
				continue
			}
		} else if v, ok := dotenv[k]; ok && strings.TrimSpace(v) != "" {
			out[k] = "file"
			continue
		}
		if jsonKeys != nil {
			if raw, ok := jsonKeys[k]; ok && !jsonRawIsEmpty(raw) {
				out[k] = "file"
				continue
			}
			if k == "ACTING_USER_ID" {
				if raw, ok := jsonKeys["USER_ID"]; ok && !jsonRawIsEmpty(raw) {
					out[k] = "file"
					continue
				}
			}
		}
		out[k] = "default"
	}
	return out
}

// jsonRawIsEmpty reports whether a JSON -config value carries no effective
// setting: null, or a string that is empty or whitespace-only. Numbers,
// booleans, arrays, and objects always count as set (even 0/false/[] — the
// loader unmarshals those into explicit values, just as a "0" .env line
// counts as set on the dotenv tier).
func jsonRawIsEmpty(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return true
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return strings.TrimSpace(str) == ""
	}
	return false
}

// readDotenvFile returns the resolved .env pairs (nil when absent or
// unreadable — source resolution degrades to the remaining tiers, never an
// error on an admin read path).
func readDotenvFile() map[string]string {
	path := ResolveEnvFile()
	if path == "" {
		return nil
	}
	vals, err := readDotenv(path)
	if err != nil {
		return nil
	}
	return vals
}

// readJSONKeys returns the top-level key set of the JSON -config file (nil
// when none is configured or it cannot be read — same degrade rule as the
// .env tier above).
func readJSONKeys(configPath string) map[string]json.RawMessage {
	if strings.TrimSpace(configPath) == "" {
		return nil
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil
	}
	return keys
}
