package config

// Wave-3 config knob tests: session-create gate caps (#86), the bounded
// finish queue (#90), the draining-list bounds (#55), the re-admit lead
// (#99), and the probe cache TTL (#60).

import (
	"testing"
	"time"
)

func TestWave3KnobDefaults(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SessionCreateMaxParallelGlobal != 128 || cfg.SessionCreateMaxParallelPerModel != 32 {
		t.Errorf("gate caps = %d/%d, want 128/32", cfg.SessionCreateMaxParallelGlobal, cfg.SessionCreateMaxParallelPerModel)
	}
	if cfg.RunFinishQueueSize != 64 {
		t.Errorf("RunFinishQueueSize = %d, want 64", cfg.RunFinishQueueSize)
	}
	if cfg.RunFinishInlineTimeout != 250*time.Millisecond {
		t.Errorf("RunFinishInlineTimeout = %v, want 250ms", cfg.RunFinishInlineTimeout)
	}
	if cfg.RunsDrainQueueCap != 64 || cfg.RunsDrainTTL != 10*time.Minute {
		t.Errorf("drain bounds = %d/%v, want 64/10m", cfg.RunsDrainQueueCap, cfg.RunsDrainTTL)
	}
	if cfg.SessionReAdmitLead != 60*time.Second {
		t.Errorf("SessionReAdmitLead = %v, want 60s", cfg.SessionReAdmitLead)
	}
	if cfg.SessionProbeCacheTTL != 15*time.Second {
		t.Errorf("SessionProbeCacheTTL = %v, want 15s", cfg.SessionProbeCacheTTL)
	}
}

func TestWave3KnobEnvOverrides(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("SESSION_CREATE_MAX_PARALLEL_GLOBAL", "4")
	t.Setenv("SESSION_CREATE_MAX_PARALLEL_PER_MODEL", "2")
	t.Setenv("RUN_FINISH_QUEUE_SIZE", "8")
	t.Setenv("RUN_FINISH_INLINE_TIMEOUT", "100ms")
	t.Setenv("RUNS_DRAIN_QUEUE_CAP", "3")
	t.Setenv("RUNS_DRAIN_TTL", "5m")
	t.Setenv("SESSION_RE_ADMIT_LEAD", "30s")
	t.Setenv("SESSION_PROBE_CACHE_TTL", "7s")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SessionCreateMaxParallelGlobal != 4 || cfg.SessionCreateMaxParallelPerModel != 2 {
		t.Errorf("gate caps = %d/%d, want 4/2", cfg.SessionCreateMaxParallelGlobal, cfg.SessionCreateMaxParallelPerModel)
	}
	if cfg.RunFinishQueueSize != 8 || cfg.RunFinishInlineTimeout != 100*time.Millisecond {
		t.Errorf("finish queue = %d/%v, want 8/100ms", cfg.RunFinishQueueSize, cfg.RunFinishInlineTimeout)
	}
	if cfg.RunsDrainQueueCap != 3 || cfg.RunsDrainTTL != 5*time.Minute {
		t.Errorf("drain bounds = %d/%v, want 3/5m", cfg.RunsDrainQueueCap, cfg.RunsDrainTTL)
	}
	if cfg.SessionReAdmitLead != 30*time.Second || cfg.SessionProbeCacheTTL != 7*time.Second {
		t.Errorf("session knobs = %v/%v, want 30s/7s", cfg.SessionReAdmitLead, cfg.SessionProbeCacheTTL)
	}
}

func TestWave3KnobValidation(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("SESSION_CREATE_MAX_PARALLEL_GLOBAL", "-1")
	if _, err := Load(""); err == nil {
		t.Error("negative SESSION_CREATE_MAX_PARALLEL_GLOBAL accepted")
	}
	t.Setenv("SESSION_CREATE_MAX_PARALLEL_GLOBAL", "128")
	t.Setenv("RUN_FINISH_QUEUE_SIZE", "-5")
	if _, err := Load(""); err == nil {
		t.Error("negative RUN_FINISH_QUEUE_SIZE accepted")
	}
	t.Setenv("RUN_FINISH_QUEUE_SIZE", "64")
	t.Setenv("SESSION_RE_ADMIT_LEAD", "bogus")
	if _, err := Load(""); err == nil {
		t.Error("bogus SESSION_RE_ADMIT_LEAD accepted")
	}
}
