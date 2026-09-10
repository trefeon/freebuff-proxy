package config

import (
	"os"
	"testing"
	"time"
)

// Smart-probe cadence knobs: defaults, live-apply across every tier (env,
// .env, DB overlay), and the settings POST gate.
func TestQuotaProbeCadenceDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaProbeActiveInterval != time.Minute {
		t.Errorf("QuotaProbeActiveInterval = %v, want 60s (default)", cfg.QuotaProbeActiveInterval)
	}
	if cfg.QuotaProbeIdleHeartbeat != 30*time.Minute {
		t.Errorf("QuotaProbeIdleHeartbeat = %v, want 30m (default)", cfg.QuotaProbeIdleHeartbeat)
	}
}

func TestQuotaProbeCadenceEnvOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("QUOTA_PROBE_ACTIVE_INTERVAL", "30s")
	t.Setenv("QUOTA_PROBE_IDLE_HEARTBEAT", "10m")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaProbeActiveInterval != 30*time.Second {
		t.Errorf("QuotaProbeActiveInterval = %v, want 30s (env wins)", cfg.QuotaProbeActiveInterval)
	}
	if cfg.QuotaProbeIdleHeartbeat != 10*time.Minute {
		t.Errorf("QuotaProbeIdleHeartbeat = %v, want 10m (env wins)", cfg.QuotaProbeIdleHeartbeat)
	}
}

func TestQuotaProbeCadenceDotenv(t *testing.T) {
	clearEnv(t)
	if err := os.WriteFile(".env", []byte("QUOTA_PROBE_ACTIVE_INTERVAL=45s\nQUOTA_PROBE_IDLE_HEARTBEAT=15m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaProbeActiveInterval != 45*time.Second {
		t.Errorf("QuotaProbeActiveInterval = %v, want 45s (from .env)", cfg.QuotaProbeActiveInterval)
	}
	if cfg.QuotaProbeIdleHeartbeat != 15*time.Minute {
		t.Errorf("QuotaProbeIdleHeartbeat = %v, want 15m (from .env)", cfg.QuotaProbeIdleHeartbeat)
	}
}

func TestQuotaProbeCadenceOverlay(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadOpts("", LoadOptions{Overlay: map[string]string{
		"QUOTA_PROBE_ACTIVE_INTERVAL": "20s",
		"QUOTA_PROBE_IDLE_HEARTBEAT":  "5m",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaProbeActiveInterval != 20*time.Second {
		t.Errorf("QuotaProbeActiveInterval = %v, want 20s (overlay wins)", cfg.QuotaProbeActiveInterval)
	}
	if cfg.QuotaProbeIdleHeartbeat != 5*time.Minute {
		t.Errorf("QuotaProbeIdleHeartbeat = %v, want 5m (overlay wins)", cfg.QuotaProbeIdleHeartbeat)
	}
}

func TestQuotaProbeCadenceValidateSettingValue(t *testing.T) {
	for key, value := range map[string]string{
		"QUOTA_PROBE_ACTIVE_INTERVAL": "30s",
		"QUOTA_PROBE_IDLE_HEARTBEAT":  "10m",
	} {
		if err := ValidateSettingValue(key, value); err != nil {
			t.Errorf("ValidateSettingValue(%s,%s) = %v, want nil", key, value, err)
		}
	}
	for key, value := range map[string]string{
		"QUOTA_PROBE_ACTIVE_INTERVAL": "soon",
		"QUOTA_PROBE_IDLE_HEARTBEAT":  "often",
	} {
		if err := ValidateSettingValue(key, value); err == nil {
			t.Errorf("ValidateSettingValue(%q,%q) accepted, want an error", key, value)
		}
	}
}

func TestQuotaProbeCadenceBadDurationRejects(t *testing.T) {
	clearEnv(t)
	t.Setenv("QUOTA_PROBE_ACTIVE_INTERVAL", "soon")
	if _, err := Load(""); err == nil {
		t.Error("Load with QUOTA_PROBE_ACTIVE_INTERVAL=soon accepted, want an error")
	}
}
