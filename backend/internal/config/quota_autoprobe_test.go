package config

import (
	"os"
	"testing"
)

// Quota auto-probe scheduler knob (ADR-0022): default on, live-apply
// GroupPool bool across every tier (env, .env, DB overlay).
func TestQuotaAutoProbeDefault(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.QuotaAutoProbe {
		t.Error("QuotaAutoProbe = false, want true (default on)")
	}
}

func TestQuotaAutoProbeEnvOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("QUOTA_AUTO_PROBE", "0")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaAutoProbe {
		t.Error("QuotaAutoProbe = true with QUOTA_AUTO_PROBE=0, want false")
	}
}

func TestQuotaAutoProbeDotenv(t *testing.T) {
	clearEnv(t)
	if err := os.WriteFile(".env", []byte("QUOTA_AUTO_PROBE=false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaAutoProbe {
		t.Error("QuotaAutoProbe = true with .env QUOTA_AUTO_PROBE=false, want false")
	}
}

func TestQuotaAutoProbeOverlay(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadOpts("", LoadOptions{Overlay: map[string]string{"QUOTA_AUTO_PROBE": "false"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaAutoProbe {
		t.Error("QuotaAutoProbe = true with overlay QUOTA_AUTO_PROBE=false, want false")
	}
	if err := ValidateSettingValue("QUOTA_AUTO_PROBE", "false"); err != nil {
		t.Errorf("ValidateSettingValue(QUOTA_AUTO_PROBE,false) = %v, want nil", err)
	}
	if err := ValidateSettingValue("QUOTA_AUTO_PROBE", "maybe"); err == nil {
		t.Error("ValidateSettingValue(QUOTA_AUTO_PROBE,maybe) accepted, want an error")
	}
}
