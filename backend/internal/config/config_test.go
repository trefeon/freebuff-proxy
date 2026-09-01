package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/clicreds"
)

// envKeys lists every environment variable the package reads. Tests clear
// them all first so machine-level env can never leak into assertions.
// Sourced from ConfigEnvKeys() (the catalog plus the legacy USER_ID alias)
// so it cannot drift from the loader (issue #281).
var envKeys = ConfigEnvKeys()

// TestMain strips ambient freebuff-proxy config env vars for the whole test
// binary (testutil.UnsetConfigEnvForTestMain). clearEnv in each test covers
// the per-test isolation, but a developer's exported SESSION_PERSIST /
// MODELS_HIDE_UNAVAILABLE / SESSION_STATE_FILE would
// otherwise leak into package-level behavior before the first clearEnv runs
// (e.g. TestDefaults / TestSessionPersist assert on those defaults).
func TestMain(m *testing.M) {
	unsetConfigEnvForTestMain()
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

	cfg, err := LoadOpts("", LoadOptions{DiscoverCLIToken: clicreds.DiscoverToken})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
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

	cfg, err := LoadOpts("", LoadOptions{DiscoverCLIToken: clicreds.DiscoverToken})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
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

func TestQuotaFallbackModels(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.QuotaFallbackModels["deepseek/deepseek-v4-flash"]; got != "mimo/mimo-v2.5" {
			t.Errorf("QuotaFallbackModels[flash] = %q, want mimo/mimo-v2.5", got)
		}
		if got := cfg.QuotaFallbackModels["z-ai/glm-5.2"]; got != "deepseek/deepseek-v4-flash" {
			t.Errorf("QuotaFallbackModels[glm-5.2] = %q, want deepseek/deepseek-v4-flash", got)
		}
		if got := cfg.QuotaFallbackModels["openai/gpt-5.6-luna"]; got != "deepseek/deepseek-v4-flash" {
			t.Errorf("QuotaFallbackModels[luna] = %q, want deepseek/deepseek-v4-flash", got)
		}
	})
	t.Run("env override", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("QUOTA_FALLBACK_MODELS", "model-a=model-b,model-c=model-d")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.QuotaFallbackModels["model-a"] != "model-b" || cfg.QuotaFallbackModels["model-c"] != "model-d" {
			t.Errorf("QuotaFallbackModels = %v, want mapped pairs", cfg.QuotaFallbackModels)
		}
	})
	t.Run("validation identical src and target", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("QUOTA_FALLBACK_MODELS", "model-a=model-a")
		_, err := Load("")
		if err == nil {
			t.Error("expected error for identical source and target in QUOTA_FALLBACK_MODELS, got nil")
		}
	})
}
