package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --- #42: default model aliases ---------------------------------------------

func TestDefaultModelAliases(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"deepseek-chat":     "deepseek/deepseek-v4-flash",
		"gpt-4o":            "deepseek/deepseek-v4-pro",
		"claude-3-5-sonnet": "anthropic/claude-fable-5",
	}
	for alias, real := range want {
		if got := cfg.ModelAliases[alias]; got != real {
			t.Errorf("ModelAliases[%q] = %q, want default %q", alias, got, real)
		}
	}
}

func TestModelAliasesExplicitSuppressesDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("MODEL_ALIASES", "gpt-4o:minimax/minimax-m3")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ModelAliases["gpt-4o"]; got != "minimax/minimax-m3" {
		t.Errorf("ModelAliases[gpt-4o] = %q, want explicit override", got)
	}
	// The default aliases must NOT leak in when the operator set any alias.
	if _, ok := cfg.ModelAliases["deepseek-chat"]; ok {
		t.Error("default alias deepseek-chat applied despite explicit MODEL_ALIASES")
	}
	if _, ok := cfg.ModelAliases["claude-3-5-sonnet"]; ok {
		t.Error("default alias claude-3-5-sonnet applied despite explicit MODEL_ALIASES")
	}
}

// --- #100: FALLBACK_AFTER_MS + FALLBACK_MODEL defaults -----------------------

func TestFallbackAfterDefault(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FallbackAfter != 10*time.Second {
		t.Errorf("FallbackAfter = %v, want 10s default", cfg.FallbackAfter)
	}
}

func TestFallbackAfterEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("FALLBACK_AFTER_MS", "2500")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FallbackAfter != 2500*time.Millisecond {
		t.Errorf("FallbackAfter = %v, want 2.5s", cfg.FallbackAfter)
	}
}

func TestFallbackAfterZeroDisables(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("FALLBACK_AFTER_MS", "0")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FallbackAfter != 0 {
		t.Errorf("FallbackAfter = %v, want 0 (disabled)", cfg.FallbackAfter)
	}
}

func TestFallbackAfterInvalidFails(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("FALLBACK_AFTER_MS", "soon")

	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "FALLBACK_AFTER_MS") {
		t.Fatalf("Load err = %v, want FALLBACK_AFTER_MS parse error", err)
	}
}

func TestDefaultFallbackModels(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for model, fb := range defaultFallbackModels() {
		if got := cfg.FallbackModels[model]; got != fb {
			t.Errorf("FallbackModels[%q] = %q, want default %q", model, got, fb)
		}
	}
	// Every default fallback target must exist in the free catalog
	// (deepseek-v4-flash is the guaranteed-available row).
	if cfg.FallbackModels["deepseek/deepseek-v4-pro"] != "deepseek/deepseek-v4-flash" {
		t.Error("deepseek-v4-pro must fall back to deepseek-v4-flash")
	}
	if cfg.FallbackModels["openai/gpt-5.6-luna"] != "deepseek/deepseek-v4-flash" {
		t.Error("openai/gpt-5.6-luna must fall back to deepseek-v4-flash")
	}
}

func TestFallbackModelsExplicitSuppressesDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("FALLBACK_MODEL", "openai/gpt-5.6-luna=deepseek/deepseek-v4-flash")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.FallbackModels["openai/gpt-5.6-luna"]; got != "deepseek/deepseek-v4-flash" {
		t.Errorf("FallbackModels[gpt-5.6-luna] = %q, want explicit", got)
	}
	if _, ok := cfg.FallbackModels["deepseek/deepseek-v4-pro"]; ok {
		t.Error("default fallback for deepseek-v4-pro applied despite explicit FALLBACK_MODEL")
	}
}

// --- #48: WEBHOOK_URL -------------------------------------------------------

func TestWebhookURLParsed(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("WEBHOOK_URL", "https://example.com/hook")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebhookURL != "https://example.com/hook" {
		t.Errorf("WebhookURL = %q, want https://example.com/hook", cfg.WebhookURL)
	}
}

func TestWebhookURLInvalidFails(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("WEBHOOK_URL", "not-a-url")

	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "WEBHOOK_URL") {
		t.Fatalf("Load err = %v, want WEBHOOK_URL validation error", err)
	}
}

// --- #97: ADOPT_CLI_SESSION -------------------------------------------------

func TestAdoptCLISessionParsed(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("ADOPT_CLI_SESSION", "true")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AdoptCLISession {
		t.Error("AdoptCLISession = false, want true")
	}
	if cfg.WaitingRoomChain {
		t.Error("WaitingRoomChain = true, want false default")
	}
}

// --- #39: XDG / AppData config search ---------------------------------------

func TestEnvFileCandidatesOrderAndShape(t *testing.T) {
	clearEnv(t)
	cands := EnvFileCandidates()
	if len(cands) != 2 {
		t.Fatalf("EnvFileCandidates = %v, want exactly 2 candidates", cands)
	}
	// Working directory always wins (first candidate).
	if filepath.Clean(cands[0]) != filepath.Join(".", ".env") {
		t.Errorf("candidate[0] = %q, want ./.env first", cands[0])
	}
	// The platform dir must end with freebuff-proxy/.env.
	if filepath.Base(filepath.Dir(cands[1])) != "freebuff-proxy" || filepath.Base(cands[1]) != ".env" {
		t.Errorf("candidate[1] = %q, want <config-dir>/freebuff-proxy/.env", cands[1])
	}
	// Platform-specific base dir.
	switch runtime.GOOS {
	case "windows":
		if !strings.Contains(strings.ToLower(cands[1]), "appdata") && !strings.Contains(strings.ToLower(cands[1]), "roaming") {
			t.Errorf("windows candidate[1] = %q, want %%APPDATA%%\\freebuff-proxy\\.env", cands[1])
		}
	case "darwin":
		if !strings.Contains(cands[1], "Application Support") {
			t.Errorf("darwin candidate[1] = %q, want ~/Library/Application Support/freebuff-proxy/.env", cands[1])
		}
	default:
		if !strings.Contains(cands[1], ".config") {
			t.Errorf("linux candidate[1] = %q, want ~/.config/freebuff-proxy/.env", cands[1])
		}
	}
}

func TestResolveEnvFileCwdWins(t *testing.T) {
	clearEnv(t)
	// cwd (temp dir) has .env; the platform dir also gets one — cwd must win.
	if err := os.WriteFile(".env", []byte("LISTEN_ADDR=:1001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	platform := EnvFileCandidates()[1]
	if err := os.MkdirAll(filepath.Dir(platform), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform, []byte("LISTEN_ADDR=:1002\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(platform)) })

	if got := ResolveEnvFile(); filepath.Clean(got) != filepath.Join(".", ".env") {
		t.Errorf("ResolveEnvFile = %q, want ./.env (cwd wins)", got)
	}
}

func TestResolveEnvFilePlatformFallback(t *testing.T) {
	clearEnv(t)
	// No ./.env in cwd; only the platform config dir has one.
	platform := EnvFileCandidates()[1]
	if err := os.MkdirAll(filepath.Dir(platform), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform, []byte("LISTEN_ADDR=:1003\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(platform)) })

	if got := ResolveEnvFile(); filepath.Clean(got) != filepath.Clean(platform) {
		t.Errorf("ResolveEnvFile = %q, want platform candidate %q", got, platform)
	}
}

func TestResolveEnvFileNone(t *testing.T) {
	clearEnv(t)
	// Neither candidate exists (temp cwd; remove any platform file).
	platform := EnvFileCandidates()[1]
	_ = os.RemoveAll(filepath.Dir(platform))
	if got := ResolveEnvFile(); got != "" {
		t.Errorf("ResolveEnvFile = %q, want empty when no candidate exists", got)
	}
}

// Load must actually READ the platform-dir .env when cwd has none and record
// the path on Config.EnvFile (issue #39).
func TestLoadReadsPlatformEnvFile(t *testing.T) {
	clearEnv(t)
	platform := EnvFileCandidates()[1]
	if err := os.MkdirAll(filepath.Dir(platform), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform, []byte("AUTH_TOKENS=tok-platform\nLISTEN_ADDR=:1004\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(platform)) })

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":1004" {
		t.Errorf("ListenAddr = %q, want :1004 from platform .env", cfg.ListenAddr)
	}
	if filepath.Clean(cfg.EnvFile) != filepath.Clean(platform) {
		t.Errorf("EnvFile = %q, want platform candidate %q", cfg.EnvFile, platform)
	}
}

// With a cwd .env present, Load must use it (not the platform file) and
// record the cwd path.
func TestLoadCwdEnvWinsOverPlatform(t *testing.T) {
	clearEnv(t)
	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-cwd\nLISTEN_ADDR=:1005\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	platform := EnvFileCandidates()[1]
	if err := os.MkdirAll(filepath.Dir(platform), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform, []byte("AUTH_TOKENS=tok-platform\nLISTEN_ADDR=:1006\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(platform)) })

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":1005" {
		t.Errorf("ListenAddr = %q, want :1005 from cwd .env", cfg.ListenAddr)
	}
	if filepath.Clean(cfg.EnvFile) != filepath.Join(".", ".env") {
		t.Errorf("EnvFile = %q, want ./.env", cfg.EnvFile)
	}
}

func TestEnvFileEmptyWithoutFiles(t *testing.T) {
	clearEnv(t)
	_ = os.RemoveAll(filepath.Dir(EnvFileCandidates()[1]))
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnvFile != "" {
		t.Errorf("EnvFile = %q, want empty when no .env exists", cfg.EnvFile)
	}
}
