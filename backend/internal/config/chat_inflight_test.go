package config

import (
	"os"
	"testing"
)

// Chat-burst queue knobs: per-token concurrent in-flight chat caps split by
// cost class (CHAT_MAX_INFLIGHT_METERED default 1, CHAT_MAX_INFLIGHT_UNMETERED
// default 3, 0 = unlimited) across every tier (env, .env, DB overlay) plus
// shape validation (non-negative). Metered vs unmetered reuses the Freebucks
// prices lookup (nil price = unmetered); no new classification lives here.
func TestChatInflightDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChatMaxInflightMetered != 1 {
		t.Errorf("ChatMaxInflightMetered = %d, want 1 (default)", cfg.ChatMaxInflightMetered)
	}
	if cfg.ChatMaxInflightUnmetered != 3 {
		t.Errorf("ChatMaxInflightUnmetered = %d, want 3 (default)", cfg.ChatMaxInflightUnmetered)
	}
	if !cfg.BurstBalanceEnabled {
		t.Error("BurstBalanceEnabled = false, want true (default on)")
	}
}

func TestChatInflightEnvOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("CHAT_MAX_INFLIGHT_METERED", "2")
	t.Setenv("CHAT_MAX_INFLIGHT_UNMETERED", "5")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChatMaxInflightMetered != 2 {
		t.Errorf("ChatMaxInflightMetered = %d, want 2 (env wins)", cfg.ChatMaxInflightMetered)
	}
	if cfg.ChatMaxInflightUnmetered != 5 {
		t.Errorf("ChatMaxInflightUnmetered = %d, want 5 (env wins)", cfg.ChatMaxInflightUnmetered)
	}
}

func TestChatInflightDotenv(t *testing.T) {
	clearEnv(t)
	if err := os.WriteFile(".env", []byte("CHAT_MAX_INFLIGHT_METERED=4\nCHAT_MAX_INFLIGHT_UNMETERED=6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChatMaxInflightMetered != 4 {
		t.Errorf("ChatMaxInflightMetered = %d, want 4 (from .env)", cfg.ChatMaxInflightMetered)
	}
	if cfg.ChatMaxInflightUnmetered != 6 {
		t.Errorf("ChatMaxInflightUnmetered = %d, want 6 (from .env)", cfg.ChatMaxInflightUnmetered)
	}
}

func TestChatInflightOverlay(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadOpts("", LoadOptions{Overlay: map[string]string{
		"CHAT_MAX_INFLIGHT_METERED":   "7",
		"CHAT_MAX_INFLIGHT_UNMETERED": "0",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChatMaxInflightMetered != 7 {
		t.Errorf("ChatMaxInflightMetered = %d, want 7 (overlay wins)", cfg.ChatMaxInflightMetered)
	}
	if cfg.ChatMaxInflightUnmetered != 0 {
		t.Errorf("ChatMaxInflightUnmetered = %d, want 0 (overlay wins, unlimited)", cfg.ChatMaxInflightUnmetered)
	}
}

func TestChatInflightValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv("CHAT_MAX_INFLIGHT_METERED", "-1")
	if _, err := Load(""); err == nil {
		t.Error("Load with CHAT_MAX_INFLIGHT_METERED=-1 accepted, want a non-negative rejection")
	}

	clearEnv(t)
	t.Setenv("CHAT_MAX_INFLIGHT_UNMETERED", "-2")
	if _, err := Load(""); err == nil {
		t.Error("Load with CHAT_MAX_INFLIGHT_UNMETERED=-2 accepted, want a non-negative rejection")
	}

	clearEnv(t)
	t.Setenv("CHAT_MAX_INFLIGHT_METERED", "many")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with CHAT_MAX_INFLIGHT_METERED=many failed: %v (unparseable ints keep the default)", err)
	}
	if cfg.ChatMaxInflightMetered != 1 {
		t.Errorf("ChatMaxInflightMetered = %d with CHAT_MAX_INFLIGHT_METERED=many, want 1 (default kept)", cfg.ChatMaxInflightMetered)
	}
}

func TestChatInflightValidateSettingValue(t *testing.T) {
	for key, value := range map[string]string{
		"CHAT_MAX_INFLIGHT_METERED":   "1",
		"CHAT_MAX_INFLIGHT_UNMETERED": "3",
	} {
		if err := ValidateSettingValue(key, value); err != nil {
			t.Errorf("ValidateSettingValue(%s,%s) = %v, want nil", key, value, err)
		}
	}
	for key, value := range map[string]string{
		"CHAT_MAX_INFLIGHT_METERED":   "many",
		"CHAT_MAX_INFLIGHT_UNMETERED": "lots",
	} {
		if err := ValidateSettingValue(key, value); err == nil {
			t.Errorf("ValidateSettingValue(%q,%q) accepted, want an error", key, value)
		}
	}
}
