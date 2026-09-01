package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/clicreds"
)

func TestDotenv(t *testing.T) {
	clearEnv(t) // chdirs to a fresh temp dir; .env is written relative to it

	content := strings.Join([]string{
		"# comment",
		"",
		"AUTH_TOKENS=from-dotenv",
		`LISTEN_ADDR=":9999"`,
		"COST_MODE=free",
		"DEBUG_DUMP=true",
	}, "\n")
	if err := os.WriteFile(".env", []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "from-dotenv" {
		t.Errorf("AuthTokens = %v, want [from-dotenv] (from .env)", got)
	}
	if cfg.ListenAddr != ":9999" {
		t.Errorf("ListenAddr = %q, want :9999 (quoted value from .env)", cfg.ListenAddr)
	}
	if cfg.CostMode != "free" {
		t.Errorf("CostMode = %q, want free", cfg.CostMode)
	}
	if !cfg.DebugDump {
		t.Error("DebugDump = false, want true")
	}
}

func TestDotenvStripsBOM(t *testing.T) {
	clearEnv(t)

	// A UTF-8 BOM on the first line would corrupt the first key into
	// "\ufeffAUTH_TOKENS" and the token would silently drop out of the pool;
	// PowerShell writers with a UTF-8-with-BOM default produce this shape.
	if err := os.WriteFile(".env", []byte("\xef\xbb\xbfAUTH_TOKENS=tok-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "tok-1" {
		t.Errorf("AuthTokens = %v, want [tok-1] (BOM must not corrupt the first key)", got)
	}
}

func TestDotenvEnvWins(t *testing.T) {
	clearEnv(t)

	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=from-dotenv\nLISTEN_ADDR=:1111\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTH_TOKENS", "from-env")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "from-env" {
		t.Errorf("AuthTokens = %v, want [from-env] (env beats .env)", got)
	}
	if cfg.ListenAddr != ":1111" {
		t.Errorf("ListenAddr = %q, want :1111 (from .env, env does not set it)", cfg.ListenAddr)
	}
}

func TestDotenvJSONWins(t *testing.T) {
	clearEnv(t)

	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=from-dotenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("cfg.json", []byte(`{"AUTH_TOKENS":["from-json"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// .env is an environment file: it wins over the JSON config, matching
	// the README rule "environment overrides the JSON config file".
	cfg, err := Load("cfg.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "from-dotenv" {
		t.Errorf("AuthTokens = %v, want [from-dotenv] (.env beats JSON)", got)
	}
}

// TestDotenvEmptyAuthTokensClearsJSON is the regression for the dashboard
// mode switch: it persists exactly "AUTH_TOKENS=" into .env, which must
// clear tokens that came from a -config JSON file — otherwise the reload
// keeps the old tokens, BridgeMode() stays false, and the dashboard pill
// still shows the old mode after "Switch to bridge mode".
func TestDotenvEmptyAuthTokensClearsJSON(t *testing.T) {
	clearEnv(t)

	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("cfg.json", []byte(`{"AUTH_TOKENS":["from-json"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("cfg.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AuthTokens) != 0 {
		t.Errorf("AuthTokens = %v, want empty (empty .env AUTH_TOKENS clears JSON tokens)", cfg.AuthTokens)
	}
	if !cfg.BridgeMode() {
		t.Error("BridgeMode() = false, want true after explicit empty AUTH_TOKENS")
	}
	if cfg.DiscoveredSource != "" {
		t.Errorf("DiscoveredSource = %q, want empty (auto-discovery must stay suppressed)", cfg.DiscoveredSource)
	}
}

// TestEnvEmptyAuthTokensBridgeMode verifies that an explicitly-empty
// AUTH_TOKENS in the real environment (the shape systemd/Docker unit files
// use to force bridge mode) records presence: cfg.AuthTokens stays empty,
// BridgeMode() is true, and CLI auto-discovery must NOT refill the pool.
// Regression: overrideCSV skipped empty values, so AUTH_TOKENS= left
// AuthTokensSet false and a local CLI login silently flipped bridge mode to
// pooled mode under systemd/Docker.
func TestEnvEmptyAuthTokensBridgeMode(t *testing.T) {
	clearEnv(t)
	// Re-enable auto-discovery; the explicit-empty AUTH_TOKENS below is what
	// must suppress it (not the AUTO_DISCOVER_TOKEN=off switch).
	t.Setenv("AUTO_DISCOVER_TOKEN", "")
	t.Setenv("AUTH_TOKENS", "")

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

	cfg, err := LoadOpts("", LoadOptions{DiscoverCLIToken: clicreds.DiscoverToken})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if len(cfg.AuthTokens) != 0 {
		t.Errorf("AuthTokens = %v, want empty (explicit bridge mode, not refilled by discovery)", cfg.AuthTokens)
	}
	if !cfg.BridgeMode() {
		t.Error("BridgeMode() = false, want true with explicitly-empty AUTH_TOKENS")
	}
	if cfg.DiscoveredSource != "" {
		t.Errorf("DiscoveredSource = %q, want empty (auto-discovery must be suppressed)", cfg.DiscoveredSource)
	}
}

// TestEnvAuthTokensSetsTokens verifies the non-empty AUTH_TOKENS=a,b env
// path lands in cfg.AuthTokens with explicit-presence semantics (a partial
// pool must never trigger CLI auto-discovery either).
func TestEnvAuthTokensSetsTokens(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "a,b")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"a", "b"}; !equalStrings(cfg.AuthTokens, want) {
		t.Errorf("AuthTokens = %v, want %v", cfg.AuthTokens, want)
	}
	if cfg.BridgeMode() {
		t.Error("BridgeMode() = true, want false with AUTH_TOKENS=a,b")
	}
}

func TestDotenvMissingIsFine(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "tok-1" {
		t.Errorf("AuthTokens = %v, want [tok-1]", got)
	}
}

// TestEnvEmptyAuthTokensClearsDotenv is the C1 precedence hole: an
// explicitly-empty AUTH_TOKENS in the real environment (the shape
// systemd/Docker unit files use to force bridge mode) must clear tokens that
// came from ./.env — the empty env value wins like any other env override —
// and suppress CLI auto-discovery.
func TestEnvEmptyAuthTokensClearsDotenv(t *testing.T) {
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

	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=from-dotenv\nLISTEN_ADDR=:1111\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTH_TOKENS", "")

	cfg, err := LoadOpts("", LoadOptions{DiscoverCLIToken: clicreds.DiscoverToken})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if len(cfg.AuthTokens) != 0 {
		t.Errorf("AuthTokens = %v, want empty (empty env AUTH_TOKENS clears .env tokens)", cfg.AuthTokens)
	}
	if !cfg.BridgeMode() {
		t.Error("BridgeMode() = false, want true after explicit empty env AUTH_TOKENS")
	}
	if cfg.ListenAddr != ":1111" {
		t.Errorf("ListenAddr = %q, want :1111 (non-token .env keys still apply)", cfg.ListenAddr)
	}
	if cfg.DiscoveredSource != "" {
		t.Errorf("DiscoveredSource = %q, want empty (auto-discovery must stay suppressed)", cfg.DiscoveredSource)
	}
}

// TestDotenvCRLF verifies a CRLF .env file (the line-ending default on
// Windows editors) parses cleanly: \r is trimmed along with surrounding
// whitespace on every line.
func TestDotenvCRLF(t *testing.T) {
	clearEnv(t)

	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=from-dotenv\r\nLISTEN_ADDR=:1111\r\nCOST_MODE=free\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load (CRLF .env): %v", err)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "from-dotenv" {
		t.Errorf("AuthTokens = %v, want [from-dotenv] (CRLF must not corrupt keys/values)", got)
	}
	if cfg.ListenAddr != ":1111" {
		t.Errorf("ListenAddr = %q, want :1111", cfg.ListenAddr)
	}
	if cfg.CostMode != "free" {
		t.Errorf("CostMode = %q, want free", cfg.CostMode)
	}
}

// TestDotenvDuplicateKeyLastWins verifies the sequential-override semantics
// of parseDotenv: when the same KEY appears twice, the LAST value wins.
func TestDotenvDuplicateKeyLastWins(t *testing.T) {
	clearEnv(t)

	if err := os.WriteFile(".env", []byte("LISTEN_ADDR=:1111\nLISTEN_ADDR=:2222\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":2222" {
		t.Errorf("ListenAddr = %q, want :2222 (duplicate key: last wins)", cfg.ListenAddr)
	}
}

// TestDotenvAsDirectoryFails verifies a ./.env path that is a directory (or
// otherwise unreadable with a non-NotExist error) fails the Load instead of
// being silently treated as absent.
func TestDotenvAsDirectoryFails(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	if err := os.Mkdir(".env", 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(""); err == nil {
		t.Fatal("Load with .env as a directory succeeded, want error")
	}
}

// TestDotenvFullKeySet verifies every env-overridable key also lands in cfg
// when set in ./.env. Regression: SAFE_MODE, REQUEST_JITTER, CLI_VERSION,
// MODEL_ALIASES and TRANSIENT_RETRIES were silently ignored in .env.
func TestDotenvFullKeySet(t *testing.T) {
	clearEnv(t)

	content := strings.Join([]string{
		"SAFE_MODE=false",
		"REQUEST_JITTER=5s",
		"CLI_VERSION=9.9.9",
		"MODEL_ALIASES=gpt-4o:deepseek/deepseek-v4-flash,glm:z-ai/glm-5.2",
		"MODELS_ALLOW=deepseek/deepseek-v4-flash,z-ai/glm-5.2",
		"CORS_ALLOWED_ORIGIN=https://dashboard.example.com",
		"TRANSIENT_RETRIES=2",
		"MAX_SPEND_PER_DAY=500",
	}, "\n")
	if err := os.WriteFile(".env", []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SafeMode {
		t.Error("SafeMode = true, want false (from .env)")
	}
	if cfg.RequestJitter != 5*time.Second {
		t.Errorf("RequestJitter = %v, want 5s (from .env)", cfg.RequestJitter)
	}
	if cfg.CLIVersion != "9.9.9" {
		t.Errorf("CLIVersion = %q, want 9.9.9 (from .env)", cfg.CLIVersion)
	}
	if cfg.ModelAliases["gpt-4o"] != "deepseek/deepseek-v4-flash" {
		t.Errorf("ModelAliases[gpt-4o] = %q, want deepseek/deepseek-v4-flash (from .env)", cfg.ModelAliases["gpt-4o"])
	}
	if cfg.TransientRetries != 2 {
		t.Errorf("TransientRetries = %d, want 2 (from .env)", cfg.TransientRetries)
	}
	if want := []string{"deepseek/deepseek-v4-flash", "z-ai/glm-5.2"}; !equalStrings(cfg.ModelsAllow, want) {
		t.Errorf("ModelsAllow = %v, want %v (from .env)", cfg.ModelsAllow, want)
	}
	if cfg.CORSAllowedOrigin != "https://dashboard.example.com" {
		t.Errorf("CORSAllowedOrigin = %q, want https://dashboard.example.com (from .env)", cfg.CORSAllowedOrigin)
	}
	if cfg.MaxSpendPerDay != 500 {
		t.Errorf("MaxSpendPerDay = %d, want 500 (from .env)", cfg.MaxSpendPerDay)
	}
}

// TestDotenvFullKeySetEnvWins verifies the real environment still beats the
// .env file for the newly mirrored keys.
func TestDotenvFullKeySetEnvWins(t *testing.T) {
	clearEnv(t)

	if err := os.WriteFile(".env", []byte("SAFE_MODE=false\nCLI_VERSION=9.9.9\nTRANSIENT_RETRIES=2\nCORS_ALLOWED_ORIGIN=https://dotenv.example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAFE_MODE", "true")
	t.Setenv("CLI_VERSION", "1.2.3")
	t.Setenv("TRANSIENT_RETRIES", "5")
	t.Setenv("CORS_ALLOWED_ORIGIN", "https://env.example.com")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.SafeMode {
		t.Error("SafeMode = false, want true (env wins over .env)")
	}
	if cfg.CLIVersion != "1.2.3" {
		t.Errorf("CLIVersion = %q, want 1.2.3 (env wins)", cfg.CLIVersion)
	}
	if cfg.TransientRetries != 5 {
		t.Errorf("TransientRetries = %d, want 5 (env wins)", cfg.TransientRetries)
	}
	if cfg.CORSAllowedOrigin != "https://env.example.com" {
		t.Errorf("CORSAllowedOrigin = %q, want https://env.example.com (env wins)", cfg.CORSAllowedOrigin)
	}
}

func TestReadDotenvQuotingAndComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := strings.Join([]string{
		`KEY=value # inline comment`,
		`QUOTED="a b # kept"`,
		`PAIR='single quoted'`,
		`TRAILING="a b" # comment`,
		`UNMATCHED="stray`,
		`EMPTY=""`,
		`RAW=a"b`,
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readDotenv(path)
	if err != nil {
		t.Fatalf("readDotenv: %v", err)
	}
	want := map[string]string{
		"KEY":       "value",
		"QUOTED":    "a b # kept",
		"PAIR":      "single quoted",
		"TRAILING":  "a b",
		"UNMATCHED": `"stray`,
		"EMPTY":     "",
		"RAW":       `a"b`,
	}
	for k, wantVal := range want {
		if got[k] != wantVal {
			t.Errorf("readDotenv[%q] = %q, want %q", k, got[k], wantVal)
		}
	}
	if len(got) != len(want) {
		t.Errorf("readDotenv returned %d keys, want %d", len(got), len(want))
	}
}

func TestEnvOnly(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "a,b")
	t.Setenv("UPSTREAM_BASE_URL", "https://codebuff.com/")
	t.Setenv("SESSION_CALL_TIMEOUT", "45s")
	t.Setenv("DEBUG_DUMP", "off")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UpstreamBaseURL != "https://www.codebuff.com" {
		t.Errorf("UpstreamBaseURL = %q", cfg.UpstreamBaseURL)
	}
	if cfg.SessionCallTimeout != 45*time.Second {
		t.Errorf("SessionCallTimeout = %v, want 45s", cfg.SessionCallTimeout)
	}
	if cfg.DebugDump {
		t.Error("DebugDump = true, want false (off)")
	}
}

// TestCORSAllowedOrigin pins the CORS_ALLOWED_ORIGIN env parsing: the env
// value (or a JSON/.env value) overrides the "*" default, and a whitespace-
// only value is skipped (overrideStringFrom convention) so the "*" default
// stays — an empty/whitespace .env line cannot disable CORS.
func TestCORSAllowedOrigin(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("CORS_ALLOWED_ORIGIN", "https://app.example.com")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CORSAllowedOrigin != "https://app.example.com" {
		t.Errorf("CORSAllowedOrigin = %q, want env override", cfg.CORSAllowedOrigin)
	}

	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("CORS_ALLOWED_ORIGIN", "   ")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load(blank): %v", err)
	}
	if cfg.CORSAllowedOrigin != "*" {
		t.Errorf("CORSAllowedOrigin = %q, want retained default %q (whitespace-only env is skipped)", cfg.CORSAllowedOrigin, "*")
	}
}

// TestSessionPersist pins the SESSION_PERSIST env parsing: an unrecognized
// boolean value is silently ignored (the default stays true — no error),
// "false" disables persistence, and "true" explicitly enables it.
func TestSessionPersist(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")

	// garbage value is silently ignored, default (true) stands
	t.Setenv("SESSION_PERSIST", "garbage")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (garbage): %v", err)
	} else if !cfg.SessionPersist {
		t.Error("SessionPersist = false for SESSION_PERSIST=garbage, want true (unrecognized value ignored)")
	}
	t.Setenv("SESSION_PERSIST", "")

	// recognized false value disables persistence
	t.Setenv("SESSION_PERSIST", "false")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (false): %v", err)
	} else if cfg.SessionPersist {
		t.Error("SessionPersist = true for SESSION_PERSIST=false, want false")
	}
	t.Setenv("SESSION_PERSIST", "")

	// recognized true value enables persistence and honors the state file
	t.Setenv("SESSION_PERSIST", "true")
	t.Setenv("SESSION_STATE_FILE", "custom-state.json")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (true): %v", err)
	} else if !cfg.SessionPersist {
		t.Error("SessionPersist = false for SESSION_PERSIST=true, want true")
	} else if cfg.SessionStateFile != "custom-state.json" {
		t.Errorf("SessionStateFile = %q, want %q", cfg.SessionStateFile, "custom-state.json")
	}
}

// TestSessionPersistEmptyStateFileEnv pins the combined env case: an empty
// SESSION_STATE_FILE env value is treated as unset (the default path
// stands), so SESSION_PERSIST=true + SESSION_STATE_FILE= loads cleanly with
// the default path — the validation error is only reachable through the
// JSON/struct path (pinned in TestValidate).
func TestSessionPersistEmptyStateFileEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("SESSION_PERSIST", "true")
	t.Setenv("SESSION_STATE_FILE", "")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load (SESSION_PERSIST=true, SESSION_STATE_FILE=): %v", err)
	}
	if !cfg.SessionPersist {
		t.Error("SessionPersist = false, want true")
	}
	if cfg.SessionStateFile != ".freebuff-session-state.json" {
		t.Errorf("SessionStateFile = %q, want default %q (empty env leaves the default)", cfg.SessionStateFile, ".freebuff-session-state.json")
	}
}

// TestModelsHideUnavailableEnv verifies MODELS_HIDE_UNAVAILABLE loads from
// env and lands in Config.
func TestModelsHideUnavailableEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("MODELS_HIDE_UNAVAILABLE", "true")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ModelsHideUnavailable {
		t.Error("ModelsHideUnavailable = false, want true (from env)")
	}
}

// TestModelUnavailableCacheTTLEnv verifies MODEL_UNAVAILABLE_CACHE_TTL loads
// from env and lands in Config, and that an explicit "0" falls back to the
// documented 1h default (zero-tolerant, matching the other session knobs).
func TestModelUnavailableCacheTTLEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("MODEL_UNAVAILABLE_CACHE_TTL", "45m")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ModelUnavailableCacheTTL != 45*time.Minute {
		t.Errorf("ModelUnavailableCacheTTL = %v, want 45m (from env)", cfg.ModelUnavailableCacheTTL)
	}

	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("MODEL_UNAVAILABLE_CACHE_TTL", "0")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ModelUnavailableCacheTTL != time.Hour {
		t.Errorf("ModelUnavailableCacheTTL = %v, want 1h (zero-tolerant default)", cfg.ModelUnavailableCacheTTL)
	}

	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("MODEL_UNAVAILABLE_CACHE_TTL", "bogus")
	if _, err := Load(""); err == nil {
		t.Error("bogus MODEL_UNAVAILABLE_CACHE_TTL accepted")
	}
}

func TestMaxMessagesPerDay(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	// default: 0 (unlimited; no SafeMode preset)
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load: %v", err)
	} else if cfg.MaxMessagesPerDay != 0 {
		t.Errorf("MaxMessagesPerDay = %d, want 0 (unlimited default)", cfg.MaxMessagesPerDay)
	}

	// SAFE_MODE=false restores unlimited
	t.Setenv("SAFE_MODE", "false")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (SAFE_MODE=false): %v", err)
	} else if cfg.MaxMessagesPerDay != 0 {
		t.Errorf("MaxMessagesPerDay = %d, want 0 (unlimited with SAFE_MODE=false)", cfg.MaxMessagesPerDay)
	}
	t.Setenv("SAFE_MODE", "")

	// env override
	t.Setenv("MAX_MESSAGES_PER_DAY", "25")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env): %v", err)
	} else if cfg.MaxMessagesPerDay != 25 {
		t.Errorf("MaxMessagesPerDay = %d, want 25 (env)", cfg.MaxMessagesPerDay)
	}

	// unparseable env value is ignored (keeps the file value)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"MAX_MESSAGES_PER_DAY": 3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAX_MESSAGES_PER_DAY", "soon")
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (bad env + file): %v", err)
	} else if cfg.MaxMessagesPerDay != 3 {
		t.Errorf("MaxMessagesPerDay = %d, want 3 (bad env ignored, file kept)", cfg.MaxMessagesPerDay)
	}

	// JSON file value
	t.Setenv("MAX_MESSAGES_PER_DAY", "")
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (file): %v", err)
	} else if cfg.MaxMessagesPerDay != 3 {
		t.Errorf("MaxMessagesPerDay = %d, want 3 (file)", cfg.MaxMessagesPerDay)
	}
}

// TestMaxSpendPerDay pins the advisory spend-ceiling knob (issue #122):
// default 0 (unlimited), env override, unparseable env ignored, JSON file
// value, and .env value. The knob is advisory-only — the upstream $ ceilings
// are server-enforced and the pool never blocks on it.
func TestMaxSpendPerDay(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	// default: 0 (unlimited)
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load: %v", err)
	} else if cfg.MaxSpendPerDay != 0 {
		t.Errorf("MaxSpendPerDay = %d, want 0 (unlimited default)", cfg.MaxSpendPerDay)
	}

	// env override
	t.Setenv("MAX_SPEND_PER_DAY", "1000")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env): %v", err)
	} else if cfg.MaxSpendPerDay != 1000 {
		t.Errorf("MaxSpendPerDay = %d, want 1000 (env)", cfg.MaxSpendPerDay)
	}

	// unparseable env value is ignored (keeps the file value)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"MAX_SPEND_PER_DAY": 250}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAX_SPEND_PER_DAY", "soon")
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (bad env + file): %v", err)
	} else if cfg.MaxSpendPerDay != 250 {
		t.Errorf("MaxSpendPerDay = %d, want 250 (bad env ignored, file kept)", cfg.MaxSpendPerDay)
	}

	// JSON file value
	t.Setenv("MAX_SPEND_PER_DAY", "")
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (file): %v", err)
	} else if cfg.MaxSpendPerDay != 250 {
		t.Errorf("MaxSpendPerDay = %d, want 250 (file)", cfg.MaxSpendPerDay)
	}

	// .env value (clearEnv chdirs to a fresh temp dir, so ./.env is the
	// file ResolveEnvFile reads)
	t.Setenv("MAX_SPEND_PER_DAY", "")
	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok\nMAX_SPEND_PER_DAY=75\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (.env): %v", err)
	} else if cfg.MaxSpendPerDay != 75 {
		t.Errorf("MaxSpendPerDay = %d, want 75 (from .env)", cfg.MaxSpendPerDay)
	}
}

func TestRateLimitConfig(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	// Default is disabled (0, 0)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	if cfg.RateLimitPerIP != 0 || cfg.RateLimitBurst != 0 {
		t.Errorf("default RateLimit = (%v, %v), want (0, 0)", cfg.RateLimitPerIP, cfg.RateLimitBurst)
	}

	// Override via environment variables
	t.Setenv("RATE_LIMIT_PER_IP", "25.5")
	t.Setenv("RATE_LIMIT_BURST", "50")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfg.RateLimitPerIP != 25.5 || cfg.RateLimitBurst != 50 {
		t.Errorf("RateLimit from env = (%v, %v), want (25.5, 50)", cfg.RateLimitPerIP, cfg.RateLimitBurst)
	}
}

func TestModelAliasesConfig(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("MODEL_ALIASES", "gpt-4o:deepseek/deepseek-v4-flash,glm:z-ai/glm-5.2")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ModelAliases) != 2 {
		t.Fatalf("ModelAliases len = %d, want 2", len(cfg.ModelAliases))
	}
	if cfg.ModelAliases["gpt-4o"] != "deepseek/deepseek-v4-flash" {
		t.Errorf("ModelAliases[gpt-4o] = %q, want deepseek/deepseek-v4-flash", cfg.ModelAliases["gpt-4o"])
	}
}
