package config

import (
	"os"
	"path/filepath"
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

// TestSettingsOverlaySecretsApply: since the env-to-DB migration every
// catalog key is overlay-addressable, secrets included (the DB file holds
// them at mode 0600) — a migrated row takes effect when the environment
// leaves the key unset. Fake values only, never real credentials.
func TestSettingsOverlaySecretsApply(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	cfg, err := LoadOpts("", LoadOptions{Overlay: map[string]string{
		"AUTH_TOKENS":       "fb-test-fake-token-1",
		"ADMIN_TOKEN":       "fb-test-fake-admin-1",
		"API_KEYS":          "fb-test-fake-client-1",
		"WEBHOOK_URL":       "https://example.invalid/hook",
		"UPSTREAM_BASE_URL": "https://example.invalid",
		"SAFE_MODE":         "false",
	}})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if len(cfg.AuthTokens) != 1 || cfg.AuthTokens[0] != "fb-test-fake-token-1" {
		t.Errorf("overlay AUTH_TOKENS did not apply: %v", cfg.AuthTokens)
	}
	if cfg.AdminToken != "fb-test-fake-admin-1" {
		t.Error("overlay ADMIN_TOKEN did not apply")
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0] != "fb-test-fake-client-1" {
		t.Errorf("overlay API_KEYS did not apply: %v", cfg.APIKeys)
	}
	if cfg.WebhookURL != "https://example.invalid/hook" {
		t.Errorf("overlay WEBHOOK_URL = %q, want the overlay value", cfg.WebhookURL)
	}
	if cfg.UpstreamBaseURL != "https://example.invalid" {
		t.Errorf("overlay UPSTREAM_BASE_URL = %q, want the overlay value", cfg.UpstreamBaseURL)
	}
	if cfg.SafeMode {
		t.Error("SafeMode = true, want false (allowed overlay must still apply)")
	}
}

// TestSettingsOverlaySecretsEnvWins: explicit process env still beats a
// migrated DB row for every formerly-blocked key — precedence never silently
// changes for existing users.
func TestSettingsOverlaySecretsEnvWins(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv("AUTH_TOKENS", "fb-test-fake-env-token-1")
	t.Setenv("ADMIN_TOKEN", "fb-test-fake-env-admin-1")
	t.Setenv("API_KEYS", "fb-test-fake-env-client-1")
	t.Setenv("WEBHOOK_URL", "https://env.invalid/hook")
	t.Setenv("UPSTREAM_BASE_URL", "https://env.invalid")
	cfg, err := LoadOpts("", LoadOptions{Overlay: map[string]string{
		"AUTH_TOKENS":       "fb-test-fake-db-token-1",
		"ADMIN_TOKEN":       "fb-test-fake-db-admin-1",
		"API_KEYS":          "fb-test-fake-db-client-1",
		"WEBHOOK_URL":       "https://db.invalid/hook",
		"UPSTREAM_BASE_URL": "https://db.invalid",
	}})
	if err != nil {
		t.Fatalf("LoadOpts env-wins: %v", err)
	}
	if len(cfg.AuthTokens) != 1 || cfg.AuthTokens[0] != "fb-test-fake-env-token-1" {
		t.Errorf("AUTH_TOKENS = %v, want the env value", cfg.AuthTokens)
	}
	if cfg.AdminToken != "fb-test-fake-env-admin-1" {
		t.Error("ADMIN_TOKEN lost to the overlay, want env to win")
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0] != "fb-test-fake-env-client-1" {
		t.Errorf("API_KEYS = %v, want the env value", cfg.APIKeys)
	}
	if cfg.WebhookURL != "https://env.invalid/hook" {
		t.Errorf("WEBHOOK_URL = %q, want the env value", cfg.WebhookURL)
	}
	if cfg.UpstreamBaseURL != "https://env.invalid" {
		t.Errorf("UPSTREAM_BASE_URL = %q, want the env value", cfg.UpstreamBaseURL)
	}
}

// TestOverlayCoversCatalog: every non-blocked catalog key must be
// overlay-addressable (applyMappedValues is the single shared key list, so
// this guards the filter in applySettingsOverlay, not the list itself).
// Since the env-to-DB migration the blocked set is empty: AUTH_TOKENS rides
// the overlay with presence semantics next to applyMappedValues.
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
	kept := OverlayFromRows(map[string]string{"config:AUTH_TOKENS": "x"})
	if kept["AUTH_TOKENS"] != "x" {
		t.Errorf("OverlayFromRows dropped unblocked AUTH_TOKENS: %v", kept)
	}
	unknown := OverlayFromRows(map[string]string{"config:NOPE_NOT_A_KEY": "x"})
	if len(unknown) != 0 {
		t.Errorf("OverlayFromRows kept unknown key: %v", unknown)
	}
}

// TestValidateSettingValue pins the POST gate: unknown keys and unparseable
// typed values reject; writable knobs accept. Secrets are storable since
// the env-to-DB migration (the DB file holds them at mode 0600); AUTH_TOKENS
// and ADMIN_TOKEN take the dedicated-endpoint path in the settings POST
// handler, but Validate itself accepts them so migrated rows read back.
func TestValidateSettingValue(t *testing.T) {
	for key, value := range map[string]string{
		"LOG_LEVEL":               "debug",
		"SAFE_MODE":               "false",
		"MAX_REQUESTS_PER_MINUTE": "30",
		"RATE_LIMIT_PER_IP":       "2.5",
		"MODELS_ALLOW":            "deepseek/deepseek-v4-flash",
		"MATURITY_TARGET_DAYS":    "14",
		"HTTP_READ_TIMEOUT":       "90s",
		"AUTH_TOKENS":             "fb-test-fake-token-1",
		"ADMIN_TOKEN":             "fb-test-fake-admin-1",
		"API_KEYS":                "fb-test-fake-client-1",
		"WEBHOOK_URL":             "https://example.invalid/hook",
		"UPSTREAM_BASE_URL":       "https://example.invalid",
		"AUTO_DISCOVER_TOKEN":     "false",
	} {
		if err := ValidateSettingValue(key, value); err != nil {
			t.Errorf("ValidateSettingValue(%s,%s) = %v, want nil", key, value, err)
		}
	}
	for key, value := range map[string]string{
		"NOPE_NOT_A_KEY":          "x",
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

// TestLogAndListenKeysAreRestartOnly pins the review finding that the logger
// (LOG_LEVEL/LOG_FORMAT/LOG_FILE/LOG_RING_SIZE) and the listener socket
// (LISTEN_ADDR) are never touched by a reload: applyReloadedConfig fans out
// to cfg/registry/pool/rate-limiter only, so the settings POST must report
// setting_restart_only for these keys instead of claiming a live apply.
// The server-side half of the parity (restartOnlyConfigKeys) is pinned by
// TestConfigCatalogRestartOnlyMatchesServer in the server package.
func TestLogAndListenKeysAreRestartOnly(t *testing.T) {
	for _, key := range []string{"LOG_LEVEL", "LOG_FORMAT", "LOG_FILE", "LOG_RING_SIZE", "LISTEN_ADDR"} {
		def, ok := LookupSetting(key)
		if !ok {
			t.Fatalf("LookupSetting(%s) missing from catalog", key)
		}
		if !def.RestartOnly {
			t.Errorf("%s RestartOnly = false, want true (reload never reconfigures the logger or re-binds the socket)", key)
		}
	}
}

// TestAutoDiscoverTokenOverlay: AUTO_DISCOVER_TOKEN is overlay-addressable
// since the env-to-DB migration. The overlay row takes effect only when the
// process environment leaves the key unset (env wins); the resolved value
// records on Config and suppresses CLI auto-discovery when false.
func TestAutoDiscoverTokenOverlay(t *testing.T) {
	if IsSettingsBlocked("AUTO_DISCOVER_TOKEN") {
		t.Error("IsSettingsBlocked(AUTO_DISCOVER_TOKEN) = true, want false (unblocked by the env-to-DB migration)")
	}
	if err := ValidateSettingValue("AUTO_DISCOVER_TOKEN", "false"); err != nil {
		t.Errorf("ValidateSettingValue(AUTO_DISCOVER_TOKEN) = %v, want nil", err)
	}
	if ov := OverlayFromRows(map[string]string{"config:AUTO_DISCOVER_TOKEN": "false"}); ov["AUTO_DISCOVER_TOKEN"] != "false" {
		t.Errorf("OverlayFromRows dropped unblocked AUTO_DISCOVER_TOKEN: %v", ov)
	}
	clearEnv(t)
	t.Chdir(t.TempDir())
	// clearEnv pins AUTO_DISCOVER_TOKEN=false in the environment; unset it so
	// the first load below resolves the overlay tier alone.
	if err := os.Unsetenv("AUTO_DISCOVER_TOKEN"); err != nil {
		t.Fatal(err)
	}
	fakeDiscover := func() (string, string, string, bool) { return "fb-test-fake-discovered-1", "", "", true }
	// Overlay "false" with no env: discovery suppressed, field records false.
	cfg, err := LoadOpts("", LoadOptions{DiscoverCLIToken: fakeDiscover, Overlay: map[string]string{"AUTO_DISCOVER_TOKEN": "false"}})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if cfg.AutoDiscoverToken {
		t.Error("AutoDiscoverToken = true, want false (overlay)")
	}
	if len(cfg.AuthTokens) != 0 {
		t.Errorf("AuthTokens = %v, want empty (overlay false suppresses discovery)", cfg.AuthTokens)
	}
	// Env "true" beats overlay "false": discovery fires.
	t.Setenv("AUTO_DISCOVER_TOKEN", "true")
	cfg, err = LoadOpts("", LoadOptions{DiscoverCLIToken: fakeDiscover, Overlay: map[string]string{"AUTO_DISCOVER_TOKEN": "false"}})
	if err != nil {
		t.Fatalf("LoadOpts env-wins: %v", err)
	}
	if !cfg.AutoDiscoverToken {
		t.Error("AutoDiscoverToken = false, want true (env wins)")
	}
	if len(cfg.AuthTokens) != 1 || cfg.AuthTokens[0] != "fb-test-fake-discovered-1" {
		t.Errorf("AuthTokens = %v, want the discovered token (env true enables discovery)", cfg.AuthTokens)
	}
}

// TestSettingSourcesDotenvUserIDAlias pins the .env USER_ID alias to the
// file tier for ACTING_USER_ID: the loader resolves it via
// overrideStringAlias on the dotenv tier, so the source tag must agree
// (the JSON and env tiers already attributed the alias). An empty alias
// value leaves the default in force like any other empty .env line.
func TestSettingSourcesDotenvUserIDAlias(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-1\nUSER_ID=user-dotenv-legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if sources := SettingSources("", nil); sources["ACTING_USER_ID"] != "file" {
		t.Errorf("ACTING_USER_ID source = %q, want file (.env USER_ID alias)", sources["ACTING_USER_ID"])
	}
}

// TestOverlayFromRowsDropsMalformed pins the documented contract that
// malformed rows are skipped: a non-empty value that fails
// ValidateSettingValue could never take effect (it would fail the POST
// gate), so a tampered or stale row must not poison the load. Empty values
// are kept as no-op pins instead (every override helper skips blanks, and
// AUTH_TOKENS presence is the bridge-mode pin).
func TestOverlayFromRowsDropsMalformed(t *testing.T) {
	ov := OverlayFromRows(map[string]string{
		"config:SAFE_MODE":               "false",
		"config:LOG_LEVEL":               "debug",
		"config:MAX_REQUESTS_PER_MINUTE": "30",
		"config:RATE_LIMIT_PER_IP":       "2.5",
		"config:NOPE_NOT_A_KEY":          "x",
		"theme":                          "dark",
	})
	for _, key := range []string{"SAFE_MODE", "LOG_LEVEL", "MAX_REQUESTS_PER_MINUTE", "RATE_LIMIT_PER_IP"} {
		if _, ok := ov[key]; !ok {
			t.Errorf("OverlayFromRows dropped valid row %s: %v", key, ov)
		}
	}
	if len(ov) != 4 {
		t.Errorf("OverlayFromRows = %v, want exactly the 4 valid rows", ov)
	}
	malformed := OverlayFromRows(map[string]string{
		"config:SAFE_MODE":               "banana",
		"config:MAX_REQUESTS_PER_MINUTE": "lots",
		"config:RATE_LIMIT_PER_IP":       "fast",
		"config:MATURITY_TARGET_DAYS":    "seven",
		"config:LOG_LEVEL2":              "debug",
	})
	if len(malformed) != 0 {
		t.Errorf("OverlayFromRows kept malformed rows: %v", malformed)
	}
	pins := OverlayFromRows(map[string]string{
		"config:LOG_LEVEL":   "",
		"config:AUTH_TOKENS": "",
	})
	if v, ok := pins["LOG_LEVEL"]; !ok || v != "" {
		t.Errorf("OverlayFromRows dropped the empty LOG_LEVEL pin: %v", pins)
	}
	if _, ok := pins["AUTH_TOKENS"]; !ok {
		t.Errorf("OverlayFromRows dropped the empty AUTH_TOKENS bridge pin: %v", pins)
	}
}

// TestSettingSourcesJSONEmptyIsDefault pins value-aware JSON attribution: an
// empty JSON value (null, "" or whitespace) leaves the default in force and
// reports "default" like an empty .env line does — never "file". Explicit
// zeros, falses, and empty arrays are real values and still report "file".
func TestSettingSourcesJSONEmptyIsDefault(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "config.json")
	body := `{"LOG_LEVEL":"","SAFE_MODE":"   ","MAX_REQUESTS_PER_MINUTE":null,` +
		`"RATE_LIMIT_PER_IP":"2.5","MAX_REQUESTS_PER_DAY":0,"MODELS_HIDE_UNAVAILABLE":false,` +
		`"ACTING_USER_ID":"","USER_ID":"user-json-legacy"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sources := SettingSources(path, nil)
	for key := range map[string]bool{"LOG_LEVEL": true, "SAFE_MODE": true, "MAX_REQUESTS_PER_MINUTE": true} {
		if sources[key] != "default" {
			t.Errorf("%s source = %q, want default (empty JSON value)", key, sources[key])
		}
	}
	for key := range map[string]bool{"RATE_LIMIT_PER_IP": true, "MAX_REQUESTS_PER_DAY": true, "MODELS_HIDE_UNAVAILABLE": true} {
		if sources[key] != "file" {
			t.Errorf("%s source = %q, want file (explicit JSON value)", key, sources[key])
		}
	}
	// The legacy USER_ID alias attributes to ACTING_USER_ID even when the
	// primary JSON key is empty (the loader's LegacyActingUserID merge).
	if sources["ACTING_USER_ID"] != "file" {
		t.Errorf("ACTING_USER_ID source = %q, want file (JSON USER_ID alias)", sources["ACTING_USER_ID"])
	}
}
