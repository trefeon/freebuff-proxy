// DB settings overlay (ADR-0019): operator knobs persisted in SQLite that
// beat the .env file without rewriting it, so UI edits stick across
// restarts. Precedence, lowest to highest:
//
//	built-in defaults < JSON -config < .env file < DB overlay < process env
//
// Since the env-to-DB migration the overlay is the persisted home of the
// whole knob set, secrets included: the settings table lives in the dashboard
// DB file (0600, enforced at open), so AUTH_TOKENS, ADMIN_TOKEN, API_KEYS,
// and WEBHOOK_URL rows are expected there. Explicit process env still wins
// at runtime — a migrated row never silently overrides the environment.
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
	"time"
)

// SettingsBlockedKeys names keys that may never live in the DB overlay.
// Empty since the env-to-DB migration: every formerly-blocked key
// (AUTH_TOKENS, ADMIN_TOKEN, API_KEYS, WEBHOOK_URL, UPSTREAM_BASE_URL,
// DB_PATH, AUTO_DISCOVER_TOKEN) is now overlay-addressable so a fully
// migrated user runs from the DB alone. The map (and IsSettingsBlocked) stay
// as the gate for any future key that must remain env-only. Two notes:
//
//   - DB_PATH has no catalog entry, so OverlayFromRows still drops it as an
//     unknown key: the open path resolves the file from the process
//     environment before any overlay could load, and a row could never
//     repoint the live file.
//   - Process env still wins over the overlay at runtime for every key, so
//     a migrated row can never silently override an explicit environment.
var SettingsBlockedKeys = map[string]bool{}

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
// skipped: the overlay only ever addresses writable catalog keys, and a
// non-empty value that fails ValidateSettingValue could never take effect
// (it would fail the POST gate), so a tampered or stale row must not poison
// the load. Empty values are kept as no-op pins: every override*From helper
// skips blanks, so they change nothing — except AUTH_TOKENS, where presence
// (even empty) is the explicit bridge-mode choice that suppresses CLI
// auto-discovery, mirroring the .env tier. The migration writes one row per
// catalog key (blanks included), so the overlay round-trips the full knob
// set; explicit process env still wins over every row at Load.
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
		if strings.TrimSpace(value) != "" {
			if err := ValidateSettingValue(n, value); err != nil {
				continue
			}
		}
		out[n] = value
	}
	return out
}

// ValidateSettingValue checks one overlay write before it touches the DB:
// the key must be a known writable catalog key and the value must parse for
// its kind. Secrets are storable: the settings table lives in the dashboard
// DB file (0600, enforced at open), which is the persisted home of the whole
// knob set since the env-to-DB migration. Deeper semantic checks (durations,
// model locks, fallback maps) run through the full Load in the POST handler
// — this gate only rejects what could never take effect (unknown keys,
// unparseable bool/int/float), so a 400 never stores a silent no-op.
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
	// BURST_MAX_TOKENS=1 would pass the int gate but fail the Load
	// range-check (minimum 2): reject it here with the actionable message
	// instead of the generic Load error. BURST_WINDOW is a text knob like
	// RATE_LIMIT_PER_IP — an unparseable duration would otherwise fall
	// through to the POST-time Load rejection with less context.
	if n == "BURST_MAX_TOKENS" {
		if parsed, ok := parseIntPtr(v); !ok || *parsed < 2 {
			return fmt.Errorf("%s must be an integer of at least 2 (burst spreading needs two or more accounts), got %q", n, value)
		}
	}
	if n == "BURST_WINDOW" {
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("%s must be a Go duration (e.g. 30s, 1m, 5m), got %q", n, value)
		}
	}
	if n == "QUOTA_PROBE_ACTIVE_INTERVAL" || n == "QUOTA_PROBE_IDLE_HEARTBEAT" {
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("%s must be a Go duration (e.g. 30s, 1m, 30m), got %q", n, value)
		}
	}
	return nil
}

// applySettingsOverlay layers the DB overlay onto raw between the .env file
// and the process environment (ADR-0019 precedence). It shares
// applyMappedValues with applyDotenv, so every key the loader parses is
// overlay-addressable by construction — no parallel key list to drift.
// AUTH_TOKENS rides alongside with .env-tier presence semantics: the key's
// presence (even empty) records an explicit pool choice and suppresses CLI
// auto-discovery, so a migrated bridge-mode user stays in bridge mode when
// the environment no longer pins the pool. Explicit process env still wins:
// Load applies the real-environment AUTH_TOKENS block after this overlay.
func applySettingsOverlay(raw *rawConfig, overlay map[string]string) {
	if len(overlay) == 0 {
		return
	}
	// Defense in depth: OverlayFromRows already drops these, but a caller
	// passing a hand-built map must not smuggle unknown rows into raw.
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
	if v, ok := filtered["AUTH_TOKENS"]; ok {
		raw.AuthTokens = splitList(v)
		raw.AuthTokensSet = true
	}
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
