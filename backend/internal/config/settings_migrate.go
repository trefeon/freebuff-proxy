package config

import (
	"os"
	"strconv"
	"strings"
)

// Env-to-DB migration (config:migrated_env_v1): the first boot with a settings
// table that lacks the marker row imports every effective knob — process env
// wins over the .env file over JSON -config over built-in defaults, the same
// precedence LoadOpts resolves — into config: overlay rows, then sets the
// marker. Later boots are no-ops via the marker, and explicit process env
// keeps winning over every migrated row at runtime, so precedence never
// silently changes for existing users: the DB becomes the persisted home the
// dashboard saves write to, not a new winner.

// MigrationMarkerRow is the settings-table row whose presence means the
// env-to-DB import already ran. It shares the config: prefix but is not a
// catalog key, so OverlayFromRows drops it and it can never leak into a
// Load overlay.
const MigrationMarkerRow = "config:migrated_env_v1"

// MigrationMarkerValue is the value stored at MigrationMarkerRow.
const MigrationMarkerValue = "1"

// EffectiveOverlayMap renders the effective configuration back to raw overlay
// strings (canonical KEY -> raw VALUE) for every catalog key: the exact map
// the migration persists as config: rows. Secrets render raw, never masked —
// the settings table lives in the dashboard DB file (0600, enforced at open)
// and is their persisted home. Empty values render as "" (a no-op pin at
// Load, except AUTH_TOKENS presence, which is the explicit bridge-mode choice
// that suppresses CLI auto-discovery).
func EffectiveOverlayMap(cfg Config) map[string]string {
	out := make(map[string]string, len(keyCatalog))
	for _, def := range keyCatalog {
		out[def.Key] = effectiveRawValue(cfg, def.Key)
	}
	return out
}

// effectiveRawValue renders one catalog key's effective value as the raw
// string its overlay row (or .env line) would carry. Non-secret Config-backed
// keys reuse renderKey, whose display rendering is already the canonical raw
// shape (durations via String, bools, ints, canonical joins); the keys below
// need raw (unmasked) or Config-field-less handling instead.
func effectiveRawValue(cfg Config, key string) string {
	switch key {
	case "AUTH_TOKENS":
		return strings.Join(cfg.AuthTokens, ",")
	case "API_KEYS":
		return strings.Join(cfg.APIKeys, ",")
	case "ADMIN_TOKEN":
		return cfg.AdminToken
	case "WEBHOOK_URL":
		return cfg.WebhookURL
	case "AUTO_DISCOVER_TOKEN":
		return strconv.FormatBool(cfg.AutoDiscoverToken)
	case "ACTING_USER_ID":
		return cfg.ActingUserID
	case "COMPRESS_PROMPT":
		return strconv.FormatBool(cfg.CompressPrompt)
	case "CACHE_CONTROL_INJECTION":
		return strconv.FormatBool(cfg.CacheControlInjection)
	case "REASONING_IN_CONTENT":
		return cfg.ReasoningInContent
	case "ADMIN_FORCE_SECURE_COOKIES":
		// No Config field: the reader (server isSecureCookie) consults the
		// process environment with a .env fallback on every request, never
		// the overlay. Snapshot the same resolution for reference; the row
		// documents the migrated value but never drives the reader.
		return strconv.FormatBool(effectiveAdminForceSecureCookies())
	default:
		v, _ := renderKey(&cfg, key)
		return v
	}
}

// effectiveAdminForceSecureCookies mirrors the server's per-request secure
// cookie resolution (process env wins, else the .env file, else false) so
// the migration snapshot records the value actually in force. True words
// follow the historical convention: "true"/"1"/"yes".
func effectiveAdminForceSecureCookies() bool {
	if v, ok := os.LookupEnv("ADMIN_FORCE_SECURE_COOKIES"); ok {
		return isTrueWord(v)
	}
	if dotenv := readDotenvFile(); dotenv != nil {
		if v, ok := dotenv["ADMIN_FORCE_SECURE_COOKIES"]; ok {
			return isTrueWord(strings.Trim(v, `"'`))
		}
	}
	return false
}

// isTrueWord reports whether s enables a flag in the secure-cookies
// convention ("true"/"1"/"yes", case-insensitive, trimmed).
func isTrueWord(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes":
		return true
	}
	return false
}
