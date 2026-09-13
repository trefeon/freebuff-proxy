package config

import (
	"strings"
	"testing"
	"time"
)

// TestLogWindowDefaults pins the two dashboard log knobs: LOG_CONSOLE_WINDOW
// is a 1h VIEW window by default, and LOG_TABLE_RETENTION keeps history rows
// for 168h (7 days) instead of the old hardcoded 30 days.
func TestLogWindowDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogConsoleWindow != time.Hour {
		t.Errorf("LogConsoleWindow = %v, want 1h default", cfg.LogConsoleWindow)
	}
	if cfg.LogTableRetention != 168*time.Hour {
		t.Errorf("LogTableRetention = %v, want 168h (7d) default", cfg.LogTableRetention)
	}
}

// TestLogWindowParse verifies both keys load from the environment as Go
// durations and land in Config.
func TestLogWindowParse(t *testing.T) {
	clearEnv(t)
	t.Setenv("LOG_CONSOLE_WINDOW", "15m")
	t.Setenv("LOG_TABLE_RETENTION", "48h")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogConsoleWindow != 15*time.Minute {
		t.Errorf("LogConsoleWindow = %v, want 15m", cfg.LogConsoleWindow)
	}
	if cfg.LogTableRetention != 48*time.Hour {
		t.Errorf("LogTableRetention = %v, want 48h", cfg.LogTableRetention)
	}
}

// TestLogWindowZeroFallsBackToDefault is the guard that keeps a zero value
// harmless: LOG_TABLE_RETENTION=0 would otherwise purge every history row on
// the next tick, and a zero console window would show an empty view. Both
// keys fall back to their documented defaults for "" too.
func TestLogWindowZeroFallsBackToDefault(t *testing.T) {
	for _, v := range []string{"", "0", "0s", "-1h"} {
		t.Run("value="+v, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("LOG_CONSOLE_WINDOW", v)
			t.Setenv("LOG_TABLE_RETENTION", v)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load(%q): %v", v, err)
			}
			if cfg.LogConsoleWindow != DefaultLogConsoleWindow {
				t.Errorf("LogConsoleWindow = %v after %q, want %v", cfg.LogConsoleWindow, v, DefaultLogConsoleWindow)
			}
			if cfg.LogTableRetention != DefaultLogTableRetention {
				t.Errorf("LogTableRetention = %v after %q, want %v", cfg.LogTableRetention, v, DefaultLogTableRetention)
			}
		})
	}
}

// TestLogWindowInvalidFailsLoad: a typo must fail loudly rather than silently
// fall back — a mistyped retention that quietly reverted to the default would
// look like data loss the operator never asked for.
func TestLogWindowInvalidFailsLoad(t *testing.T) {
	clearEnv(t)
	t.Setenv("LOG_CONSOLE_WINDOW", "1 hour")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "LOG_CONSOLE_WINDOW") {
		t.Fatalf("Load(LOG_CONSOLE_WINDOW=1 hour): err = %v, want parse error naming the key", err)
	}

	clearEnv(t)
	t.Setenv("LOG_TABLE_RETENTION", "7d")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "LOG_TABLE_RETENTION") {
		t.Fatalf("Load(LOG_TABLE_RETENTION=7d): err = %v, want parse error naming the key", err)
	}
}
