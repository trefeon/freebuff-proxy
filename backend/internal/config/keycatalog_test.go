package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// validCatalogKinds is the fixed set of form-control kinds the dashboard
// renderer understands.
var validCatalogKinds = map[string]bool{
	"bool": true, "select": true, "int": true, "text": true, "secret": true, "list": true,
}

// dotenvKeys is the set of keys applyDotenv mirrors from a .env file onto
// rawConfig (config_env.go). It must stay in lock-step with the catalog:
// a key the loader parses but the catalog does not describe would be lost
// from the settings form, and a catalog key the loader cannot apply would
// be a phantom row.
var dotenvKeys = map[string]bool{
	"AUTH_TOKENS": true, "LISTEN_ADDR": true, "UPSTREAM_BASE_URL": true,
	"ROTATION_INTERVAL": true, "REQUEST_TIMEOUT": true, "SESSION_CALL_TIMEOUT": true,
	"API_KEYS": true, "ADMIN_TOKEN": true, "COST_MODE": true,
	"ACTING_USER_ID": true, "TLS_FINGERPRINT": true, "REGISTRY_REFRESH": true,
	"DEBUG_DUMP": true, "DEVTOOLS_ENABLED": true, "LOG_FILE": true,
	"LOG_LEVEL": true, "LOG_FORMAT": true, "LOG_ACCESS": true,
	"LOG_RING_SIZE": true, "MAX_MESSAGES_PER_DAY": true, "BRIDGE_DAILY_LIMIT": true,
	"MAX_SPEND_PER_DAY": true, "BRIDGE_ENABLED": true, "BRIDGE_IDLE_EVICT": true,
	"IDLE_ROTATION_TIMEOUT": true, "SESSION_IDLE_END": true, "SAFE_MODE": true,
	"MODELS_HIDE_UNAVAILABLE": true, "MODELS_ALLOW": true, "CORS_ALLOWED_ORIGIN": true,
	"REQUEST_JITTER": true, "CLI_VERSION": true, "MODEL_ALIASES": true,
	"TRANSIENT_RETRIES": true, "SESSION_PERSIST": true, "SESSION_STATE_FILE": true,
	"HTTP2_UPSTREAM": true, "SESSION_CREATE_MAX_PARALLEL_GLOBAL": true,
	"SESSION_CREATE_MAX_PARALLEL_PER_MODEL": true, "RUN_FINISH_QUEUE_SIZE": true,
	"RUN_FINISH_INLINE_TIMEOUT": true, "RUNS_DRAIN_QUEUE_CAP": true,
	"RUNS_DRAIN_TTL": true, "SESSION_RE_ADMIT_LEAD": true, "SESSION_PROBE_CACHE_TTL": true,
	"MODEL_UNAVAILABLE_CACHE_TTL": true,
	"QUOTA_FALLBACK_MODELS":       true, "WEBHOOK_URL": true, "FALLBACK_AFTER_MS": true,
	"FALLBACK_MODEL": true, "ADOPT_CLI_SESSION": true, "WAITING_ROOM_CHAIN": true,
	"RATE_LIMIT_PER_IP": true, "RATE_LIMIT_BURST": true, "TOKEN_ROTATION": true,
	"DASHBOARD_ENABLED": true,
}

// catalogExtras are documented keys the catalog may hold beyond the
// applyDotenv set: env-only knobs the loader reads straight from the
// process environment (never from .env).
var catalogExtras = map[string]bool{
	"AUTO_DISCOVER_TOKEN": true,
}

// secretKeys is the exact set of keys whose effective value the dashboard
// must mask: credentials by shape, or URLs that may carry credentials.
var secretKeys = map[string]bool{
	"ADMIN_TOKEN": true, "AUTH_TOKENS": true, "API_KEYS": true, "WEBHOOK_URL": true,
}

// TestCatalogSanity pins the catalog's own invariants: unique keys, known
// groups and kinds, enum presence exactly for selects, secrets flagged, and
// non-empty descriptions.
func TestCatalogSanity(t *testing.T) {
	catalog := Catalog()
	if len(catalog) == 0 {
		t.Fatal("catalog is empty")
	}
	seen := make(map[string]bool, len(catalog))
	validGroups := map[string]bool{
		GroupGeneral: true, GroupPool: true, GroupQuota: true, GroupUpstream: true, GroupSecurity: true,
	}
	for i, def := range catalog {
		if def.Key == "" {
			t.Errorf("entry %d: empty key", i)
		}
		if seen[def.Key] {
			t.Errorf("duplicate catalog key: %s", def.Key)
		}
		seen[def.Key] = true
		if !validGroups[def.Group] {
			t.Errorf("key %s: invalid group %q", def.Key, def.Group)
		}
		if !validCatalogKinds[def.Kind] {
			t.Errorf("key %s: invalid kind %q", def.Key, def.Kind)
		}
		if def.Kind == "select" {
			if len(def.Enum) == 0 {
				t.Errorf("key %s: select kind without enum", def.Key)
			}
			if def.Default != "" && !contains(def.Enum, def.Default) {
				t.Errorf("key %s: default %q not in enum %v", def.Key, def.Default, def.Enum)
			}
		} else if len(def.Enum) > 0 {
			t.Errorf("key %s: enum on non-select kind %q", def.Key, def.Kind)
		}
		if def.Description == "" {
			t.Errorf("key %s: missing description", def.Key)
		}
		if def.Kind == "secret" && !def.Secret {
			t.Errorf("key %s: secret kind without secret flag", def.Key)
		}
	}
}

// TestCatalogCoversApplyDotenvKeys pins the catalog against the loader: every
// key applyDotenv applies must be described, and no catalog key may be a
// phantom (not applied and not a documented env-only extra).
func TestCatalogCoversApplyDotenvKeys(t *testing.T) {
	catalog := Catalog()
	have := make(map[string]bool, len(catalog))
	for _, def := range catalog {
		have[def.Key] = true
	}
	for key := range dotenvKeys {
		if !have[key] {
			t.Errorf("loader parses %s but the catalog lacks it — add it to keyCatalog", key)
		}
	}
	for key := range have {
		if !dotenvKeys[key] && !catalogExtras[key] {
			t.Errorf("catalog documents %s which the loader can neither apply nor has as an env-only key", key)
		}
	}
}

// TestCatalogOrderedPins the emission order: groups in the fixed UI order
// (general, pool, quota, upstream, security), keys byte-ascending within
// each group. Consumers render by group order, so the array must be stable.
func TestCatalogOrdered(t *testing.T) {
	catalog := Catalog()
	groupIndex := make(map[string]int, len(catalogGroupOrder))
	for i, g := range catalogGroupOrder {
		groupIndex[g] = i
	}
	prevGroup := ""
	for i, def := range catalog {
		if groupIndex[def.Group] < groupIndex[prevGroup] && prevGroup != "" {
			t.Errorf("group %q out of order: appears before group %q", def.Group, prevGroup)
		}
		if i > 0 && catalog[i-1].Group == def.Group && catalog[i-1].Key >= def.Key {
			t.Errorf("key %s out of order within group %s (after %s)", def.Key, def.Group, catalog[i-1].Key)
		}
		prevGroup = def.Group
	}
	// Byte-ascending within group must match sort.Strings exactly.
	for _, g := range catalogGroupOrder {
		var keys []string
		var groupKeys []string
		for _, def := range catalog {
			if def.Group == g {
				keys = append(keys, def.Key)
				groupKeys = append(groupKeys, def.Key)
			}
		}
		sort.Strings(keys)
		for i := range keys {
			if keys[i] != groupKeys[i] {
				t.Errorf("group %s not sorted: %v", g, groupKeys)
				break
			}
		}
	}
}

// TestCatalogSecretFlags pins the exact secret set so a credential-bearing
// key added to the loader never reaches the dashboard unmasked, and a
// mistakenly masked key is caught early.
func TestCatalogSecretFlags(t *testing.T) {
	catalog := Catalog()
	flagged := make(map[string]bool)
	for _, def := range catalog {
		if def.Secret {
			flagged[def.Key] = true
		}
	}
	if len(flagged) != len(secretKeys) {
		t.Errorf("secret count = %d, want %d (%v)", len(flagged), len(secretKeys), flagged)
	}
	for key := range secretKeys {
		if !flagged[key] {
			t.Errorf("key %s must be flagged secret", key)
		}
	}
	for key := range flagged {
		if !secretKeys[key] {
			t.Errorf("key %s is flagged secret but is not in the expected secret set", key)
		}
	}
}

// TestConfigMetaFixtureParity asserts that the frontend e2e mock fixture
// frontend/e2e/fixtures/config-meta.json stays byte-exact in sync with
// Catalog(). Run with FP_REGEN_FIXTURE=1 to regenerate the fixture.
func TestConfigMetaFixtureParity(t *testing.T) {
	data, err := json.MarshalIndent(Catalog(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	fixturePath := filepath.Join("..", "..", "..", "frontend", "e2e", "fixtures", "config-meta.json")

	if os.Getenv("FP_REGEN_FIXTURE") != "" {
		if err := os.WriteFile(fixturePath, data, 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Logf("regenerated %s", fixturePath)
		return
	}

	existing, err := os.ReadFile(fixturePath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("fixture does not exist; skipping parity check")
		}
		t.Fatal(err)
	}
	normExisting := strings.ReplaceAll(string(existing), "\r\n", "\n")
	normData := string(data)
	if normExisting != normData {
		t.Errorf("frontend e2e fixture %s is out of date with Catalog(); re-run with FP_REGEN_FIXTURE=1 to regenerate", fixturePath)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
