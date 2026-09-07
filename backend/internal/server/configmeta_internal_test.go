package server

import (
	"testing"

	"freebuff-proxy/backend/internal/config"
)

// TestConfigCatalogRestartOnlyMatchesServer pins the catalog's restart_only
// flags to the server's restart-only set: the dashboard save response lists
// exactly the keys whose live objects keep boot-time values, so catalog and
// server must agree or the settings form would mark restart-only keys that
// the save flow does not report (or miss ones it does).
func TestConfigCatalogRestartOnlyMatchesServer(t *testing.T) {
	catalogByKey := make(map[string]config.KeyDef)
	for _, def := range config.Catalog() {
		catalogByKey[def.Key] = def
	}
	serverSet := make(map[string]bool, len(restartOnlyConfigKeys))
	for _, k := range restartOnlyConfigKeys {
		serverSet[k] = true
		def, ok := catalogByKey[k]
		if !ok {
			t.Errorf("restart-only key %s is missing from the config catalog", k)
			continue
		}
		if !def.RestartOnly {
			t.Errorf("restart-only key %s is not flagged restart_only in the catalog", k)
		}
	}
	for _, def := range config.Catalog() {
		if def.RestartOnly && !serverSet[def.Key] {
			t.Errorf("catalog marks %s restart_only but the server's restartOnlyConfigKeys does not include it", def.Key)
		}
	}
}

// TestRestartOnlyIncludesLoggerAndListener pins the review finding that the
// logger (LOG_LEVEL/LOG_FORMAT/LOG_FILE/LOG_RING_SIZE) and the listener
// socket (LISTEN_ADDR) are reload-proof: applyReloadedConfig never touches
// the logger and the socket never re-binds, so both the server set (which
// drives the .env-save message) and the catalog flags (which drive the
// settings POST code) must carry them. Agreement itself is pinned by
// TestConfigCatalogRestartOnlyMatchesServer above.
func TestRestartOnlyIncludesLoggerAndListener(t *testing.T) {
	for _, key := range []string{"LOG_LEVEL", "LOG_FORMAT", "LOG_FILE", "LOG_RING_SIZE", "LISTEN_ADDR"} {
		found := false
		for _, k := range restartOnlyConfigKeys {
			if k == key {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s missing from restartOnlyConfigKeys", key)
		}
	}
}
