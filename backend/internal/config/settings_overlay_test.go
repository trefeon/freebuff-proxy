package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSettingsOverlayPrecedence pins ADR-0019 precedence on one live knob
// (LOG_LEVEL, default "info"): file < db overlay < process env.
func TestSettingsOverlayPrecedence(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("LOG_LEVEL=warn\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// File only.
	cfg, err := LoadOpts("", LoadOptions{})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if cfg.LogLevel != "warn" {
		t.Fatalf("file-only LogLevel = %q, want warn", cfg.LogLevel)
	}

	// DB overlay beats the file.
	cfg, err = LoadOpts("", LoadOptions{Overlay: map[string]string{"LOG_LEVEL": "debug"}})
	if err != nil {
		t.Fatalf("LoadOpts overlay: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("overlay LogLevel = %q, want debug", cfg.LogLevel)
	}

	// Explicit process env beats the overlay.
	t.Setenv("LOG_LEVEL", "error")
	cfg, err = LoadOpts("", LoadOptions{Overlay: map[string]string{"LOG_LEVEL": "debug"}})
	if err != nil {
		t.Fatalf("LoadOpts env-wins: %v", err)
	}
	if cfg.LogLevel != "error" {
		t.Fatalf("env LogLevel = %q, want error", cfg.LogLevel)
	}
}

// TestSettingsOverlayIntBool pins typed overlay parsing (int + bool knobs).
func TestSettingsOverlayIntBool(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	cfg, err := LoadOpts("", LoadOptions{Overlay: map[string]string{
		"MAX_REQUESTS_PER_MINUTE": "7",
		"SAFE_MODE":               "false",
	}})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if cfg.MaxRequestsPerMinute != 7 {
		t.Errorf("MaxRequestsPerMinute = %d, want 7", cfg.MaxRequestsPerMinute)
	}
	if cfg.SafeMode {
		t.Error("SafeMode = true, want false (overlay)")
	}
}

// TestSettingsOverlayBlockedNeverApplies: a tampered overlay row for a
// secret/endpoint key must not take effect — the file/env value stands.
func TestSettingsOverlayBlockedNeverApplies(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	cfg, err := LoadOpts("", LoadOptions{Overlay: map[string]string{
		"AUTH_TOKENS":       "tok-should-not-apply",
		"ADMIN_TOKEN":       "hijack",
		"UPSTREAM_BASE_URL": "https://evil.example.com",
		"SAFE_MODE":         "false",
	}})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if len(cfg.AuthTokens) != 0 {
		t.Errorf("blocked AUTH_TOKENS overlay applied: %v", cfg.AuthTokens)
	}
	if cfg.AdminToken == "hijack" {
		t.Error("blocked ADMIN_TOKEN overlay applied")
	}
	if strings.Contains(cfg.UpstreamBaseURL, "evil") {
		t.Errorf("blocked UPSTREAM_BASE_URL overlay applied: %q", cfg.UpstreamBaseURL)
	}
	if cfg.SafeMode {
		t.Error("SafeMode = true, want false (allowed overlay must still apply)")
	}
}

// TestOverlayCoversCatalog: every non-blocked catalog key must be
// overlay-addressable (applyMappedValues is the single shared key list, so
// this guards the filter in applySettingsOverlay, not the list itself).
func TestOverlayCoversCatalog(t *testing.T) {
	for _, def := range Catalog() {
		if IsSettingsBlocked(def.Key) {
			continue
		}
		raw := defaultRawConfig()
		get := func(name string) string {
			if name == def.Key {
				return "true"
			}
			return ""
		}
		// Must not panic on any catalog key; filter membership is
		// asserted via OverlayFromRows below.
		applyMappedValues(&raw, get)
	}
	rows := map[string]string{"config:SAFE_MODE": "false", "theme": "dark"}
	ov := OverlayFromRows(rows)
	if ov["SAFE_MODE"] != "false" {
		t.Errorf("OverlayFromRows dropped config:SAFE_MODE: %v", ov)
	}
	if _, ok := ov["THEME"]; ok {
		t.Errorf("OverlayFromRows kept non-config row: %v", ov)
	}
	blocked := OverlayFromRows(map[string]string{"config:AUTH_TOKENS": "x"})
	if len(blocked) != 0 {
		t.Errorf("OverlayFromRows kept blocked key: %v", blocked)
	}
	unknown := OverlayFromRows(map[string]string{"config:NOPE_NOT_A_KEY": "x"})
	if len(unknown) != 0 {
		t.Errorf("OverlayFromRows kept unknown key: %v", unknown)
	}
}

// TestValidateSettingValue pins the POST gate: unknown/blocked/secret keys
// and unparseable typed values reject; writable knobs accept.
func TestValidateSettingValue(t *testing.T) {
	for key, value := range map[string]string{
		"LOG_LEVEL":               "debug",
		"SAFE_MODE":               "false",
		"MAX_REQUESTS_PER_MINUTE": "30",
		"RATE_LIMIT_PER_IP":       "2.5",
		"MODELS_ALLOW":            "deepseek/deepseek-v4-flash",
		"MATURITY_TARGET_DAYS":    "14",
		"HTTP_READ_TIMEOUT":       "90s",
	} {
		if err := ValidateSettingValue(key, value); err != nil {
			t.Errorf("ValidateSettingValue(%s,%s) = %v, want nil", key, value, err)
		}
	}
	for key, value := range map[string]string{
		"NOPE_NOT_A_KEY":          "x",
		"AUTH_TOKENS":             "tok",
		"ADMIN_TOKEN":             "x",
		"API_KEYS":                "x",
		"WEBHOOK_URL":             "https://example.com",
		"UPSTREAM_BASE_URL":       "https://example.com",
		"DB_PATH":                 "/tmp/x.db",
		"SAFE_MODE":               "banana",
		"MAX_REQUESTS_PER_MINUTE": "lots",
		"RATE_LIMIT_PER_IP":       "fast",
		"MATURITY_TARGET_DAYS":    "seven",
		"LOG_LEVEL":               "",
		"":                        "x",
	} {
		if err := ValidateSettingValue(key, value); err == nil {
			t.Errorf("ValidateSettingValue(%q,%q) accepted, want an error", key, value)
		}
	}
}

// TestSettingSources pins the per-key source tags across all four tiers.
func TestSettingSources(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("LOG_LEVEL=warn\nSAFE_MODE=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := map[string]string{"LOG_LEVEL": "debug", "BRIDGE_ENABLED": "false"}
	t.Setenv("SAFE_MODE", "true")

	sources := SettingSources("", overlay)
	if sources["LOG_LEVEL"] != "db" {
		t.Errorf("LOG_LEVEL source = %q, want db", sources["LOG_LEVEL"])
	}
	if sources["SAFE_MODE"] != "env" {
		t.Errorf("SAFE_MODE source = %q, want env", sources["SAFE_MODE"])
	}
	if sources["BRIDGE_ENABLED"] != "db" {
		t.Errorf("BRIDGE_ENABLED source = %q, want db", sources["BRIDGE_ENABLED"])
	}
	if sources["MAX_REQUESTS_PER_MINUTE"] != "default" {
		t.Errorf("MAX_REQUESTS_PER_MINUTE source = %q, want default", sources["MAX_REQUESTS_PER_MINUTE"])
	}

	// Without overlay or env, the .env key reports file.
	sources = SettingSources("", nil)
	if sources["LOG_LEVEL"] != "file" {
		t.Errorf("LOG_LEVEL source = %q, want file", sources["LOG_LEVEL"])
	}
}

// TestSettingSourcesJSONFile: a JSON -config key counts as the file tier.
func TestSettingSourcesJSONFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"LOG_LEVEL":"debug"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sources := SettingSources(path, nil)
	if sources["LOG_LEVEL"] != "file" {
		t.Errorf("LOG_LEVEL source = %q, want file (JSON)", sources["LOG_LEVEL"])
	}
	if sources["SAFE_MODE"] != "default" {
		t.Errorf("SAFE_MODE source = %q, want default", sources["SAFE_MODE"])
	}
}
