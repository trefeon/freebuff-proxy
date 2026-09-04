package config

// Effective-config rendering for the dashboard (issue #288): one owner for
// the key -> display-value mapping (moved out of dashboard_data.go's
// formatKey switch) and the .env editor template generated from the
// catalog's own defaults. Secret classification comes from the catalog;
// the few value shapes (counts, boolWord) stay here so every consumer
// renders identically.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// DataEntry is one effective-config key rendered for the dashboard.
type DataEntry struct {
	Key    string
	Value  string
	Secret bool
}

// Data returns the effective configuration as a key map in catalog order:
// secret classification is the catalog's, value shapes are the canonical
// renderings (secret counts / set-unset words / duration strings). Keys the
// Config has no field for render as their catalog default or "".
func (c *Config) Data() []DataEntry {
	out := make([]DataEntry, 0, len(Catalog()))
	for _, def := range Catalog() {
		val, valueIsSecret := renderKey(c, def.Key)
		out = append(out, DataEntry{
			Key:    def.Key,
			Value:  val,
			Secret: def.Secret || valueIsSecret,
		})
	}
	return out
}

// renderKey maps one config key to its canonical display value.
func renderKey(c *Config, key string) (val string, valueIsSecret bool) {
	switch key {
	case "LISTEN_ADDR":
		return c.ListenAddr, false
	case "UPSTREAM_BASE_URL":
		return c.UpstreamBaseURL, false
	case "AUTH_TOKENS":
		return fmt.Sprintf("%d token(s)", len(c.AuthTokens)), true
	case "API_KEYS":
		return fmt.Sprintf("%d key(s)", len(c.APIKeys)), true
	case "ADMIN_TOKEN":
		return boolWord(c.AdminToken != ""), true
	case "ROTATION_INTERVAL":
		return c.RotationInterval.String(), false
	case "REQUEST_TIMEOUT":
		return c.RequestTimeout.String(), false
	case "HTTP_READ_TIMEOUT":
		return c.HTTPReadTimeout.String(), false
	case "SESSION_CALL_TIMEOUT":
		return c.SessionCallTimeout.String(), false
	case "COST_MODE":
		return c.CostMode, false
	case "TLS_FINGERPRINT":
		return c.TLSFingerprint, false
	case "REGISTRY_REFRESH":
		return c.RegistryRefresh.String(), false
	case "DEBUG_DUMP":
		return strconv.FormatBool(c.DebugDump), false
	case "LOG_FILE":
		return c.LogFile, false
	case "LOG_LEVEL":
		return c.LogLevel, false
	case "LOG_FORMAT":
		return c.LogFormat, false
	case "LOG_ACCESS":
		return strconv.FormatBool(c.LogAccess), false
	case "LOG_RING_SIZE":
		return strconv.Itoa(c.LogRingSize), false
	case "MAX_MESSAGES_PER_DAY":
		return strconv.Itoa(c.MaxMessagesPerDay), false
	case "MAX_SPEND_PER_DAY":
		return strconv.FormatInt(c.MaxSpendPerDay, 10), false
	case "IDLE_ROTATION_TIMEOUT":
		return c.IdleRotationTimeout.String(), false
	case "SAFE_MODE":
		return strconv.FormatBool(c.SafeMode), false
	case "REQUEST_JITTER":
		return c.RequestJitter.String(), false
	case "CLI_VERSION":
		return c.CLIVersion, false
	case "MODEL_ALIASES":
		return joinPairs(c.ModelAliases, ":"), false
	case "MODELS_ALLOW":
		return strings.Join(c.ModelsAllow, ","), false
	case "MODELS_HIDE_UNAVAILABLE":
		return strconv.FormatBool(c.ModelsHideUnavailable), false
	case "TRANSIENT_RETRIES":
		return strconv.Itoa(c.TransientRetries), false
	case "DASHBOARD_ENABLED":
		return strconv.FormatBool(c.DashboardEnabled), false
	case "DASHBOARD_REQUIRE_LOGIN":
		return strconv.FormatBool(c.DashboardRequireLogin), false
	case "SESSION_PERSIST":
		return strconv.FormatBool(c.SessionPersist), false
	case "SESSION_STATE_FILE":
		return c.SessionStateFile, false
	case "TOKEN_ROTATION":
		return c.TokenRotation, false
	case "RATE_LIMIT_FAILOVER":
		return strconv.FormatBool(c.RateLimitFailover), false
	case "MODEL_LOCKS":
		return formatModelLocks(c.ModelLocks), false
	case "BRIDGE_ENABLED":
		return strconv.FormatBool(c.BridgeEnabled), false
	case "BRIDGE_IDLE_EVICT":
		return c.BridgeIdleEvict.String(), false
	case "BRIDGE_DAILY_LIMIT":
		return strconv.Itoa(c.BridgeDailyLimit), false
	case "FALLBACK_AFTER_MS":
		return strconv.Itoa(int(c.FallbackAfter.Milliseconds())), false
	case "FALLBACK_MODEL":
		return joinPairs(c.FallbackModels, "="), false
	case "QUOTA_FALLBACK_MODELS":
		return joinPairs(c.QuotaFallbackModels, "="), false
	case "SESSION_IDLE_END":
		return c.SessionIdleEnd.String(), false
	case "SESSION_PROBE_CACHE_TTL":
		return c.SessionProbeCacheTTL.String(), false
	case "SESSION_RE_ADMIT_LEAD":
		return c.SessionReAdmitLead.String(), false
	case "SESSION_CREATE_MAX_PARALLEL_GLOBAL":
		return strconv.Itoa(c.SessionCreateMaxParallelGlobal), false
	case "SESSION_CREATE_MAX_PARALLEL_PER_MODEL":
		return strconv.Itoa(c.SessionCreateMaxParallelPerModel), false
	case "RUN_FINISH_QUEUE_SIZE":
		return strconv.Itoa(c.RunFinishQueueSize), false
	case "RUN_FINISH_INLINE_TIMEOUT":
		return c.RunFinishInlineTimeout.String(), false
	case "RUNS_DRAIN_QUEUE_CAP":
		return strconv.Itoa(c.RunsDrainQueueCap), false
	case "RUNS_DRAIN_TTL":
		return c.RunsDrainTTL.String(), false
	case "MODEL_UNAVAILABLE_CACHE_TTL":
		return c.ModelUnavailableCacheTTL.String(), false
	case "RATE_LIMIT_PER_IP":
		return strconv.FormatFloat(c.RateLimitPerIP, 'f', -1, 64), false
	case "RATE_LIMIT_BURST":
		return strconv.Itoa(c.RateLimitBurst), false
	case "CORS_ALLOWED_ORIGIN":
		return c.CORSAllowedOrigin, false
	case "WEBHOOK_URL":
		return c.WebhookURL, true
	case "HTTP2_UPSTREAM":
		return strconv.FormatBool(c.HTTP2Upstream), false
	case "AUTO_DISCOVER_TOKEN":
		return "true", false
	case "DEVTOOLS_ENABLED":
		return strconv.FormatBool(c.DevToolsEnabled), false
	case "ADOPT_CLI_SESSION":
		return strconv.FormatBool(c.AdoptCLISession), false
	case "WAITING_ROOM_CHAIN":
		return strconv.FormatBool(c.WaitingRoomChain), false
	default:
		// A catalog key with no Config field (new upstream knob not yet
		// wired into Config): fall back to the catalog default so the
		// dashboard never silently shows an empty row.
		return defaultFor(key), false
	}
}

func defaultFor(key string) string {
	for _, def := range Catalog() {
		if def.Key == key {
			return def.Default
		}
	}
	return ""
}

func joinPairs(m map[string]string, sep string) string {
	if len(m) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, k+sep+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// formatModelLocks renders the parsed MODEL_LOCKS map back to canonical
// "idx:model,model;..." form (slots ascending) for dashboard display.
func formatModelLocks(locks map[int][]string) string {
	if len(locks) == 0 {
		return ""
	}
	idxs := make([]int, 0, len(locks))
	for idx := range locks {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	parts := make([]string, 0, len(idxs))
	for _, idx := range idxs {
		parts = append(parts, strconv.Itoa(idx)+":"+strings.Join(locks[idx], ","))
	}
	return strings.Join(parts, ";")
}

// boolWord is the canonical set/unset wording for presence-sensitive keys.
func boolWord(v bool) string {
	if v {
		return "set"
	}
	return "unset"
}

// DefaultEnvTemplate generates the raw .env editor template from the
// catalog defaults: every non-hidden key (secret keys included, so a fresh
// install still sees AUTH_TOKENS/API_KEYS) appears commented-out with its
// default, in catalog order.
func DefaultEnvTemplate() string {
	var b strings.Builder
	b.WriteString("# freebuff-proxy configuration (.env)\n")
	b.WriteString("# Keys mirror the environment variables; leave commented to keep the default.\n")
	b.WriteString("# See the README and docs/guides for the full reference.\n\n")
	for _, def := range Catalog() {
		if def.Hidden && !def.Secret {
			continue
		}
		b.WriteString("#" + def.Key + "=" + def.Default + "\n")
	}
	return b.String()
}
