package config

import (
	"os"
	"reflect"
	"testing"
)

// TestEffectiveOverlayCoversCatalog pins the migration contract: every
// catalog key exports a row, so a fresh env-only boot lands all knobs as
// config: rows plus the marker.
func TestEffectiveOverlayCoversCatalog(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	cfg, err := LoadOpts("", LoadOptions{})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	exported := EffectiveOverlayMap(cfg)
	for _, def := range Catalog() {
		if _, ok := exported[def.Key]; !ok {
			t.Errorf("EffectiveOverlayMap lacks catalog key %s", def.Key)
		}
	}
	if len(exported) != len(Catalog()) {
		t.Errorf("EffectiveOverlayMap has %d keys for %d catalog keys", len(exported), len(Catalog()))
	}
}

// TestEffectiveOverlayRoundTrip pins the DB-alone boot: env-set values
// (fake only) export to overlay rows that reproduce the identical effective
// config once the environment is cleared — the migrated user runs from the
// DB alone. The marker and foreign-namespace rows must not leak into the
// overlay.
func TestEffectiveOverlayRoundTrip(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	// clearEnv pins AUTO_DISCOVER_TOKEN=false in the environment; the export
	// must capture an explicit env value here, so set it directly.
	envSet := map[string]string{
		"AUTH_TOKENS":             "fb-test-fake-token-1,fb-test-fake-token-2",
		"ADMIN_TOKEN":             "fb-test-fake-admin-1",
		"API_KEYS":                "fb-test-fake-client-1",
		"WEBHOOK_URL":             "https://example.invalid/hook",
		"UPSTREAM_BASE_URL":       "https://example.invalid",
		"LOG_LEVEL":               "debug",
		"MAX_REQUESTS_PER_DAY":    "765",
		"MAX_REQUESTS_PER_MINUTE": "7",
		"SAFE_MODE":               "false",
		"AUTO_DISCOVER_TOKEN":     "false",
		"BRIDGE_ENABLED":          "false",
		"MODELS_ALLOW":            "deepseek/deepseek-v4-flash",
		"MODEL_LOCKS":             "0:z-ai/glm-5.2",
		"FALLBACK_MODEL":          "a=b",
		"REASONING_IN_CONTENT":    "thinking",
		"RATE_LIMIT_PER_IP":       "2.5",
		"BURST_WINDOW":            "90s",
	}
	for k, v := range envSet {
		t.Setenv(k, v)
	}
	cfgEnv, err := LoadOpts("", LoadOptions{})
	if err != nil {
		t.Fatalf("LoadOpts from env: %v", err)
	}
	exported := EffectiveOverlayMap(cfgEnv)

	// Simulate the settings-table dump: namespaced rows plus the marker and
	// foreign rows the overlay must ignore.
	rows := make(map[string]string, len(exported)+3)
	for k, v := range exported {
		rows[OverlayRowKey(k)] = v
	}
	rows[MigrationMarkerRow] = MigrationMarkerValue
	rows["theme"] = "dark"
	rows["config:NOPE_NOT_A_KEY"] = "x"
	ov := OverlayFromRows(rows)
	if _, ok := ov["MIGRATED_ENV_V1"]; ok {
		t.Errorf("OverlayFromRows leaked the marker row into the overlay: %v", ov)
	}
	if len(ov) != len(exported) {
		t.Errorf("OverlayFromRows kept %d of %d exported rows", len(ov), len(exported))
	}

	// DB-alone boot: the environment no longer pins anything.
	for k := range envSet {
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	cfgDB, err := LoadOpts("", LoadOptions{Overlay: ov})
	if err != nil {
		t.Fatalf("LoadOpts from overlay: %v", err)
	}
	if !reflect.DeepEqual(cfgDB, cfgEnv) {
		t.Errorf("DB-alone config differs from env config:\nDB:  %+v\nENV: %+v", cfgDB, cfgEnv)
	}
}

// TestEffectiveOverlayEmptyPoolPinsBridge pins the bridge-mode export: an
// explicitly-empty AUTH_TOKENS (the shape the dashboard mode switch persists
// as "AUTH_TOKENS=" in .env) must survive as an empty overlay row whose
// presence suppresses CLI auto-discovery — otherwise a migrated bridge user
// would silently flip to pooled mode on the next boot.
func TestEffectiveOverlayEmptyPoolPinsBridge(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	if err := os.Unsetenv("AUTO_DISCOVER_TOKEN"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTH_TOKENS", "")
	cfgEnv, err := LoadOpts("", LoadOptions{})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if !cfgEnv.BridgeMode() {
		t.Fatal("env-loaded config not in bridge mode, want empty pool")
	}
	exported := EffectiveOverlayMap(cfgEnv)
	v, ok := exported["AUTH_TOKENS"]
	if !ok {
		t.Fatal("EffectiveOverlayMap dropped empty AUTH_TOKENS, want the bridge pin")
	}
	if v != "" {
		t.Errorf("AUTH_TOKENS export = %q, want empty (bridge pin)", v)
	}
	rows := map[string]string{OverlayRowKey("AUTH_TOKENS"): v}
	ov := OverlayFromRows(rows)
	if _, ok := ov["AUTH_TOKENS"]; !ok {
		t.Fatalf("OverlayFromRows dropped the empty AUTH_TOKENS pin: %v", ov)
	}
	if err := os.Unsetenv("AUTH_TOKENS"); err != nil {
		t.Fatal(err)
	}
	fakeDiscover := func() (string, string, string, bool) { return "fb-test-fake-discovered-1", "", "", true }
	cfgDB, err := LoadOpts("", LoadOptions{DiscoverCLIToken: fakeDiscover, Overlay: ov})
	if err != nil {
		t.Fatalf("LoadOpts DB-alone: %v", err)
	}
	if !cfgDB.BridgeMode() {
		t.Errorf("DB-alone AuthTokens = %v, want bridge mode (discovery suppressed by the pin)", cfgDB.AuthTokens)
	}
}

// TestMigratedKeysReportDB pins the dashboard tier tags: every migrated key
// reports source=db once its row exists (env still wins when set).
func TestMigratedKeysReportDB(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	// clearEnv pins AUTO_DISCOVER_TOKEN=false in the environment (which
	// correctly wins); unset it so the overlay tier reports below.
	if err := os.Unsetenv("AUTO_DISCOVER_TOKEN"); err != nil {
		t.Fatal(err)
	}
	overlay := map[string]string{
		"AUTH_TOKENS":         "fb-test-fake-token-1",
		"ADMIN_TOKEN":         "fb-test-fake-admin-1",
		"API_KEYS":            "fb-test-fake-client-1",
		"WEBHOOK_URL":         "https://example.invalid/hook",
		"UPSTREAM_BASE_URL":   "https://example.invalid",
		"AUTO_DISCOVER_TOKEN": "false",
		"LOG_LEVEL":           "debug",
	}
	sources := SettingSources("", overlay)
	for _, k := range []string{"AUTH_TOKENS", "ADMIN_TOKEN", "API_KEYS", "WEBHOOK_URL", "UPSTREAM_BASE_URL", "AUTO_DISCOVER_TOKEN", "LOG_LEVEL"} {
		if sources[k] != "db" {
			t.Errorf("%s source = %q, want db", k, sources[k])
		}
	}
	t.Setenv("ADMIN_TOKEN", "fb-test-fake-env-admin-1")
	if sources := SettingSources("", overlay); sources["ADMIN_TOKEN"] != "env" {
		t.Errorf("ADMIN_TOKEN source = %q with env set, want env", sources["ADMIN_TOKEN"])
	}
}
