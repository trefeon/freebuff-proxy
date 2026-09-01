package config

// Local test helpers mirroring internal/testutil but sourced from
// ConfigEnvKeys() so config's own internal tests do not import testutil
// (which itself imports config — an import cycle in test builds).

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// unsetConfigEnv removes every ambient freebuff-proxy config env var for the
// duration of the test and restores them afterwards (mirrors
// testutil.UnsetConfigEnv, but driven by ConfigEnvKeys()).
func unsetConfigEnv(t *testing.T) {
	t.Helper()
	prev := make(map[string]*string, len(ConfigEnvKeys()))
	for _, k := range ConfigEnvKeys() {
		if v, ok := os.LookupEnv(k); ok {
			val := v
			prev[k] = &val
			if err := os.Unsetenv(k); err != nil {
				t.Fatalf("unset %s: %v", k, err)
			}
		}
	}
	t.Cleanup(func() {
		for k, v := range prev {
			if v == nil {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, *v)
			}
		}
	})
}

// unsetConfigEnvForTestMain strips ambient config env vars for the whole
// test binary (mirrors testutil.UnsetConfigEnvForTestMain).
func unsetConfigEnvForTestMain() {
	for _, k := range ConfigEnvKeys() {
		_ = os.Unsetenv(k)
	}
}

// drainStrayTempFiles removes leftover .tmp*/.bak files in dir (mirrors
// testutil.DrainStrayTempFiles).
func drainStrayTempFiles(t *testing.T, dir string) {
	t.Helper()
	patterns := []string{
		filepath.Join(dir, "*.tmp*"),
		filepath.Join(dir, "*.bak"),
	}
	const attempts = 50
	for i := 0; i < attempts; i++ {
		stray := 0
		for _, pat := range patterns {
			matches, err := filepath.Glob(pat)
			if err != nil {
				continue
			}
			for _, m := range matches {
				if os.Remove(m) != nil {
					stray++
				}
			}
		}
		if stray == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
