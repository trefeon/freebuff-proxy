package testutil

import (
	"os"
	"testing"

	"freebuff-proxy/backend/internal/config"
)

// configEnvKeys is every environment variable backend/internal/config reads,
// derived from config.ConfigEnvKeys() (the catalog plus the legacy USER_ID
// alias). Ambient values from the developer's shell (e.g. AUTH_TOKENS
// exported for a live proxy) leak into config.Load with HIGHER precedence
// than .env, which breaks dashboard tests that assert on mode switches or
// token persistence (issue #281).
var configEnvKeys = config.ConfigEnvKeys()

// UnsetConfigEnv removes every ambient freebuff-proxy config env var for the
// duration of the test and restores them afterwards. Call it from tests that
// exercise config.Load (directly or via server handlers) so a developer's
// exported proxy environment cannot silently flip bridge mode, admin auth,
// or any other config key.
func UnsetConfigEnv(t *testing.T) {
	t.Helper()
	prev := make(map[string]*string, len(configEnvKeys))
	for _, k := range configEnvKeys {
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

// UnsetConfigEnvForTestMain strips ambient config env vars for the whole test
// binary. Use from a package TestMain so no test in the package inherits the
// developer's proxy environment. Tests that deliberately set a config key use
// t.Setenv, which still works afterwards.
func UnsetConfigEnvForTestMain() {
	for _, k := range configEnvKeys {
		_ = os.Unsetenv(k)
	}
}
