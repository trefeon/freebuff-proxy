package config

// config_keys.go - raw key declarations, split from config.go (Wave C revamp):
// the rawConfig key mirror, the lenient list/map JSON shapes, and the
// built-in defaults. The load precedence pipeline lives in config_load.go.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// rawConfig mirrors the JSON file / env keys as strings so that parsing and
// validation happen once, after all overrides are applied.
type rawConfig struct {
	ListenAddr      string   `json:"LISTEN_ADDR"`
	UpstreamBaseURL string   `json:"UPSTREAM_BASE_URL"`
	AuthTokens      []string `json:"AUTH_TOKENS"`
	// AuthTokensSet records that AUTH_TOKENS was explicitly provided (even
	// as an empty value) by the JSON file, .env, or the environment. An
	// explicitly-empty AUTH_TOKENS means the operator chose bridge mode, so
	// CLI auto-discovery must not refill it (runtime mode switch persists
	// "AUTH_TOKENS=" to .env and relies on this).
	AuthTokensSet      bool     `json:"-"`
	RotationInterval   string   `json:"ROTATION_INTERVAL"`
	RequestTimeout     string   `json:"REQUEST_TIMEOUT"`
	SessionCallTimeout string   `json:"SESSION_CALL_TIMEOUT"`
	HTTPReadTimeout    string   `json:"HTTP_READ_TIMEOUT"`
	APIKeys            []string `json:"API_KEYS"`
	AdminToken         string   `json:"ADMIN_TOKEN"`
	CostMode           string   `json:"COST_MODE"`
	ActingUserID       string   `json:"ACTING_USER_ID"`
	// LegacyActingUserID is the pre-rename JSON key (USER_ID) — merged at
	// the end of Load when no ACTING_USER_ID source set a value (#126).
	LegacyActingUserID   string `json:"USER_ID"`
	TLSFingerprint       string `json:"TLS_FINGERPRINT"`
	RegistryRefresh      string `json:"REGISTRY_REFRESH"`
	DebugDump            bool   `json:"DEBUG_DUMP"`
	DevToolsEnabled      bool   `json:"DEVTOOLS_ENABLED"`
	LogFile              string `json:"LOG_FILE"`
	LogLevel             string `json:"LOG_LEVEL"`
	LogFormat            string `json:"LOG_FORMAT"`
	LogAccess            bool   `json:"LOG_ACCESS"`
	LogRingSize          *int   `json:"LOG_RING_SIZE"`
	MaxMessagesPerDay    *int   `json:"MAX_MESSAGES_PER_DAY"`
	MaxRequestsPerDay    *int   `json:"MAX_REQUESTS_PER_DAY"`
	MaxRequestsPerMinute *int   `json:"MAX_REQUESTS_PER_MINUTE"`
	// BridgeDailyLimit is the global daily chat cap across all bridge
	// entries (BRIDGE_DAILY_LIMIT; 0 = unlimited).
	BridgeDailyLimit *int `json:"BRIDGE_DAILY_LIMIT"`
	// BridgeEnabled records BRIDGE_ENABLED (default true via
	// defaultRawConfig): whether bridge-mode traffic is accepted alongside
	// the AUTH_TOKENS pool (hybrid mode).
	BridgeEnabled bool `json:"BRIDGE_ENABLED"`
	// BridgeIdleEvict is the sliding-TTL string for idle bridge-entry
	// eviction (BRIDGE_IDLE_EVICT; default "72h", zero-tolerant → 72h).
	BridgeIdleEvict                  string                  `json:"BRIDGE_IDLE_EVICT"`
	MaxSpendPerDay                   *int                    `json:"MAX_SPEND_PER_DAY"`
	IdleRotationTimeout              string                  `json:"IDLE_ROTATION_TIMEOUT"`
	SafeMode                         bool                    `json:"SAFE_MODE"`
	SessionIdleEnd                   string                  `json:"SESSION_IDLE_END"`
	ModelsHideUnavailable            bool                    `json:"MODELS_HIDE_UNAVAILABLE"`
	ModelsAllow                      modelsAllowList         `json:"MODELS_ALLOW"`
	CORSAllowedOrigin                string                  `json:"CORS_ALLOWED_ORIGIN"`
	RequestJitter                    string                  `json:"REQUEST_JITTER"`
	CLIVersion                       string                  `json:"CLI_VERSION"`
	ModelAliases                     string                  `json:"MODEL_ALIASES"`
	TransientRetries                 *int                    `json:"TRANSIENT_RETRIES"`
	SessionPersist                   bool                    `json:"SESSION_PERSIST"`
	SessionStateFile                 string                  `json:"SESSION_STATE_FILE"`
	HTTP2Upstream                    bool                    `json:"HTTP2_UPSTREAM"`
	SessionCreateMaxParallelGlobal   *int                    `json:"SESSION_CREATE_MAX_PARALLEL_GLOBAL"`
	SessionCreateMaxParallelPerModel *int                    `json:"SESSION_CREATE_MAX_PARALLEL_PER_MODEL"`
	RunFinishQueueSize               *int                    `json:"RUN_FINISH_QUEUE_SIZE"`
	RunFinishInlineTimeout           string                  `json:"RUN_FINISH_INLINE_TIMEOUT"`
	RunsDrainQueueCap                *int                    `json:"RUNS_DRAIN_QUEUE_CAP"`
	RunsDrainTTL                     string                  `json:"RUNS_DRAIN_TTL"`
	SessionReAdmitLead               string                  `json:"SESSION_RE_ADMIT_LEAD"`
	SessionProbeCacheTTL             string                  `json:"SESSION_PROBE_CACHE_TTL"`
	ModelUnavailableCacheTTL         string                  `json:"MODEL_UNAVAILABLE_CACHE_TTL"`
	QuotaFallbackModels              quotaFallbackModelsList `json:"QUOTA_FALLBACK_MODELS"`
	WebhookURL                       string                  `json:"WEBHOOK_URL"`
	FallbackAfter                    string                  `json:"FALLBACK_AFTER_MS"`
	FallbackModels                   string                  `json:"FALLBACK_MODEL"`
	AdoptCLISession                  bool                    `json:"ADOPT_CLI_SESSION"`
	MaturityEnabled                  bool                    `json:"MATURITY_ENABLED"`
	MaturityDryRun                   bool                    `json:"MATURITY_DRY_RUN"`
	MaturityTouchModel               string                  `json:"MATURITY_TOUCH_MODEL"`
	MaturityTargetDays               *int                    `json:"MATURITY_TARGET_DAYS"`
	QuotaAutoProbe                   bool                    `json:"QUOTA_AUTO_PROBE"`
	BurstBalanceEnabled              bool                    `json:"BURST_BALANCE_ENABLED"`
	BurstWindow                      string                  `json:"BURST_WINDOW"`
	BurstThreshold                   *int                    `json:"BURST_THRESHOLD"`
	BurstMaxTokens                   *int                    `json:"BURST_MAX_TOKENS"`
	WaitingRoomChain                 bool                    `json:"WAITING_ROOM_CHAIN"`
	RateLimitPerIP                   *float64                `json:"RATE_LIMIT_PER_IP"`
	RateLimitBurst                   *int                    `json:"RATE_LIMIT_BURST"`
	TokenRotation                    string                  `json:"TOKEN_ROTATION"`
	RateLimitFailover                *bool                   `json:"RATE_LIMIT_FAILOVER"`
	ModelLocks                       string                  `json:"MODEL_LOCKS"`
	DashboardEnabled                 bool                    `json:"DASHBOARD_ENABLED"`
	DashboardRequireLogin            bool                    `json:"DASHBOARD_REQUIRE_LOGIN"`
	CompressPrompt                   string                  `json:"COMPRESS_PROMPT"`
	CacheControlInjection            string                  `json:"CACHE_CONTROL_INJECTION"`
	ReasoningInContent               string                  `json:"REASONING_IN_CONTENT"`
}

// modelsAllowList is the raw MODELS_ALLOW value. The README documents list
// values as comma-separated in env and arrays in JSON, but operators write
// JSON configs by hand — accepting a plain comma-separated string here too
// avoids a hard parse error for the most natural single-value form. Both
// shapes are normalized to a comma-separated string; Config.ModelsAllow
// parses it with splitList in Load.
type modelsAllowList string

func (m *modelsAllowList) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*m = modelsAllowList(s)
		return nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err != nil {
		return fmt.Errorf("MODELS_ALLOW must be a comma-separated string or an array of strings, got: %s", data)
	}
	*m = modelsAllowList(strings.Join(arr, ","))
	return nil
}

// quotaFallbackModelsList is the raw QUOTA_FALLBACK_MODELS value (issue #155):
// env is a comma-separated list of k=v pairs, JSON may be a string or a map.
type quotaFallbackModelsList string

func (q *quotaFallbackModelsList) UnmarshalJSON(data []byte) error {
	var v string
	if err := json.Unmarshal(data, &v); err == nil {
		*q = quotaFallbackModelsList(v)
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err == nil {
		parts := make([]string, 0, len(m))
		for k, val := range m {
			parts = append(parts, k+"="+val)
		}
		sort.Strings(parts)
		*q = quotaFallbackModelsList(strings.Join(parts, ","))
		return nil
	}
	return fmt.Errorf("QUOTA_FALLBACK_MODELS must be a comma-separated k=v string or a map, got: %s", data)
}

func defaultRawConfig() rawConfig {
	return rawConfig{
		ListenAddr:                       "127.0.0.1:3457",       // loopback by default (PRD §3); containers set LISTEN_ADDR=:3457
		UpstreamBaseURL:                  "https://codebuff.com", // normalized to www.
		RotationInterval:                 "6h",
		RequestTimeout:                   "15m",
		HTTPReadTimeout:                  "60s",
		SessionCallTimeout:               "30s",
		TokenRotation:                    "drain",
		RateLimitFailover:                new(true),
		CostMode:                         "free",
		RegistryRefresh:                  "6h",
		MaxSpendPerDay:                   nil,   // 0 = unlimited advisory spend ceiling (never enforced)
		IdleRotationTimeout:              "",    // "" = disabled (unset → SAFE_MODE preset may fill)
		BridgeEnabled:                    true,  // hybrid by default: AUTH_TOKENS + bridge relay share one instance
		BridgeIdleEvict:                  "72h", // sliding-TTL for idle bridge-entry eviction
		SafeMode:                         true,  // anti-ban presets on by default; set SAFE_MODE=false to disable
		SessionIdleEnd:                   "",    // "" = disabled (opt-in: ending a session forces a fresh admission when the user returns)
		DashboardEnabled:                 true,  // dashboard on by default; set DASHBOARD_ENABLED=false to disable
		DashboardRequireLogin:            true,  // require login on by default; set DASHBOARD_REQUIRE_LOGIN=false to disable
		LogAccess:                        true,
		DevToolsEnabled:                  false,       // per-request access lines on by default; LOG_ACCESS=false disables them
		LogRingSize:                      ptrInt(500), // dashboard log viewer ring capacity (T19)
		CORSAllowedOrigin:                "*",         // browser clients reach /v1/* cross-origin by default
		RequestJitter:                    "",          // "" = disabled (unset → SAFE_MODE preset may fill)
		CLIVersion:                       "0.10.7",
		TransientRetries:                 nil,  // nil = 1 (one retry after a transient transport failure; 0 disables)
		SessionPersist:                   true, // session persistence on by default: restart resumes unexpired sessions
		SessionStateFile:                 ".freebuff-session-state.json",
		HTTP2Upstream:                    true,                         // h2 ALPN matches real browsers (reference proxy-freebuff USE_HTTP2 default '1'); HTTP2_UPSTREAM=false forces h1 (#51)
		SessionCreateMaxParallelGlobal:   ptrInt(128),                  // #86: concurrent session admissions cap
		SessionCreateMaxParallelPerModel: ptrInt(32),                   // #86: per-model concurrent admissions cap
		RunFinishQueueSize:               ptrInt(64),                   // #90: bounded deferred-FINISH queue
		RunFinishInlineTimeout:           "250ms",                      // #90: inline FINISH fallback bound
		RunsDrainQueueCap:                ptrInt(64),                   // #55: draining-runs list cap
		RunsDrainTTL:                     "10m",                        // #55: draining-runs TTL eviction
		SessionReAdmitLead:               "60s",                        // #99: pre-emptive re-admit lead
		SessionProbeCacheTTL:             "15s",                        // #60: admission probe cache TTL
		FallbackAfter:                    "0",                          // #100: queue-wait fallback threshold (ms); 0 = disabled by default
		BurstBalanceEnabled:              false,                        // burst spreading off by default (ADR-0023); true spreads a per-model burst across BURST_MAX_TOKENS accounts
		BurstWindow:                      "1m",                         // sliding window for counting same-model admissions toward BURST_THRESHOLD
		BurstThreshold:                   ptrInt(20),                   // same-model admissions inside BURST_WINDOW that trip spreading for that model
		BurstMaxTokens:                   ptrInt(2),                    // distinct accounts one model's burst spreads across (minimum 2)
		MaturityEnabled:                  true,                         // streak-maturity automation on by default; dry-run probes prove schedule before live touches
		MaturityDryRun:                   true,                         // maturity touches probe only until the operator proves the schedule
		MaturityTouchModel:               "deepseek/deepseek-v4-flash", // unmetered default: never burns premium quota
		QuotaAutoProbe:                   true,                         // quota auto-probe scheduler on by default (ADR-0022); false restores pre-scheduler behavior
	}
}

// ptrInt returns a pointer to n for *int raw fields with a non-nil default.
func ptrInt(n int) *int { return &n }

// defaultFallbackModels returns the FALLBACK_MODEL defaults (issue #100):
// empty by default — no automatic model fallback on queue wait.
func defaultFallbackModels() map[string]string {
	return nil
}

// defaultQuotaFallbackModels returns the QUOTA_FALLBACK_MODELS defaults (issue #155, #183):
// empty by default — no automatic model fallback on quota exhaustion.
func defaultQuotaFallbackModels() map[string]string {
	return nil
}
