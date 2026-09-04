package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

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
	if cfg.CORSAllowedOrigin != "*" {
		t.Errorf("CORSAllowedOrigin = %q, want %q (default)", cfg.CORSAllowedOrigin, "*")
	}
	if got := cfg.EffectiveMode(); got != "hybrid" {
		t.Errorf("EffectiveMode = %q, want hybrid (default when AUTH_TOKENS set)", got)
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
	if !cfg.SessionPersist {
		t.Error("SessionPersist = false, want true (default: persistence enabled)")
	}
	if cfg.SessionStateFile != ".freebuff-session-state.json" {
		t.Errorf("SessionStateFile = %q, want %q", cfg.SessionStateFile, ".freebuff-session-state.json")
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
		if cfg.RequestJitter != 200*time.Millisecond {
			t.Errorf("RequestJitter = %v, want 200ms under SafeMode", cfg.RequestJitter)
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
		if cfg.RequestJitter != 200*time.Millisecond {
			t.Errorf("RequestJitter = %v, want 200ms under SafeMode", cfg.RequestJitter)
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
		if cfg.TLSFingerprint != "" {
			t.Errorf("TLSFingerprint = %q, want empty (CLI-faithful, no browser JA3) under SafeMode", cfg.TLSFingerprint)
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
		{"negative max requests per day", func(c *Config) { c.MaxRequestsPerDay = -1 }},
		{"negative max requests per minute", func(c *Config) { c.MaxRequestsPerMinute = -1 }},
		{"negative max spend", func(c *Config) { c.MaxSpendPerDay = -1 }},
		{"negative rate limit per ip", func(c *Config) { c.RateLimitPerIP = -1 }},
		{"negative rate limit burst", func(c *Config) { c.RateLimitBurst = -1 }},
		{"session persist with empty state file", func(c *Config) { c.SessionPersist = true; c.SessionStateFile = "" }},
		{"invalid listen port non-int", func(c *Config) { c.ListenAddr = "127.0.0.1:abc" }},
		{"invalid listen port overflow", func(c *Config) { c.ListenAddr = "127.0.0.1:99999" }},
		{"invalid listen port zero", func(c *Config) { c.ListenAddr = "127.0.0.1:0" }},
		{"quota fallback self-loop", func(c *Config) { c.QuotaFallbackModels = map[string]string{"a": "a"} }},
		{"quota fallback multi-hop cycle", func(c *Config) { c.QuotaFallbackModels = map[string]string{"a": "b", "b": "a"} }},
		{"quota fallback three-hop cycle", func(c *Config) { c.QuotaFallbackModels = map[string]string{"a": "b", "b": "c", "c": "a"} }},
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

	// default: CLI-faithful (empty, no browser JA3) when unset
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (default): %v", err)
	} else if cfg.TLSFingerprint != "" {
		t.Errorf("TLSFingerprint = %q, want empty (CLI-faithful default, no browser JA3)", cfg.TLSFingerprint)
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

// TestSessionIdleEnd mirrors TestIdleRotationTimeout for the opt-in
// SESSION_IDLE_END knob: unset → disabled, env override parses, explicit
// "0" disables, invalid values fail with the key named.
func TestSessionIdleEnd(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	// Unset: disabled (never preset by SAFE_MODE — ending a session costs
	// a fresh daily-slot admission when traffic resumes).
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	} else if cfg.SessionIdleEnd != 0 {
		t.Errorf("SessionIdleEnd = %v, want 0 (unset)", cfg.SessionIdleEnd)
	}

	t.Setenv("SESSION_IDLE_END", "45m")
	if cfg, err = Load(""); err != nil {
		t.Fatalf("Load (env): %v", err)
	} else if cfg.SessionIdleEnd != 45*time.Minute {
		t.Errorf("SessionIdleEnd = %v, want 45m (env)", cfg.SessionIdleEnd)
	}

	t.Setenv("SESSION_IDLE_END", "0")
	if cfg, err = Load(""); err != nil {
		t.Fatalf("Load (env 0): %v", err)
	} else if cfg.SessionIdleEnd != 0 {
		t.Errorf("SessionIdleEnd = %v, want 0 (explicit 0)", cfg.SessionIdleEnd)
	}

	t.Setenv("SESSION_IDLE_END", "soon")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "SESSION_IDLE_END") {
		t.Fatalf("Load (bad): err = %v, want parse error mentioning SESSION_IDLE_END", err)
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

// TestValidateWebhookURL pins the WEBHOOK_URL host policy: loopback,
// private (RFC1918 / IPv6 ULA), link-local (incl. the cloud-metadata
// 169.254.169.254), multicast, unspecified, and the reserved "localhost"
// name are rejected; public IP literals and (unresolved) hostnames pass.
func TestValidateWebhookURL(t *testing.T) {
	base := Config{
		ListenAddr:         ":3457",
		UpstreamBaseURL:    "https://www.codebuff.com",
		AuthTokens:         []string{"tok"},
		RotationInterval:   6 * time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 30 * time.Second,
		RegistryRefresh:    6 * time.Hour,
	}

	rejected := []string{
		"http://127.0.0.1/hook",       // loopback
		"http://10.1.2.3/hook",        // RFC1918 10/8
		"http://172.16.0.1/hook",      // RFC1918 172.16/12
		"http://172.31.255.1/hook",    // RFC1918 172.16/12
		"http://192.168.1.1/hook",     // RFC1918 192.168/16
		"http://169.254.169.254/hook", // link-local cloud metadata
		"http://169.254.0.1/hook",     // link-local
		"http://[::1]/hook",           // IPv6 loopback
		"http://[fc00::1]/hook",       // IPv6 ULA
		"http://[fe80::1]/hook",       // IPv6 link-local
		"http://localhost/hook",       // reserved loopback name
	}
	for _, u := range rejected {
		c := base
		c.WebhookURL = u
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(WebhookURL=%q) succeeded, want rejection", u)
		}
	}

	accepted := []string{
		"http://93.184.216.34/hook",    // public IPv4 literal
		"http://8.8.8.8/hook",          // public IPv4 literal
		"https://example.com/hook",     // public hostname (not resolved here)
		"https://www.codebuff.com/api", // public hostname
	}
	for _, u := range accepted {
		c := base
		c.WebhookURL = u
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(WebhookURL=%q) = %v, want nil", u, err)
		}
	}
}
