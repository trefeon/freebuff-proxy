package config

import (
	"os"
	"testing"
	"time"
)

// Burst-balance knobs (ADR-0023): default-off opt-in GroupPool quartet
// across every tier (env, .env, DB overlay) plus shape validation
// (BURST_MAX_TOKENS >= 2, parseable BURST_WINDOW, non-negative threshold).
func TestBurstBalanceDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BurstBalanceEnabled {
		t.Error("BurstBalanceEnabled = true, want false (default off)")
	}
	if cfg.BurstWindow != time.Minute {
		t.Errorf("BurstWindow = %v, want 1m (default)", cfg.BurstWindow)
	}
	if cfg.BurstThreshold != 20 {
		t.Errorf("BurstThreshold = %d, want 20 (default)", cfg.BurstThreshold)
	}
	if cfg.BurstMaxTokens != 2 {
		t.Errorf("BurstMaxTokens = %d, want 2 (default)", cfg.BurstMaxTokens)
	}
}

func TestBurstBalanceEnvOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("BURST_BALANCE_ENABLED", "1")
	t.Setenv("BURST_WINDOW", "30s")
	t.Setenv("BURST_THRESHOLD", "5")
	t.Setenv("BURST_MAX_TOKENS", "3")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.BurstBalanceEnabled {
		t.Error("BurstBalanceEnabled = false with BURST_BALANCE_ENABLED=1, want true")
	}
	if cfg.BurstWindow != 30*time.Second {
		t.Errorf("BurstWindow = %v, want 30s (env wins)", cfg.BurstWindow)
	}
	if cfg.BurstThreshold != 5 {
		t.Errorf("BurstThreshold = %d, want 5 (env wins)", cfg.BurstThreshold)
	}
	if cfg.BurstMaxTokens != 3 {
		t.Errorf("BurstMaxTokens = %d, want 3 (env wins)", cfg.BurstMaxTokens)
	}
}

func TestBurstBalanceDotenv(t *testing.T) {
	clearEnv(t)
	if err := os.WriteFile(".env", []byte("BURST_BALANCE_ENABLED=true\nBURST_WINDOW=2m\nBURST_THRESHOLD=7\nBURST_MAX_TOKENS=4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.BurstBalanceEnabled {
		t.Error("BurstBalanceEnabled = false with .env BURST_BALANCE_ENABLED=true, want true")
	}
	if cfg.BurstWindow != 2*time.Minute {
		t.Errorf("BurstWindow = %v, want 2m (from .env)", cfg.BurstWindow)
	}
	if cfg.BurstThreshold != 7 {
		t.Errorf("BurstThreshold = %d, want 7 (from .env)", cfg.BurstThreshold)
	}
	if cfg.BurstMaxTokens != 4 {
		t.Errorf("BurstMaxTokens = %d, want 4 (from .env)", cfg.BurstMaxTokens)
	}
}

func TestBurstBalanceOverlay(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadOpts("", LoadOptions{Overlay: map[string]string{
		"BURST_BALANCE_ENABLED": "true",
		"BURST_WINDOW":          "90s",
		"BURST_THRESHOLD":       "11",
		"BURST_MAX_TOKENS":      "3",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.BurstBalanceEnabled {
		t.Error("BurstBalanceEnabled = false with overlay true, want true")
	}
	if cfg.BurstWindow != 90*time.Second {
		t.Errorf("BurstWindow = %v, want 90s (overlay wins)", cfg.BurstWindow)
	}
	if cfg.BurstThreshold != 11 {
		t.Errorf("BurstThreshold = %d, want 11 (overlay wins)", cfg.BurstThreshold)
	}
	if cfg.BurstMaxTokens != 3 {
		t.Errorf("BurstMaxTokens = %d, want 3 (overlay wins)", cfg.BurstMaxTokens)
	}
}

func TestBurstBalanceValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv("BURST_MAX_TOKENS", "1")
	if _, err := Load(""); err == nil {
		t.Error("Load with BURST_MAX_TOKENS=1 accepted, want a minimum-2 rejection")
	}

	clearEnv(t)
	t.Setenv("BURST_WINDOW", "not-a-duration")
	if _, err := Load(""); err == nil {
		t.Error("Load with BURST_WINDOW=not-a-duration accepted, want a parse rejection")
	}

	clearEnv(t)
	t.Setenv("BURST_THRESHOLD", "-1")
	if _, err := Load(""); err == nil {
		t.Error("Load with BURST_THRESHOLD=-1 accepted, want a non-negative rejection")
	}
}

func TestBurstBalanceValidateSettingValue(t *testing.T) {
	for key, value := range map[string]string{
		"BURST_BALANCE_ENABLED": "true",
		"BURST_WINDOW":          "1m",
		"BURST_THRESHOLD":       "20",
		"BURST_MAX_TOKENS":      "2",
	} {
		if err := ValidateSettingValue(key, value); err != nil {
			t.Errorf("ValidateSettingValue(%s,%s) = %v, want nil", key, value, err)
		}
	}
	for key, value := range map[string]string{
		"BURST_BALANCE_ENABLED": "maybe",
		"BURST_WINDOW":          "soon",
		"BURST_THRESHOLD":       "many",
		"BURST_MAX_TOKENS":      "1",
	} {
		if err := ValidateSettingValue(key, value); err == nil {
			t.Errorf("ValidateSettingValue(%q,%q) accepted, want an error", key, value)
		}
	}
}
