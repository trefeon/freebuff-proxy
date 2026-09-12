package config

// Step-1 smart-routing knob tests: ROUTING_SMART, TOKEN_MAX_CONCURRENT,
// QUEUE_WAIT, QUEUE_DEPTH — defaults, env/file/JSON tiers, floors, and
// validation. Restart behavior: all four are live-apply (read per Acquire
// through the atomic config pointer; no pool rebuild).

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withRoutingEnvUnset removes the routing knobs from the environment so a
// test observes loader defaults, restoring any prior values afterwards.
// (unsetConfigEnv only strips catalog keys; the routing knobs land in the
// catalog in step 3.)
func withRoutingEnvUnset(t *testing.T) {
	t.Helper()
	keys := []string{"ROUTING_SMART", "TOKEN_MAX_CONCURRENT", "QUEUE_WAIT", "QUEUE_DEPTH"}
	saved := make(map[string]string, len(keys))
	present := make(map[string]bool, len(keys))
	for _, k := range keys {
		v, ok := os.LookupEnv(k)
		saved[k], present[k] = v, ok
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, k := range keys {
			if present[k] {
				_ = os.Setenv(k, saved[k])
			} else {
				_ = os.Unsetenv(k)
			}
		}
	})
}

func TestRoutingKnobDefaults(t *testing.T) {
	unsetConfigEnv(t)
	withRoutingEnvUnset(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.RoutingSmart {
		t.Error("RoutingSmart = false, want true (smart routing on by default)")
	}
	if cfg.TokenMaxConcurrent != 2 {
		t.Errorf("TokenMaxConcurrent = %d, want 2", cfg.TokenMaxConcurrent)
	}
	if cfg.QueueWait != 30*time.Second {
		t.Errorf("QueueWait = %v, want 30s", cfg.QueueWait)
	}
	if cfg.QueueDepth != 16 {
		t.Errorf("QueueDepth = %d, want 16", cfg.QueueDepth)
	}
}

func TestRoutingKnobEnvOverrides(t *testing.T) {
	unsetConfigEnv(t)
	withRoutingEnvUnset(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("ROUTING_SMART", "false")
	t.Setenv("TOKEN_MAX_CONCURRENT", "1")
	t.Setenv("QUEUE_WAIT", "5s")
	t.Setenv("QUEUE_DEPTH", "4")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RoutingSmart {
		t.Error("RoutingSmart = true, want false")
	}
	if cfg.TokenMaxConcurrent != 1 {
		t.Errorf("TokenMaxConcurrent = %d, want 1 (bunker)", cfg.TokenMaxConcurrent)
	}
	if cfg.QueueWait != 5*time.Second {
		t.Errorf("QueueWait = %v, want 5s", cfg.QueueWait)
	}
	if cfg.QueueDepth != 4 {
		t.Errorf("QueueDepth = %d, want 4", cfg.QueueDepth)
	}
}

func TestRoutingKnobFloors(t *testing.T) {
	unsetConfigEnv(t)
	withRoutingEnvUnset(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	// TOKEN_MAX_CONCURRENT floors at 1: a zero live-turn cap could never
	// serve, so 0/negative values floor instead of failing the load.
	for _, v := range []string{"0", "-3"} {
		t.Setenv("TOKEN_MAX_CONCURRENT", v)
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load(TOKEN_MAX_CONCURRENT=%s): %v", v, err)
		}
		if cfg.TokenMaxConcurrent != 1 {
			t.Errorf("TokenMaxConcurrent(%s) = %d, want floor 1", v, cfg.TokenMaxConcurrent)
		}
	}
	// QUEUE_DEPTH=0 disables queueing (fail over at once when full).
	t.Setenv("TOKEN_MAX_CONCURRENT", "2")
	t.Setenv("QUEUE_DEPTH", "0")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.QueueDepth != 0 {
		t.Errorf("QueueDepth = %d, want 0 (no queueing)", cfg.QueueDepth)
	}
	// QUEUE_WAIT is zero-tolerant like BURST_WINDOW: non-positive falls
	// back to the 30s default.
	t.Setenv("QUEUE_DEPTH", "16")
	t.Setenv("QUEUE_WAIT", "0s")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.QueueWait != 30*time.Second {
		t.Errorf("QueueWait(0s) = %v, want 30s fallback", cfg.QueueWait)
	}
}

func TestRoutingKnobValidation(t *testing.T) {
	unsetConfigEnv(t)
	withRoutingEnvUnset(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("QUEUE_DEPTH", "-1")
	if _, err := Load(""); err == nil {
		t.Error("negative QUEUE_DEPTH accepted")
	}
	t.Setenv("QUEUE_DEPTH", "16")
	t.Setenv("QUEUE_WAIT", "bogus")
	if _, err := Load(""); err == nil {
		t.Error("bogus QUEUE_WAIT accepted")
	}
}

func TestRoutingKnobDotenvTier(t *testing.T) {
	unsetConfigEnv(t)
	withRoutingEnvUnset(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	dir := t.TempDir()
	env := "ROUTING_SMART=false\nTOKEN_MAX_CONCURRENT=1\nQUEUE_WAIT=7s\nQUEUE_DEPTH=3\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RoutingSmart || cfg.TokenMaxConcurrent != 1 || cfg.QueueWait != 7*time.Second || cfg.QueueDepth != 3 {
		t.Errorf("dotenv tier not applied: smart=%v cap=%d wait=%v depth=%d",
			cfg.RoutingSmart, cfg.TokenMaxConcurrent, cfg.QueueWait, cfg.QueueDepth)
	}
}

func TestRoutingKnobJSONTier(t *testing.T) {
	unsetConfigEnv(t)
	withRoutingEnvUnset(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	dir := t.TempDir()
	doc := `{"ROUTING_SMART": false, "TOKEN_MAX_CONCURRENT": 1, "QUEUE_WAIT": "9s", "QUEUE_DEPTH": 5}`
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RoutingSmart || cfg.TokenMaxConcurrent != 1 || cfg.QueueWait != 9*time.Second || cfg.QueueDepth != 5 {
		t.Errorf("JSON tier not applied: smart=%v cap=%d wait=%v depth=%d",
			cfg.RoutingSmart, cfg.TokenMaxConcurrent, cfg.QueueWait, cfg.QueueDepth)
	}
}
