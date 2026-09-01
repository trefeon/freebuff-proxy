package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnvExampleLoadsCleanly proves the shipped .env.example is a valid,
// loadable configuration (a fresh user copying it to .env starts without
// errors) and that every documented safety default lands as expected.
func TestEnvExampleLoadsCleanly(t *testing.T) {
	// Read the fixture from the repo before changing the working directory.
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	// Isolate from the developer's real environment: an ambient AUTH_TOKENS
	// (or LISTEN_ADDR etc.) outranks .env values and would break the
	// BridgeMode()/ListenAddr assertions below. t.Chdir also keeps the .env
	// lookup inside the test's temp dir.
	unsetConfigEnv(t)
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(.env.example) failed: %v", err)
	}
	if !cfg.SafeMode {
		t.Error("SafeMode = false, want true (anti-ban default)")
	}
	if cfg.CostMode != "free" {
		t.Errorf("CostMode = %q, want free (402 avoidance)", cfg.CostMode)
	}
	if cfg.ListenAddr != "127.0.0.1:3457" {
		t.Errorf("ListenAddr = %q, want loopback default", cfg.ListenAddr)
	}
	if cfg.TLSFingerprint != "auto" {
		t.Errorf("TLSFingerprint = %q, want auto", cfg.TLSFingerprint)
	}
	if cfg.TransientRetries != 1 {
		t.Errorf("TransientRetries = %d, want 1", cfg.TransientRetries)
	}
	if !cfg.BridgeMode() {
		t.Error("BridgeMode() = false, want true (empty AUTH_TOKENS)")
	}
	// Issue #238: .env.example documents SESSION_PERSIST as on-by-default
	// (the key is commented out, so the built-in default applies). A fresh
	// install copying the example verbatim must persist, not silently write
	// state files while being told it is opt-in.
	if !cfg.SessionPersist {
		t.Error("SessionPersist = false, want true (.env.example default)")
	}
	if cfg.SessionStateFile != ".freebuff-session-state.json" {
		t.Errorf("SessionStateFile = %q, want .freebuff-session-state.json", cfg.SessionStateFile)
	}
}
