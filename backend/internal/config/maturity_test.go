package config

import (
	"testing"
)

// Maturity defaults: automation on (probes in dry-run mode), dry-run on,
// empty touch-model (= auto: cheapest served unmetered row), 7-day target.
func TestMaturityDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MaturityEnabled {
		t.Error("MaturityEnabled = false, want true (default on)")
	}
	if !cfg.MaturityDryRun {
		t.Error("MaturityDryRun = false, want true (probe-only until proven)")
	}
	if cfg.MaturityTouchModel != "" {
		t.Errorf("MaturityTouchModel = %q, want empty default (= auto)", cfg.MaturityTouchModel)
	}
	if cfg.MaturityTargetDays != 7 {
		t.Errorf("MaturityTargetDays = %d, want 7", cfg.MaturityTargetDays)
	}
}

func TestMaturityEnvOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("MATURITY_ENABLED", "0")
	t.Setenv("MATURITY_DRY_RUN", "0")
	t.Setenv("MATURITY_TOUCH_MODEL", "mimo/mimo-v2.5")
	t.Setenv("MATURITY_TARGET_DAYS", "14")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaturityEnabled || cfg.MaturityDryRun || cfg.MaturityTouchModel != "mimo/mimo-v2.5" ||
		cfg.MaturityTargetDays != 14 {
		t.Errorf("maturity overrides not applied: %+v", cfg)
	}
}

func TestMaturityValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv("MATURITY_TARGET_DAYS", "29")
	if _, err := Load(""); err == nil {
		t.Error("MATURITY_TARGET_DAYS=29 accepted, want 1..28 error")
	}
	clearEnv(t)
	t.Setenv("MATURITY_TOUCH_MODEL", "not-a-model")
	if _, err := Load(""); err == nil {
		t.Error("MATURITY_TOUCH_MODEL without provider/ rejected, want shape error")
	}
	clearEnv(t)
	t.Setenv("MATURITY_TOUCH_MODEL", "auto")
	if _, err := Load(""); err != nil {
		t.Errorf("MATURITY_TOUCH_MODEL=auto rejected: %v, want accepted sentinel", err)
	}
}
