package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/testutil"
)

// envKeys lists every environment variable the package reads. Tests clear
// them all first so machine-level env can never leak into assertions.
var envKeys = []string{
	"LISTEN_ADDR", "UPSTREAM_BASE_URL", "AUTH_TOKENS", "ROTATION_INTERVAL",
	"REQUEST_TIMEOUT", "SESSION_CALL_TIMEOUT", "API_KEYS", "COST_MODE", "ACTING_USER_ID", "USER_ID",
	"TLS_FINGERPRINT", "REGISTRY_REFRESH", "DEBUG_DUMP", "LOG_FILE", "LOG_LEVEL", "LOG_FORMAT", "LOG_ACCESS", "LOG_RING_SIZE",
	"MAX_MESSAGES_PER_DAY", "IDLE_ROTATION_TIMEOUT", "SAFE_MODE", "HYBRID_MODE",
	"MODELS_HIDE_UNAVAILABLE", "MODELS_ALLOW", "CORS_ALLOWED_ORIGIN", "REQUEST_JITTER", "CLI_VERSION", "MODEL_ALIASES",
	"AUTO_DISCOVER_TOKEN", "TRANSIENT_RETRIES", "ADMIN_TOKEN",
	"SESSION_PERSIST", "SESSION_STATE_FILE",
	"HTTP2_UPSTREAM",
	"MAX_SPEND_PER_DAY", "SESSION_RE_ADMIT_LEAD", "SESSION_PROBE_CACHE_TTL",
	"SESSION_CREATE_MAX_PARALLEL_GLOBAL", "SESSION_CREATE_MAX_PARALLEL_PER_MODEL",
	"RUN_FINISH_QUEUE_SIZE", "RUN_FINISH_INLINE_TIMEOUT", "RUNS_DRAIN_QUEUE_CAP", "RUNS_DRAIN_TTL",
	"WEBHOOK_URL", "FALLBACK_AFTER_MS", "FALLBACK_MODEL", "ADOPT_CLI_SESSION", "WAITING_ROOM_CHAIN",
	"PREFER_MAX_MODELS", "ACCESS_TIER",
}

// TestMain strips ambient freebuff-proxy config env vars for the whole test
// binary (testutil.UnsetConfigEnvForTestMain). clearEnv in each test covers
// the per-test isolation, but a developer's exported SESSION_PERSIST /
// MODELS_HIDE_UNAVAILABLE / SESSION_STATE_FILE would
// otherwise leak into package-level behavior before the first clearEnv runs
// (e.g. TestDefaults / TestSessionPersist assert on those defaults).
func TestMain(m *testing.M) {
	testutil.UnsetConfigEnvForTestMain()
	os.Exit(m.Run())
}

func clearEnv(t *testing.T) {
	t.Helper()
	// Isolate from any real ./.env in the working directory (the repo ships
	// a gitignored .env with real tokens) — Load() reads it by default.
	t.Chdir(t.TempDir())
	for _, k := range envKeys {
		// AUTH_TOKENS is presence-sensitive (an empty value is an explicit
		// bridge-mode choice), so the neutral test state is ABSENT, not
		// empty: setting it to "" would record presence and suppress
		// auto-discovery in every test. Unsetting also blocks a
		// machine-level AUTH_TOKENS leak into assertions.
		if k == "AUTH_TOKENS" {
			_ = os.Unsetenv(k)
			continue
		}
		t.Setenv(k, "")
	}
	t.Setenv("AUTO_DISCOVER_TOKEN", "false")
}

func TestDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ListenAddr != "127.0.0.1:3457" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, "127.0.0.1:3457")
	}
	if cfg.UpstreamBaseURL != "https://www.codebuff.com" {
		t.Errorf("UpstreamBaseURL = %q, want %q", cfg.UpstreamBaseURL, "https://www.codebuff.com")
	}
	if want := []string{"tok-1"}; !equalStrings(cfg.AuthTokens, want) {
		t.Errorf("AuthTokens = %v, want %v", cfg.AuthTokens, want)
	}
	if cfg.RotationInterval != 6*time.Hour {
		t.Errorf("RotationInterval = %v, want 6h", cfg.RotationInterval)
	}
	if cfg.RequestTimeout != 15*time.Minute {
		t.Errorf("RequestTimeout = %v, want 15m", cfg.RequestTimeout)
	}
	if cfg.SessionCallTimeout != 30*time.Second {
		t.Errorf("SessionCallTimeout = %v, want 30s", cfg.SessionCallTimeout)
	}
	if cfg.RegistryRefresh != 6*time.Hour {
		t.Errorf("RegistryRefresh = %v, want 6h", cfg.RegistryRefresh)
	}
	if cfg.DebugDump {
		t.Error("DebugDump = true, want false")
	}
	if !cfg.SafeMode {
		t.Error("SafeMode = false, want true (default)")
	}
	if cfg.HybridMode {
		t.Error("HybridMode = true, want false (default)")
	}
	if cfg.CORSAllowedOrigin != "*" {
		t.Errorf("CORSAllowedOrigin = %q, want %q (default)", cfg.CORSAllowedOrigin, "*")
	}
	if got := cfg.EffectiveMode(); got != "pooled" {
		t.Errorf("EffectiveMode = %q, want pooled", got)
	}
	if cfg.LogFile != "" {
		t.Errorf("LogFile = %q, want empty", cfg.LogFile)
	}
	if cfg.LogLevel != "" {
		t.Errorf("LogLevel = %q, want empty", cfg.LogLevel)
	}
	if cfg.CostMode != "free" {
		t.Errorf("CostMode = %q, want free (default: omission routes requests as paid -> 402)", cfg.CostMode)
	}
	if cfg.SessionPersist {
		t.Error("SessionPersist = true, want false (default: persistence opt-in)")
	}
	if cfg.SessionStateFile != ".freebuff-session-state.json" {
		t.Errorf("SessionStateFile = %q, want %q", cfg.SessionStateFile, ".freebuff-session-state.json")
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
// boolean value is silently ignored (the default stays false — no error),
// and "true" enables persistence with the configured state file path. The
// SESSION_PERSIST=true + empty SESSION_STATE_FILE validation error is only
// reachable through the JSON/struct path (an env value of "" is treated as
// unset and leaves the default), so it is pinned in TestValidate.
func TestSessionPersist(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")

	// garbage value is silently ignored, default (false) stands
	t.Setenv("SESSION_PERSIST", "garbage")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (garbage): %v", err)
	} else if cfg.SessionPersist {
		t.Error("SessionPersist = true for SESSION_PERSIST=garbage, want false (unrecognized value ignored)")
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

func TestTransientRetries(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")

	// default: 1 (one additional attempt after a transient transport failure)
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (default): %v", err)
	} else if cfg.TransientRetries != 1 {
		t.Errorf("TransientRetries = %d, want 1 (default)", cfg.TransientRetries)
	}

	// explicit 0 disables retries
	t.Setenv("TRANSIENT_RETRIES", "0")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (0): %v", err)
	} else if cfg.TransientRetries != 0 {
		t.Errorf("TransientRetries = %d, want 0 (disabled)", cfg.TransientRetries)
	}
	t.Setenv("TRANSIENT_RETRIES", "")

	// JSON file value
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"TRANSIENT_RETRIES": 2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (JSON): %v", err)
	} else if cfg.TransientRetries != 2 {
		t.Errorf("TransientRetries = %d, want 2 (JSON)", cfg.TransientRetries)
	}

	// negative fails validation
	t.Setenv("TRANSIENT_RETRIES", "-1")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "TRANSIENT_RETRIES") {
		t.Fatalf("Load (negative): err = %v, want error mentioning TRANSIENT_RETRIES", err)
	}
	t.Setenv("TRANSIENT_RETRIES", "")
}

// TestLogRingSize pins the T19 LOG_RING_SIZE knob: default 500 when unset,
// an empty value keeps the default, explicit values must stay within
// 50..5000 (below the floor / above the cap fail validation), and the JSON
// and .env sources both apply.
func TestLogRingSize(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	// default: 500 when unset
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (default): %v", err)
	} else if cfg.LogRingSize != 500 {
		t.Errorf("LogRingSize default = %d, want 500", cfg.LogRingSize)
	}

	// explicit empty value keeps the default
	t.Setenv("LOG_RING_SIZE", "")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (empty): %v", err)
	} else if cfg.LogRingSize != 500 {
		t.Errorf("LogRingSize (empty) = %d, want 500", cfg.LogRingSize)
	}

	// env source: a valid value loads
	t.Setenv("LOG_RING_SIZE", "2000")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env 2000): %v", err)
	} else if cfg.LogRingSize != 2000 {
		t.Errorf("LogRingSize (env) = %d, want 2000", cfg.LogRingSize)
	}

	// boundary values are accepted
	for _, v := range []string{"50", "5000"} {
		t.Setenv("LOG_RING_SIZE", v)
		n, _ := strconv.Atoi(v)
		if cfg, err := Load(""); err != nil {
			t.Fatalf("Load (LOG_RING_SIZE=%s): %v", v, err)
		} else if cfg.LogRingSize != n {
			t.Errorf("LogRingSize (LOG_RING_SIZE=%s) = %d, want %d", v, cfg.LogRingSize, n)
		}
	}

	// below the floor fails validation
	t.Setenv("LOG_RING_SIZE", "49")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "LOG_RING_SIZE") {
		t.Fatalf("Load (49): err = %v, want validation error mentioning LOG_RING_SIZE", err)
	}

	// above the cap fails validation
	t.Setenv("LOG_RING_SIZE", "5001")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "LOG_RING_SIZE") {
		t.Fatalf("Load (5001): err = %v, want validation error mentioning LOG_RING_SIZE", err)
	}
	t.Setenv("LOG_RING_SIZE", "")

	// JSON file source
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"LOG_RING_SIZE": 750}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (file): %v", err)
	} else if cfg.LogRingSize != 750 {
		t.Errorf("LogRingSize (file) = %d, want 750", cfg.LogRingSize)
	}

	// .env source (applyDotenv)
	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok\nLOG_RING_SIZE=900\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (.env): %v", err)
	} else if cfg.LogRingSize != 900 {
		t.Errorf("LogRingSize (.env) = %d, want 900", cfg.LogRingSize)
	}
}

func TestSafeMode(t *testing.T) {
	t.Run("default SafeMode values", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("SAFE_MODE", "true")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		if cfg.MaxMessagesPerDay != 0 {
			t.Errorf("MaxMessagesPerDay = %d, want 0 (unlimited default, no SafeMode preset)", cfg.MaxMessagesPerDay)
		}
		if cfg.IdleRotationTimeout != 30*time.Minute {
			t.Errorf("IdleRotationTimeout = %v, want 30m under SafeMode", cfg.IdleRotationTimeout)
		}
		if cfg.RequestJitter != 2*time.Second {
			t.Errorf("RequestJitter = %v, want 2s under SafeMode", cfg.RequestJitter)
		}
	})

	t.Run("explicit zero MaxMessagesPerDay under SafeMode", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("SAFE_MODE", "true")
		t.Setenv("MAX_MESSAGES_PER_DAY", "0")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		if cfg.MaxMessagesPerDay != 0 {
			t.Errorf("MaxMessagesPerDay = %d, want 0 (explicit unlimited)", cfg.MaxMessagesPerDay)
		}
		if cfg.RequestJitter != 2*time.Second {
			t.Errorf("RequestJitter = %v, want 2s under SafeMode", cfg.RequestJitter)
		}
	})

	t.Run("explicit zero knobs under SafeMode stay disabled", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("SAFE_MODE", "true")
		t.Setenv("IDLE_ROTATION_TIMEOUT", "0")
		t.Setenv("REQUEST_JITTER", "0s")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		if cfg.IdleRotationTimeout != 0 {
			t.Errorf("IdleRotationTimeout = %v, want 0 (explicit 0 beats the preset)", cfg.IdleRotationTimeout)
		}
		if cfg.RequestJitter != 0 {
			t.Errorf("RequestJitter = %v, want 0 (explicit 0 beats the preset)", cfg.RequestJitter)
		}
		// Unset knobs still get the presets; the daily cap stays unlimited.
		if cfg.MaxMessagesPerDay != 0 {
			t.Errorf("MaxMessagesPerDay = %d, want 0 (unlimited default, no SafeMode preset)", cfg.MaxMessagesPerDay)
		}
		if cfg.TLSFingerprint != "auto" {
			t.Errorf("TLSFingerprint = %q, want auto under SafeMode", cfg.TLSFingerprint)
		}
	})

	t.Run("SAFE_MODE=false restores non-safe defaults", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("SAFE_MODE", "false")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		if cfg.MaxMessagesPerDay != 0 {
			t.Errorf("MaxMessagesPerDay = %d, want 0 (unlimited)", cfg.MaxMessagesPerDay)
		}
		if cfg.IdleRotationTimeout != 0 {
			t.Errorf("IdleRotationTimeout = %v, want 0 (disabled)", cfg.IdleRotationTimeout)
		}
		if cfg.RequestJitter != 0 {
			t.Errorf("RequestJitter = %v, want 0 (disabled)", cfg.RequestJitter)
		}
		if cfg.TLSFingerprint != "" {
			t.Errorf("TLSFingerprint = %q, want empty", cfg.TLSFingerprint)
		}
	})
}

func TestValidationFixSuggestions(t *testing.T) {
	t.Run("Bearer prefix", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "Bearer token123")
		_, err := Load("")
		if err == nil || !strings.Contains(err.Error(), "starts with 'Bearer ' prefix") {
			t.Errorf("expected Bearer prefix error, got: %v", err)
		}
	})

	t.Run("Placeholder token", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "cb_xxx")
		_, err := Load("")
		if err == nil || !strings.Contains(err.Error(), "placeholder") {
			t.Errorf("expected placeholder error, got: %v", err)
		}
	})

	t.Run("ListenAddr missing colon", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok1")
		t.Setenv("LISTEN_ADDR", "3457")
		_, err := Load("")
		if err == nil || !strings.Contains(err.Error(), "missing port separator ':'") {
			t.Errorf("expected missing port separator error, got: %v", err)
		}
	})
}

func TestLoadNoTokensBridgeMode(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with no AUTH_TOKENS: %v", err)
	}
	if len(cfg.AuthTokens) != 0 {
		t.Errorf("AuthTokens = %v, want empty (bridge mode)", cfg.AuthTokens)
	}
	if !cfg.BridgeMode() {
		t.Error("BridgeMode() = false, want true with no tokens")
	}
}

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

func TestSplitList(t *testing.T) {
	got := splitList(" a, b ,,c , d\n e\r f ")
	want := []string{"a", "b", "c", "d", "e", "f"}
	if !equalStrings(got, want) {
		t.Errorf("splitList = %v, want %v", got, want)
	}
}

func TestDedupeStrings(t *testing.T) {
	got := dedupeStrings([]string{"a", " a ", "b", "", "b"})
	want := []string{"a", "b"}
	if !equalStrings(got, want) {
		t.Errorf("dedupeStrings = %v, want %v", got, want)
	}
}

func TestNormalizeUpstreamBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"bare host normalized", "https://codebuff.com", "https://www.codebuff.com", false},
		{"bare host + slash", "https://codebuff.com/", "https://www.codebuff.com", false},
		{"case-insensitive host", "https://CODEBUFF.COM", "https://www.codebuff.com", false},
		{"www kept", "https://www.codebuff.com", "https://www.codebuff.com", false},
		{"www kept + path", "https://www.codebuff.com/api", "https://www.codebuff.com/api", false},
		{"host with port untouched (reference parity)", "https://codebuff.com:8443/x/", "https://codebuff.com:8443/x", false},
		{"other host untouched", "https://api.example.com", "https://api.example.com", false},
		{"no scheme", "codebuff.com", "", true},
		{"bad scheme", "ftp://codebuff.com", "", true},
		{"unparseable", "https://exa mple.com", "", true},
		{"no host", "https://", "", true},
		{"empty", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeUpstreamBaseURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeUpstreamBaseURL(%q) = %q, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeUpstreamBaseURL(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("normalizeUpstreamBaseURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	good := Config{
		ListenAddr:         ":3457",
		UpstreamBaseURL:    "https://www.codebuff.com",
		AuthTokens:         []string{"tok"},
		RotationInterval:   6 * time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 30 * time.Second,
		RegistryRefresh:    6 * time.Hour,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good config Validate: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty url", func(c *Config) { c.UpstreamBaseURL = "" }},
		{"unparseable url", func(c *Config) { c.UpstreamBaseURL = "https://exa mple.com" }},
		{"non-http scheme", func(c *Config) { c.UpstreamBaseURL = "ftp://codebuff.com" }},
		{"hostless url", func(c *Config) { c.UpstreamBaseURL = "https://" }},
		{"empty listen addr", func(c *Config) { c.ListenAddr = "" }},
		{"zero rotation", func(c *Config) { c.RotationInterval = 0 }},
		{"zero request timeout", func(c *Config) { c.RequestTimeout = 0 }},
		{"zero session timeout", func(c *Config) { c.SessionCallTimeout = 0 }},
		{"zero registry refresh", func(c *Config) { c.RegistryRefresh = 0 }},
		{"bad cost mode", func(c *Config) { c.CostMode = "Free" }},
		{"negative max messages", func(c *Config) { c.MaxMessagesPerDay = -1 }},
		{"negative max spend", func(c *Config) { c.MaxSpendPerDay = -1 }},
		{"negative rate limit per ip", func(c *Config) { c.RateLimitPerIP = -1 }},
		{"negative rate limit burst", func(c *Config) { c.RateLimitBurst = -1 }},
		{"session persist with empty state file", func(c *Config) { c.SessionPersist = true; c.SessionStateFile = "" }},
		{"invalid listen port non-int", func(c *Config) { c.ListenAddr = "127.0.0.1:abc" }},
		{"invalid listen port overflow", func(c *Config) { c.ListenAddr = "127.0.0.1:99999" }},
		{"invalid listen port zero", func(c *Config) { c.ListenAddr = "127.0.0.1:0" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := good
			tt.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Error("Validate succeeded, want error")
			}
		})
	}
}

// TestValidateListenAddr verifies rejection of invalid listen addresses and acceptance of valid ones.
func TestValidateListenAddr(t *testing.T) {
	good := Config{
		ListenAddr:         "127.0.0.1:3457",
		UpstreamBaseURL:    "https://www.codebuff.com",
		AuthTokens:         []string{"tok"},
		RotationInterval:   6 * time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 30 * time.Second,
		RegistryRefresh:    6 * time.Hour,
	}

	for _, addr := range []string{"127.0.0.1:3457", ":3457", "0.0.0.0:8080", "[::1]:3457", "localhost:1", "127.0.0.1:65535"} {
		c := good
		c.ListenAddr = addr
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(ListenAddr=%q) = %v, want nil", addr, err)
		}
	}

	for _, bad := range []string{
		"127.0.0.1:abc",
		"127.0.0.1:99999",
		"127.0.0.1:0",
		"127.0.0.1:-1",
		"127.0.0.1:",
		"3457",
		"",
	} {
		c := good
		c.ListenAddr = bad
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(ListenAddr=%q) succeeded, want error", bad)
		}
	}
}

// TestValidateModeKnobs pins the accepted values for the routing knobs that
// otherwise silently change behavior (a COST_MODE typo routes requests as
// PAID → 402).
func TestValidateModeKnobs(t *testing.T) {
	good := Config{
		ListenAddr:         ":3457",
		UpstreamBaseURL:    "https://www.codebuff.com",
		AuthTokens:         []string{"tok"},
		RotationInterval:   6 * time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 30 * time.Second,
		RegistryRefresh:    6 * time.Hour,
	}
	for _, cost := range []string{"", "free"} {
		for _, mmd := range []int{0, 1} {
			c := good
			c.CostMode, c.MaxMessagesPerDay = cost, mmd
			if err := c.Validate(); err != nil {
				t.Errorf("Validate(COST_MODE=%q MAX=%d) = %v, want nil", cost, mmd, err)
			}
		}
	}
}

// TestHybridMode verifies HYBRID_MODE loads from env and .env, EffectiveMode
// reports hybrid before bridge/pooled, and Validate accepts hybrid with and
// without AUTH_TOKENS (token-less requests 502 until a token is added, but
// client-token requests relay like bridge).
func TestHybridMode(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HybridMode {
		t.Error("HybridMode = true by default, want false")
	}
	if got := cfg.EffectiveMode(); got != "pooled" {
		t.Errorf("EffectiveMode = %q, want pooled", got)
	}

	// HYBRID_MODE=true via env.
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("HYBRID_MODE", "true")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load(HYBRID_MODE=true): %v", err)
	}
	if !cfg.HybridMode {
		t.Error("HybridMode = false, want true (from env)")
	}
	if got := cfg.EffectiveMode(); got != "hybrid" {
		t.Errorf("EffectiveMode = %q, want hybrid", got)
	}

	// Hybrid without tokens is legal; EffectiveMode still wins over bridge.
	clearEnv(t)
	t.Setenv("HYBRID_MODE", "true")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load(HYBRID_MODE=true, no tokens): %v", err)
	}
	if !cfg.BridgeMode() {
		t.Error("BridgeMode = false, want true (no AUTH_TOKENS)")
	}
	if got := cfg.EffectiveMode(); got != "hybrid" {
		t.Errorf("EffectiveMode = %q, want hybrid (hybrid beats bridge)", got)
	}

	// Validate accepts hybrid in both token configurations.
	c := Config{
		ListenAddr:         ":3457",
		UpstreamBaseURL:    "https://www.codebuff.com",
		AuthTokens:         []string{"tok"},
		RotationInterval:   6 * time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 30 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		HybridMode:         true,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("hybrid with tokens Validate: %v", err)
	}
	c.AuthTokens = nil
	if err := c.Validate(); err != nil {
		t.Fatalf("hybrid without tokens Validate: %v", err)
	}
}

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

// TestModelsHideUnavailableEnv verifies MODELS_HIDE_UNAVAILABLE loads from
// env and lands in Config.
func TestModelsHideUnavailableEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("MODELS_HIDE_UNAVAILABLE", "true")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ModelsHideUnavailable {
		t.Error("ModelsHideUnavailable = false, want true (from env)")
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

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
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

func TestBadDuration(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")
	t.Setenv("ROTATION_INTERVAL", "soon")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "ROTATION_INTERVAL") {
		t.Fatalf("Load with bad duration: err = %v, want parse error mentioning ROTATION_INTERVAL", err)
	}
}

func TestLogLevel(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	// default: empty when unset
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (default): %v", err)
	} else if cfg.LogLevel != "" {
		t.Errorf("LogLevel = %q, want empty by default", cfg.LogLevel)
	}

	// env source
	t.Setenv("LOG_LEVEL", "debug")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env debug): %v", err)
	} else if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}

	// trace is accepted (case-insensitive), matching telemetry.ParseLevel
	t.Setenv("LOG_LEVEL", "trace")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env trace): %v", err)
	} else if cfg.LogLevel != "trace" {
		t.Errorf("LogLevel = %q, want trace", cfg.LogLevel)
	}

	// invalid level fails validation
	t.Setenv("LOG_LEVEL", "bogus")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("Load (invalid level): err = %v, want error mentioning LOG_LEVEL", err)
	}
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "debug, info, warn, error, trace") {
		t.Fatalf("Load (invalid level): err = %v, want error listing trace", err)
	}

	// .env source
	t.Setenv("LOG_LEVEL", "")
	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok\nLOG_LEVEL=warn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (.env): %v", err)
	} else if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want warn (from .env)", cfg.LogLevel)
	}
}

func TestLogFormat(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	// default: "text" when unset (the historic output shape)
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (default): %v", err)
	} else if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want text by default", cfg.LogFormat)
	}

	// env source
	t.Setenv("LOG_FORMAT", "json")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env json): %v", err)
	} else if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}

	// explicit empty resets to the default
	t.Setenv("LOG_FORMAT", "")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (empty format): %v", err)
	} else if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want text for empty value", cfg.LogFormat)
	}

	// invalid format fails validation
	t.Setenv("LOG_FORMAT", "xml")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "LOG_FORMAT") {
		t.Fatalf("Load (invalid format): err = %v, want error mentioning LOG_FORMAT", err)
	}

	// JSON file source (weakest): env wins over it
	t.Setenv("LOG_FORMAT", "text")
	json := `{"AUTH_TOKENS":["tok"],"LOG_FORMAT":"json"}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (json file): %v", err)
	} else if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want text (env beats JSON file)", cfg.LogFormat)
	}
	t.Setenv("LOG_FORMAT", "")
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (json file, no env): %v", err)
	} else if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json from JSON file", cfg.LogFormat)
	}
}

// TestLogAccess pins T17: LOG_ACCESS defaults to true, an empty .env line
// keeps it enabled (the access gate must never flip off from an unset or
// blank value), and only an explicit false disables the access lines.
func TestLogAccess(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (default): %v", err)
	} else if !cfg.LogAccess {
		t.Error("LogAccess = false by default, want true")
	}

	// env source: explicit false disables.
	t.Setenv("LOG_ACCESS", "false")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env false): %v", err)
	} else if cfg.LogAccess {
		t.Error("LogAccess = true for LOG_ACCESS=false, want false")
	}

	// Explicit true re-enables.
	t.Setenv("LOG_ACCESS", "true")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env true): %v", err)
	} else if !cfg.LogAccess {
		t.Error("LogAccess = false for LOG_ACCESS=true, want true")
	}

	// An empty .env line must not disable access logging: the empty value
	// leaves the default (true) untouched.
	t.Setenv("LOG_ACCESS", "")
	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok\nLOG_ACCESS=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (empty .env line): %v", err)
	} else if !cfg.LogAccess {
		t.Error("LogAccess = false for an empty LOG_ACCESS=.env line, want true")
	}

	// The .env source: LOG_ACCESS=false in .env disables (env wins).
	t.Setenv("LOG_ACCESS", "")
	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok\nLOG_ACCESS=false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (.env false): %v", err)
	} else if cfg.LogAccess {
		t.Error("LogAccess = true for .env LOG_ACCESS=false, want false")
	}
}

func TestTLSFingerprint(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	// default: SAFE_MODE preset (auto) when unset
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (default): %v", err)
	} else if cfg.TLSFingerprint != "auto" {
		t.Errorf("TLSFingerprint = %q, want auto (SAFE_MODE default preset)", cfg.TLSFingerprint)
	}

	// SAFE_MODE=false leaves it empty
	t.Setenv("SAFE_MODE", "false")
	t.Setenv("TLS_FINGERPRINT", "")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (SAFE_MODE=false): %v", err)
	} else if cfg.TLSFingerprint != "" {
		t.Errorf("TLSFingerprint = %q, want empty with SAFE_MODE=false", cfg.TLSFingerprint)
	}
	t.Setenv("SAFE_MODE", "")

	// valid values load OK
	for _, v := range []string{"chrome120", "safari17", "firefox120", "random"} {
		t.Setenv("TLS_FINGERPRINT", v)
		if cfg, err := Load(""); err != nil {
			t.Fatalf("Load (TLSFingerprint=%s): %v", v, err)
		} else if cfg.TLSFingerprint != v {
			t.Errorf("TLSFingerprint = %q, want %s", cfg.TLSFingerprint, v)
		}
	}

	// invalid value fails validation
	t.Setenv("TLS_FINGERPRINT", "bogus")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "TLS_FINGERPRINT") {
		t.Fatalf("Load (invalid TLSFingerprint): err = %v, want error mentioning TLS_FINGERPRINT", err)
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

func TestIdleRotationTimeout(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	// default: 30m via the SAFE_MODE preset (unset)
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load: %v", err)
	} else if cfg.IdleRotationTimeout != 30*time.Minute {
		t.Errorf("IdleRotationTimeout = %v, want 30m (SAFE_MODE default preset)", cfg.IdleRotationTimeout)
	}

	// env override
	t.Setenv("IDLE_ROTATION_TIMEOUT", "90m")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env): %v", err)
	} else if cfg.IdleRotationTimeout != 90*time.Minute {
		t.Errorf("IdleRotationTimeout = %v, want 90m (env)", cfg.IdleRotationTimeout)
	}

	// explicit "0" disables
	t.Setenv("IDLE_ROTATION_TIMEOUT", "0")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env 0): %v", err)
	} else if cfg.IdleRotationTimeout != 0 {
		t.Errorf("IdleRotationTimeout = %v, want 0 (explicit 0)", cfg.IdleRotationTimeout)
	}

	// empty string in JSON is tolerated as disabled (under SAFE_MODE=false;
	// with the default SAFE_MODE=true an empty value is "unset" → preset)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"IDLE_ROTATION_TIMEOUT": ""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IDLE_ROTATION_TIMEOUT", "")
	t.Setenv("SAFE_MODE", "false")
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (empty file): %v", err)
	} else if cfg.IdleRotationTimeout != 0 {
		t.Errorf("IdleRotationTimeout = %v, want 0 (empty tolerated)", cfg.IdleRotationTimeout)
	}
	t.Setenv("SAFE_MODE", "")

	// JSON file value
	if err := os.WriteFile(path, []byte(`{"IDLE_ROTATION_TIMEOUT": "2h"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (file): %v", err)
	} else if cfg.IdleRotationTimeout != 2*time.Hour {
		t.Errorf("IdleRotationTimeout = %v, want 2h (file)", cfg.IdleRotationTimeout)
	}

	// invalid value fails
	t.Setenv("IDLE_ROTATION_TIMEOUT", "soon")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "IDLE_ROTATION_TIMEOUT") {
		t.Fatalf("Load (bad): err = %v, want parse error mentioning IDLE_ROTATION_TIMEOUT", err)
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

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
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

// TestRequestJitterNegative pins the REQUEST_JITTER validation: a negative
// duration must fail Load (only TRANSIENT_RETRIES negativity was tested).
func TestRequestJitterNegative(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("REQUEST_JITTER", "-1s")

	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "REQUEST_JITTER") {
		t.Fatalf("Load (REQUEST_JITTER=-1s): err = %v, want validation error mentioning REQUEST_JITTER", err)
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

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
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

// TestDedupeAPIKeys asserts the dedupeStrings pass for API_KEYS (only
// AUTH_TOKENS dedupe was previously asserted) when the same value appears
// multiple times in one env value.
func TestDedupeAPIKeys(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("API_KEYS", "k1,k2,k1, k1")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"k1", "k2"}; !equalStrings(cfg.APIKeys, want) {
		t.Errorf("APIKeys = %v, want %v (deduped)", cfg.APIKeys, want)
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

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseMap(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{
			name: "empty",
			raw:  "",
			want: map[string]string{},
		},
		{
			name: "single pair",
			raw:  "gpt-4o:deepseek/deepseek-v4-flash",
			want: map[string]string{"gpt-4o": "deepseek/deepseek-v4-flash"},
		},
		{
			name: "multiple pairs with spaces and newlines",
			raw:  " gpt-4o : deepseek/deepseek-v4-flash , \n glm: z-ai/glm-5.2 \n",
			want: map[string]string{
				"gpt-4o": "deepseek/deepseek-v4-flash",
				"glm":    "z-ai/glm-5.2",
			},
		},
		{
			name: "malformed pair skipped",
			raw:  "valid:model,novalue,also:ok",
			want: map[string]string{"valid": "model", "also": "ok"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMap(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("parseMap(%q) len = %d, want %d", tc.raw, len(got), len(tc.want))
			}
			for k, wantVal := range tc.want {
				if got[k] != wantVal {
					t.Errorf("got[%q] = %q, want %q", k, got[k], wantVal)
				}
			}
		})
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

// TestDotenvFullKeySet verifies every env-overridable key also lands in cfg
// when set in ./.env. Regression: SAFE_MODE, REQUEST_JITTER, CLI_VERSION,
// MODEL_ALIASES and TRANSIENT_RETRIES were silently ignored in .env.
func TestDotenvFullKeySet(t *testing.T) {
	clearEnv(t)

	content := strings.Join([]string{
		"SAFE_MODE=false",
		"HYBRID_MODE=true",
		"REQUEST_JITTER=5s",
		"CLI_VERSION=9.9.9",
		"MODEL_ALIASES=gpt-4o:deepseek/deepseek-v4-flash,glm:z-ai/glm-5.2",
		"MODELS_ALLOW=deepseek/deepseek-v4-flash,z-ai/glm-5.2",
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
	if !cfg.HybridMode {
		t.Error("HybridMode = false, want true (from .env)")
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
	if cfg.MaxSpendPerDay != 500 {
		t.Errorf("MaxSpendPerDay = %d, want 500 (from .env)", cfg.MaxSpendPerDay)
	}
}

// TestDotenvFullKeySetEnvWins verifies the real environment still beats the
// .env file for the newly mirrored keys.
func TestDotenvFullKeySetEnvWins(t *testing.T) {
	clearEnv(t)

	if err := os.WriteFile(".env", []byte("SAFE_MODE=false\nHYBRID_MODE=true\nCLI_VERSION=9.9.9\nTRANSIENT_RETRIES=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAFE_MODE", "true")
	t.Setenv("HYBRID_MODE", "false")
	t.Setenv("CLI_VERSION", "1.2.3")
	t.Setenv("TRANSIENT_RETRIES", "5")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.SafeMode {
		t.Error("SafeMode = false, want true (env wins over .env)")
	}
	if cfg.HybridMode {
		t.Error("HybridMode = true, want false (env wins over .env)")
	}
	if cfg.CLIVersion != "1.2.3" {
		t.Errorf("CLIVersion = %q, want 1.2.3 (env wins)", cfg.CLIVersion)
	}
	if cfg.TransientRetries != 5 {
		t.Errorf("TransientRetries = %d, want 5 (env wins)", cfg.TransientRetries)
	}
}

// TestAutoDiscoverStripsCredentialsBOM verifies that a UTF-8 BOM at the
// start of the CLI credentials file (a Windows writer artifact) does not
// break json.Unmarshal and silently disable auto-discovery.
func TestAutoDiscoverStripsCredentialsBOM(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTO_DISCOVER_TOKEN", "")

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	credDir := filepath.Join(home, ".config", "manicode")
	if err := os.MkdirAll(credDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := "\xef\xbb\xbf" + `{"default": {"authToken": "cb_discovered", "email": "dev@example.com"}}`
	if err := os.WriteFile(filepath.Join(credDir, "credentials.json"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "cb_discovered" {
		t.Fatalf("AuthTokens = %v, want [cb_discovered] (BOM must not break credentials parsing)", got)
	}
	if cfg.DiscoveredSource == "" {
		t.Fatal("DiscoveredSource = empty, want the credentials file path")
	}
}

// TestAutoDiscoverWarnsOnBridgeToPooled verifies that auto-discovery filling
// an empty AUTH_TOKENS (which silently flips bridge mode to pooled mode)
// emits a prominent slog warning naming the source file and the off switch.
func TestAutoDiscoverWarnsOnBridgeToPooled(t *testing.T) {
	clearEnv(t)
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

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "cb_discovered" {
		t.Fatalf("AuthTokens = %v, want [cb_discovered] (auto-discovered)", got)
	}
	if cfg.DiscoveredSource == "" {
		t.Fatal("DiscoveredSource = empty, want the credentials file path")
	}
	out := buf.String()
	if !strings.Contains(out, "AUTO_DISCOVER_TOKEN=false") {
		t.Errorf("warning missing disable hint (AUTO_DISCOVER_TOKEN=false), got: %q", out)
	}
	if !strings.Contains(out, "manicode") {
		t.Errorf("warning missing source file name, got: %q", out)
	}
}

// TestAutoDiscoverSkippedWhenTokensExplicitlyCleared verifies that an
// explicitly-empty AUTH_TOKENS (the shape the dashboard mode switch persists
// as "AUTH_TOKENS=" in .env) suppresses CLI auto-discovery — the operator
// chose bridge mode, so a local CLI login must not silently refill the pool.
func TestAutoDiscoverSkippedWhenTokensExplicitlyCleared(t *testing.T) {
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

	// The dashboard mode switch writes exactly "AUTH_TOKENS=" into .env.
	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.AuthTokens) != 0 {
		t.Errorf("AuthTokens = %v, want empty (explicit bridge mode, not refilled by discovery)", cfg.AuthTokens)
	}
	if !cfg.BridgeMode() {
		t.Error("BridgeMode() = false, want true with explicitly-cleared AUTH_TOKENS")
	}
	if cfg.DiscoveredSource != "" {
		t.Errorf("DiscoveredSource = %q, want empty (auto-discovery must be suppressed)", cfg.DiscoveredSource)
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

// ── Wave 1 issue tests (#79: ACTING_USER_ID) ─────────────────────────────

// TestActingUserID verifies ACTING_USER_ID resolves from the environment,
// the .env file, and the JSON config (optional key; empty default), issue
// #79, and that the pre-rename USER_ID knob still works as a backward-compat
// alias (#126; the new name always wins when both are set).
func TestActingUserID(t *testing.T) {
	t.Run("default empty", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ActingUserID != "" {
			t.Errorf("ActingUserID = %q, want empty default", cfg.ActingUserID)
		}
	})
	t.Run("env override", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("ACTING_USER_ID", "user-abc")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ActingUserID != "user-abc" {
			t.Errorf("ActingUserID = %q, want user-abc", cfg.ActingUserID)
		}
	})
	t.Run("env legacy alias", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("USER_ID", "user-legacy")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ActingUserID != "user-legacy" {
			t.Errorf("ActingUserID = %q, want user-legacy (USER_ID alias)", cfg.ActingUserID)
		}
	})
	t.Run("new name wins over legacy", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("ACTING_USER_ID", "user-new")
		t.Setenv("USER_ID", "user-legacy")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ActingUserID != "user-new" {
			t.Errorf("ActingUserID = %q, want user-new (ACTING_USER_ID wins)", cfg.ActingUserID)
		}
	})
	t.Run("dotenv", func(t *testing.T) {
		clearEnv(t)
		if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-1\nACTING_USER_ID=user-dotenv\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ActingUserID != "user-dotenv" {
			t.Errorf("ActingUserID = %q, want user-dotenv (from .env)", cfg.ActingUserID)
		}
	})
	t.Run("dotenv legacy alias", func(t *testing.T) {
		clearEnv(t)
		if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-1\nUSER_ID=user-dotenv-legacy\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ActingUserID != "user-dotenv-legacy" {
			t.Errorf("ActingUserID = %q, want user-dotenv-legacy (USER_ID alias in .env)", cfg.ActingUserID)
		}
	})
	t.Run("JSON config", func(t *testing.T) {
		clearEnv(t)
		if err := os.WriteFile("cfg.json", []byte(`{"AUTH_TOKENS":["tok-1"],"ACTING_USER_ID":"user-json"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load("cfg.json")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ActingUserID != "user-json" {
			t.Errorf("ActingUserID = %q, want user-json (from JSON config)", cfg.ActingUserID)
		}
	})
	t.Run("JSON legacy key", func(t *testing.T) {
		clearEnv(t)
		if err := os.WriteFile("cfg.json", []byte(`{"AUTH_TOKENS":["tok-1"],"USER_ID":"user-json-legacy"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load("cfg.json")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ActingUserID != "user-json-legacy" {
			t.Errorf("ActingUserID = %q, want user-json-legacy (legacy USER_ID JSON key)", cfg.ActingUserID)
		}
	})
}

func TestPreferMaxModels(t *testing.T) {
	t.Run("default false", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PreferMaxModels {
			t.Error("PreferMaxModels default = true, want false")
		}
	})
	t.Run("env true variants", func(t *testing.T) {
		for _, val := range []string{"true", "1", "yes", "on", "TRUE"} {
			clearEnv(t)
			t.Setenv("AUTH_TOKENS", "tok-1")
			t.Setenv("PREFER_MAX_MODELS", val)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load with PREFER_MAX_MODELS=%q: %v", val, err)
			}
			if !cfg.PreferMaxModels {
				t.Errorf("PreferMaxModels with env %q = false, want true", val)
			}
		}
	})
	t.Run("env false variants", func(t *testing.T) {
		for _, val := range []string{"false", "0", "no", "off", "FALSE"} {
			clearEnv(t)
			t.Setenv("AUTH_TOKENS", "tok-1")
			t.Setenv("PREFER_MAX_MODELS", val)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load with PREFER_MAX_MODELS=%q: %v", val, err)
			}
			if cfg.PreferMaxModels {
				t.Errorf("PreferMaxModels with env %q = true, want false", val)
			}
		}
	})
	t.Run("dotenv override", func(t *testing.T) {
		clearEnv(t)
		if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-1\nPREFER_MAX_MODELS=true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.PreferMaxModels {
			t.Error("PreferMaxModels from .env = false, want true")
		}
	})
	t.Run("JSON config", func(t *testing.T) {
		clearEnv(t)
		if err := os.WriteFile("cfg.json", []byte(`{"AUTH_TOKENS":["tok-1"],"PREFER_MAX_MODELS":true}`), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load("cfg.json")
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.PreferMaxModels {
			t.Error("PreferMaxModels from JSON = false, want true")
		}
	})
}

// TestAccessTier verifies ACCESS_TIER resolves from the environment, the
// .env file, and the JSON config (optional key; empty default = unknown =
// full). AccessTierExplicit records that a configured source set the value,
// so runtime session-probe observations never override the operator choice.
func TestAccessTier(t *testing.T) {
	t.Run("default empty", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AccessTier != "" {
			t.Errorf("AccessTier default = %q, want empty (unknown = full)", cfg.AccessTier)
		}
		if cfg.AccessTierExplicit {
			t.Error("AccessTierExplicit default = true, want false")
		}
	})
	t.Run("env", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("ACCESS_TIER", "limited")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AccessTier != "limited" {
			t.Errorf("AccessTier = %q, want limited (env)", cfg.AccessTier)
		}
		if !cfg.AccessTierExplicit {
			t.Error("AccessTierExplicit = false, want true (env set the value)")
		}
	})
	t.Run("env whitespace trimmed", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("ACCESS_TIER", "  limited  ")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AccessTier != "limited" {
			t.Errorf("AccessTier = %q, want limited (trimmed)", cfg.AccessTier)
		}
	})
	t.Run("dotenv", func(t *testing.T) {
		clearEnv(t)
		if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-1\nACCESS_TIER=limited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AccessTier != "limited" {
			t.Errorf("AccessTier from .env = %q, want limited", cfg.AccessTier)
		}
	})
	t.Run("JSON config", func(t *testing.T) {
		clearEnv(t)
		if err := os.WriteFile("cfg.json", []byte(`{"AUTH_TOKENS":["tok-1"],"ACCESS_TIER":"limited"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load("cfg.json")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AccessTier != "limited" {
			t.Errorf("AccessTier from JSON = %q, want limited", cfg.AccessTier)
		}
		if !cfg.AccessTierExplicit {
			t.Error("AccessTierExplicit = false with JSON ACCESS_TIER, want true")
		}
	})
	t.Run("env beats dotenv", func(t *testing.T) {
		clearEnv(t)
		if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-1\nACCESS_TIER=limited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ACCESS_TIER", "full")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AccessTier != "full" {
			t.Errorf("AccessTier = %q, want full (env beats .env)", cfg.AccessTier)
		}
	})
}
