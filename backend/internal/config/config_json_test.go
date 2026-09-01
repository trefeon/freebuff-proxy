package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/clicreds"
)

func TestLoadFromFile(t *testing.T) {
	clearEnv(t)

	json := `{
		"LISTEN_ADDR": ":9999",
		"UPSTREAM_BASE_URL": "https://codebuff.com",
		"AUTH_TOKENS": ["tok-a", "tok-b"],
		"ROTATION_INTERVAL": "1h",
		"REQUEST_TIMEOUT": "5m",
		"SESSION_CALL_TIMEOUT": "10s",
		"API_KEYS": ["k1"],
		"COST_MODE": "free",
		"REGISTRY_REFRESH": "2h",
		"DEBUG_DUMP": true,
		"LOG_FILE": "proxy.log"
	}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ListenAddr != ":9999" {
		t.Errorf("ListenAddr = %q, want :9999", cfg.ListenAddr)
	}
	if cfg.UpstreamBaseURL != "https://www.codebuff.com" {
		t.Errorf("UpstreamBaseURL = %q, want https://www.codebuff.com", cfg.UpstreamBaseURL)
	}
	if want := []string{"tok-a", "tok-b"}; !equalStrings(cfg.AuthTokens, want) {
		t.Errorf("AuthTokens = %v, want %v", cfg.AuthTokens, want)
	}
	if cfg.RotationInterval != time.Hour {
		t.Errorf("RotationInterval = %v, want 1h", cfg.RotationInterval)
	}
	if cfg.RequestTimeout != 5*time.Minute {
		t.Errorf("RequestTimeout = %v, want 5m", cfg.RequestTimeout)
	}
	if cfg.SessionCallTimeout != 10*time.Second {
		t.Errorf("SessionCallTimeout = %v, want 10s", cfg.SessionCallTimeout)
	}
	if want := []string{"k1"}; !equalStrings(cfg.APIKeys, want) {
		t.Errorf("APIKeys = %v, want %v", cfg.APIKeys, want)
	}
	if cfg.CostMode != "free" {
		t.Errorf("CostMode = %q, want free", cfg.CostMode)
	}
	if cfg.RegistryRefresh != 2*time.Hour {
		t.Errorf("RegistryRefresh = %v, want 2h", cfg.RegistryRefresh)
	}
	if !cfg.DebugDump {
		t.Error("DebugDump = false, want true")
	}
	if cfg.LogFile != "proxy.log" {
		t.Errorf("LogFile = %q, want proxy.log", cfg.LogFile)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	clearEnv(t)

	path := filepath.Join(t.TempDir(), "config.json")
	json := `{
		"LISTEN_ADDR": ":9999",
		"UPSTREAM_BASE_URL": "https://codebuff.com",
		"AUTH_TOKENS": ["file-a"],
		"ROTATION_INTERVAL": "1h",
		"COST_MODE": "free"
	}`
	if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("AUTH_TOKENS", "env-a, env-b ,env-a")
	t.Setenv("LISTEN_ADDR", ":7777")
	t.Setenv("ROTATION_INTERVAL", "90m")
	t.Setenv("DEBUG_DUMP", "true")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ListenAddr != ":7777" {
		t.Errorf("ListenAddr = %q, want :7777 (env wins)", cfg.ListenAddr)
	}
	if want := []string{"env-a", "env-b"}; !equalStrings(cfg.AuthTokens, want) {
		t.Errorf("AuthTokens = %v, want %v (env wins, trimmed, deduped)", cfg.AuthTokens, want)
	}
	if cfg.RotationInterval != 90*time.Minute {
		t.Errorf("RotationInterval = %v, want 90m (env wins)", cfg.RotationInterval)
	}
	if !cfg.DebugDump {
		t.Error("DebugDump = false, want true (env bool wins)")
	}
	if cfg.CostMode != "free" {
		t.Errorf("CostMode = %q, want free (file value kept)", cfg.CostMode)
	}
}

func TestBadFile(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load of malformed JSON succeeded, want error")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("Load of missing file succeeded, want error")
	}
}

// TestConfigJSONStripsBOM is the B3 regression: a UTF-8 BOM at the start of
// a -config JSON file (Windows editors/PowerShell writers) must not break
// json.Unmarshal — every other file reader in the package strips it.
func TestConfigJSONStripsBOM(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.json")
	data := "\xef\xbb\xbf" + `{"LISTEN_ADDR":":9999","AUTH_TOKENS":["from-json"]}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load of BOM'd config.json: %v", err)
	}
	if cfg.ListenAddr != ":9999" {
		t.Errorf("ListenAddr = %q, want :9999 (BOM must not corrupt the first key)", cfg.ListenAddr)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "from-json" {
		t.Errorf("AuthTokens = %v, want [from-json]", got)
	}
}

// TestModelsAllowParsing verifies MODELS_ALLOW loads from env (single, multi,
// whitespace, empty), JSON (array and comma-separated string), and .env,
// landing in Config.ModelsAllow as an exact-id []string (drops empties).
func TestModelsAllowParsing(t *testing.T) {
	t.Run("env single", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("MODELS_ALLOW", "deepseek/deepseek-v4-flash")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"deepseek/deepseek-v4-flash"}; !equalStrings(cfg.ModelsAllow, want) {
			t.Errorf("ModelsAllow = %v, want %v", cfg.ModelsAllow, want)
		}
	})
	t.Run("env multi whitespace", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("MODELS_ALLOW", " deepseek/deepseek-v4-flash , z-ai/glm-5.2 , ,mimo/mimo-v2.5 ")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"deepseek/deepseek-v4-flash", "z-ai/glm-5.2", "mimo/mimo-v2.5"}
		if !equalStrings(cfg.ModelsAllow, want) {
			t.Errorf("ModelsAllow = %v, want %v", cfg.ModelsAllow, want)
		}
	})
	t.Run("empty is no restriction", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("MODELS_ALLOW", "  , ")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.ModelsAllow) != 0 {
			t.Errorf("ModelsAllow = %v, want empty (no restriction)", cfg.ModelsAllow)
		}
	})
	t.Run("JSON array", func(t *testing.T) {
		clearEnv(t)
		path := filepath.Join(t.TempDir(), "config.json")
		json := `{
			"AUTH_TOKENS": ["tok-1"],
			"MODELS_ALLOW": ["deepseek/deepseek-v4-flash", "z-ai/glm-5.2"]
		}`
		if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load (JSON array): %v", err)
		}
		want := []string{"deepseek/deepseek-v4-flash", "z-ai/glm-5.2"}
		if !equalStrings(cfg.ModelsAllow, want) {
			t.Errorf("ModelsAllow = %v, want %v", cfg.ModelsAllow, want)
		}
	})
	t.Run("JSON string", func(t *testing.T) {
		clearEnv(t)
		path := filepath.Join(t.TempDir(), "config.json")
		json := `{
			"AUTH_TOKENS": ["tok-1"],
			"MODELS_ALLOW": "deepseek/deepseek-v4-flash, z-ai/glm-5.2"
		}`
		if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load (JSON string): %v", err)
		}
		want := []string{"deepseek/deepseek-v4-flash", "z-ai/glm-5.2"}
		if !equalStrings(cfg.ModelsAllow, want) {
			t.Errorf("ModelsAllow = %v, want %v", cfg.ModelsAllow, want)
		}
	})
	t.Run("env overrides JSON", func(t *testing.T) {
		clearEnv(t)
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"AUTH_TOKENS":["tok-1"],"MODELS_ALLOW":["from-json"]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MODELS_ALLOW", "from-env")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := []string{"from-env"}; !equalStrings(cfg.ModelsAllow, want) {
			t.Errorf("ModelsAllow = %v, want %v (env beats JSON)", cfg.ModelsAllow, want)
		}
	})
	t.Run("dotenv", func(t *testing.T) {
		clearEnv(t)
		if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-1\nMODELS_ALLOW=deepseek/deepseek-v4-flash, mimo/mimo-v2.5\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load (.env): %v", err)
		}
		want := []string{"deepseek/deepseek-v4-flash", "mimo/mimo-v2.5"}
		if !equalStrings(cfg.ModelsAllow, want) {
			t.Errorf("ModelsAllow = %v, want %v (from .env)", cfg.ModelsAllow, want)
		}
	})
	t.Run("dotenv env wins", func(t *testing.T) {
		clearEnv(t)
		if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-1\nMODELS_ALLOW=from-dotenv\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MODELS_ALLOW", "from-env")
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := []string{"from-env"}; !equalStrings(cfg.ModelsAllow, want) {
			t.Errorf("ModelsAllow = %v, want %v (env beats .env)", cfg.ModelsAllow, want)
		}
	})
}

// TestModelAliasesFromJSON verifies MODEL_ALIASES is read from the -config
// JSON file (rawConfig.ModelAliases is a string field; only the env and .env
// paths were previously exercised).
func TestModelAliasesFromJSON(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"AUTH_TOKENS":["tok-1"],"MODEL_ALIASES":"gpt-4o:deepseek/deepseek-v4-flash,glm:z-ai/glm-5.2"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ModelAliases) != 2 {
		t.Fatalf("ModelAliases len = %d, want 2", len(cfg.ModelAliases))
	}
	if cfg.ModelAliases["gpt-4o"] != "deepseek/deepseek-v4-flash" {
		t.Errorf("ModelAliases[gpt-4o] = %q, want deepseek/deepseek-v4-flash (from JSON config)", cfg.ModelAliases["gpt-4o"])
	}
	if cfg.ModelAliases["glm"] != "z-ai/glm-5.2" {
		t.Errorf("ModelAliases[glm] = %q, want z-ai/glm-5.2 (from JSON config)", cfg.ModelAliases["glm"])
	}
}

// TestJSONExplicitEmptyAuthTokensBridge is the C8 distinction: a JSON
// `"AUTH_TOKENS": []` (explicit empty array) must record presence — bridge
// mode, and CLI auto-discovery must NOT refill the pool — unlike an absent
// key which leaves AuthTokensSet false.
func TestJSONExplicitEmptyAuthTokensBridge(t *testing.T) {
	clearEnv(t)
	// Re-enable auto-discovery; the explicit-empty AUTH_TOKENS below is what
	// must suppress it (not the AUTO_DISCOVER_TOKEN=off switch).
	t.Setenv("AUTO_DISCOVER_TOKEN", "")

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	credDir := filepath.Join(home, ".config", "manicode")
	if err := os.MkdirAll(credDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := `{"default": {"authToken": "cb_discovered", "email": "dev@example.com"}}`
	if err := os.WriteFile(filepath.Join(credDir, "credentials.json"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"AUTH_TOKENS":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadOpts(path, LoadOptions{DiscoverCLIToken: clicreds.DiscoverToken})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if len(cfg.AuthTokens) != 0 {
		t.Errorf("AuthTokens = %v, want empty (explicit [] is bridge mode)", cfg.AuthTokens)
	}
	if !cfg.BridgeMode() {
		t.Error("BridgeMode() = false, want true with explicit empty AUTH_TOKENS array")
	}
	if cfg.DiscoveredSource != "" {
		t.Errorf("DiscoveredSource = %q, want empty (auto-discovery must be suppressed)", cfg.DiscoveredSource)
	}
}

// TestPrecedenceChainSingleLoad exercises the full defaults < JSON < .env <
// env chain in ONE Load: a key provided at every level resolves to the env
// value, a JSON-only key survives, a .env-only key applies, and an env-only
// key applies.
func TestPrecedenceChainSingleLoad(t *testing.T) {
	clearEnv(t)

	path := filepath.Join(t.TempDir(), "config.json")
	json := `{
		"LISTEN_ADDR": ":9999",
		"LOG_FILE": "from-json.log",
		"AUTH_TOKENS": ["json-tok"],
		"COST_MODE": "free"
	}`
	if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(".env", []byte("LISTEN_ADDR=:1111\nAUTH_TOKENS=dotenv-tok\nCLI_VERSION=9.9.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LISTEN_ADDR", ":7777")
	t.Setenv("AUTH_TOKENS", "env-tok")
	t.Setenv("SESSION_PERSIST", "true")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":7777" {
		t.Errorf("ListenAddr = %q, want :7777 (env > .env > JSON)", cfg.ListenAddr)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "env-tok" {
		t.Errorf("AuthTokens = %v, want [env-tok] (env wins)", cfg.AuthTokens)
	}
	if cfg.LogFile != "from-json.log" {
		t.Errorf("LogFile = %q, want from-json.log (JSON-only key survives)", cfg.LogFile)
	}
	if cfg.CLIVersion != "9.9.9" {
		t.Errorf("CLIVersion = %q, want 9.9.9 (from .env)", cfg.CLIVersion)
	}
	if !cfg.SessionPersist {
		t.Error("SessionPersist = false, want true (env-only key applies)")
	}
	if cfg.CostMode != "free" {
		t.Errorf("CostMode = %q, want free (JSON default kept)", cfg.CostMode)
	}
}

// TestAdminToken verifies ADMIN_TOKEN loads from env, JSON config, and .env
// with the standard precedence (env > .env > JSON).
func TestAdminToken(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_TOKEN", "from-env")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load (env): %v", err)
	}
	if cfg.AdminToken != "from-env" {
		t.Errorf("AdminToken = %q, want from-env (env)", cfg.AdminToken)
	}

	// JSON file value loses to the environment.
	if err := os.WriteFile("cfg.json", []byte(`{"ADMIN_TOKEN":"from-json"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load("cfg.json")
	if err != nil {
		t.Fatalf("Load (json): %v", err)
	}
	if cfg.AdminToken != "from-env" {
		t.Errorf("AdminToken = %q, want from-env (env beats JSON)", cfg.AdminToken)
	}

	// dotenv value applies when the environment is empty, and wins over JSON.
	clearEnv(t)
	if err := os.WriteFile(".env", []byte("ADMIN_TOKEN=from-dotenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// clearEnv chdirs to a fresh temp dir, so the cfg.json written above is
	// no longer in the working directory; re-write it next to .env.
	if err := os.WriteFile("cfg.json", []byte(`{"ADMIN_TOKEN":"from-json"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load("cfg.json")
	if err != nil {
		t.Fatalf("Load (dotenv): %v", err)
	}
	if cfg.AdminToken != "from-dotenv" {
		t.Errorf("AdminToken = %q, want from-dotenv (.env beats JSON)", cfg.AdminToken)
	}
}
func TestAdminTokenDefault(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminToken != DefaultAdminToken {
		t.Errorf("AdminToken = %q, want default %q", cfg.AdminToken, DefaultAdminToken)
	}
	if !cfg.IsDefaultAdminToken() {
		t.Errorf("IsDefaultAdminToken() = false, want true")
	}

	t.Setenv("ADMIN_TOKEN", "custom-token")
	cfg2, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.AdminToken != "custom-token" {
		t.Errorf("AdminToken = %q, want custom-token", cfg2.AdminToken)
	}
	if cfg2.IsDefaultAdminToken() {
		t.Errorf("IsDefaultAdminToken() = true, want false")
	}
}
