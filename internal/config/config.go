// Package config loads and validates freebuff-proxy configuration.
//
// Precedence: JSON config file (optional) < environment variables. Every key
// in the JSON file mirrors its environment variable name; values set in the
// environment always win. Auth tokens and API keys are comma-separated lists
// in the environment and JSON arrays in the file.

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Config is the fully-resolved, validated runtime configuration.
type Config struct {
	ListenAddr         string
	UpstreamBaseURL    string
	AuthTokens         []string
	RotationInterval   time.Duration
	RequestTimeout     time.Duration
	SessionCallTimeout time.Duration
	APIKeys            []string
	AdminToken         string // bearer token required for POST /admin/reload ("" = unauthenticated in default deployments)
	HTTP2Upstream      bool   // true = negotiate HTTP/2 with the upstream so the ALPN matches real browsers (HTTP2_UPSTREAM); false forces HTTP/1.1 (#51)
	CostMode           string // "" (omit) or "free"; A/B pending, PRD §8
	// ActingUserID is the optional FreeBuff account id sent as
	// x-freebuff-acting-user-id (ACTING_USER_ID; empty = header omitted).
	// BAN RISK: the official CLI sends the account's OWN id here, derived
	// from GET /api/v1/me (sdk/src/run.ts:649-658), and the server honors
	// the header only for the FreeBuff Web service account
	// (common/src/constants/freebuff-models.ts:1180-1183). Any value other
	// than the token's own account id impersonates another user — a flag.
	// The only safe value is the token's own account id. (True CLI parity —
	// auto-deriving each token's own id once via GET /api/v1/me — is
	// deferred; see the gap analysis item 24.)
	ActingUserID    string
	TLSFingerprint  string // "" (plain Go transport) | chrome120 | chrome126 | safari17 | safari18 | firefox120 | firefox128 | edge126 | random | auto
	RegistryRefresh time.Duration
	DebugDump       bool
	LogFile         string
	LogLevel        string // "" (use -v/default) or debug|info|warn|error|trace
	LogFormat       string // "text" (default) or "json"
	LogAccess       bool   // true = per-request access log lines (LOG_ACCESS; default true, an empty .env line keeps it enabled)
	// LogRingSize is the bounded in-memory log ring capacity behind the
	// dashboard log viewer (LOG_RING_SIZE; default 500, validated 50..5000).
	LogRingSize       int
	MaxMessagesPerDay int // 0 = unlimited: per-token cap on successful chats per 24h
	// BridgeDailyLimit is the global daily chat cap across ALL bridge-mode
	// entries (BRIDGE_DAILY_LIMIT; 0 = unlimited). Enforced in AcquireBridge
	// before the per-entry check so a flood of distinct client tokens cannot
	// collectively exceed the operator's budget.
	BridgeDailyLimit      int
	MaxSpendPerDay        int64         // 0 = unlimited: ADVISORY per-token Pacific-day spend ceiling in ledger units (tokens from upstream usage blocks; issue #122). Never blocks — the upstream $ ceilings ($15 full / $5 limited / $0.50 restricted, compose by minimum, server-enforced) are the real gate. Surfaced as SpendLimit/SpendPct on /healthz so operator comparisons align with the Pacific-midnight reset.
	IdleRotationTimeout   time.Duration // 0 = disabled: pause rotation/refresh after this idle period
	SafeMode              bool          // true = apply recommended anti-ban safe defaults
	ModelsHideUnavailable bool          // true = /v1/models prunes models marked unavailable (region/quota/lock)
	// ModelsAllow is the operator-set model allowlist (MODELS_ALLOW,
	// comma-separated). When non-empty, /v1/models lists only the allowed
	// ids and chat/messages/responses requests whose RESOLVED model (after
	// registry alias resolution and -max upgrades) is not listed are
	// rejected with 404 model_not_found ("model not allowed by
	// MODELS_ALLOW"). Empty = no restriction.
	ModelsAllow       []string
	CORSAllowedOrigin string            // Access-Control-Allow-Origin for /v1/* responses (CORS_ALLOWED_ORIGIN; default "*")
	RequestJitter     time.Duration     // random delay range [0, RequestJitter) before upstream chat calls
	CLIVersion        string            // upstream CLI version string (default: 0.10.7)
	ModelAliases      map[string]string // map model alias -> real model ID (#25)
	TransientRetries  int               // max additional attempts after a transient transport failure (0 = disabled; default 1)
	SessionPersist    bool              // true = persist session state to disk so restart resumes unexpired sessions (SESSION_PERSIST)
	SessionStateFile  string            // path to the session state file (SESSION_STATE_FILE; default .freebuff-session-state.json)
	// SessionCreateMaxParallelGlobal / SessionCreateMaxParallelPerModel cap
	// concurrent in-flight session admissions (issue #86): the pool's create
	// gate returns 503 when a cap is hit instead of hammering upstream.
	// SESSION_CREATE_MAX_PARALLEL_GLOBAL default 128; _PER_MODEL default 32.
	SessionCreateMaxParallelGlobal   int
	SessionCreateMaxParallelPerModel int
	// RunFinishQueueSize is the bounded deferred-FINISH worker queue size
	// (issue #90, RUN_FINISH_QUEUE_SIZE default 64): rotated/drained runs
	// are FINISHed by a background worker; when the queue is full the caller
	// falls back to a synchronous FINISH bounded by RunFinishInlineTimeout.
	RunFinishQueueSize int
	// RunFinishInlineTimeout bounds the synchronous inline FINISH fallback
	// when the finish queue is full (issue #90, RUN_FINISH_INLINE_TIMEOUT
	// default 250ms).
	RunFinishInlineTimeout time.Duration
	// RunsDrainQueueCap / RunsDrainTTL bound the draining-runs list (issue
	// #55, RUNS_DRAIN_QUEUE_CAP default 64, RUNS_DRAIN_TTL default 10m):
	// entries beyond the cap or older than the TTL are force-dropped with a
	// warn log (their upstream FINISH is best-effort anyway).
	RunsDrainQueueCap int
	RunsDrainTTL      time.Duration
	// SessionReAdmitLead is how long before session expiry a pre-emptive
	// async re-admit is triggered (issue #99, SESSION_RE_ADMIT_LEAD default
	// 60s): the request rides the old session while the refresh runs in the
	// background; the next request gets the new instance.
	SessionReAdmitLead time.Duration
	// SessionProbeCacheTTL is how long the last successful session state is
	// reused before a fresh upstream poll (issue #60, SESSION_PROBE_CACHE_TTL
	// default 15s): session poll GETs within the TTL are skipped.
	SessionProbeCacheTTL time.Duration
	// ModelUnavailableCacheTTL is how long a model_unavailable admission
	// refusal is remembered per model (issue #158,
	// MODEL_UNAVAILABLE_CACHE_TTL default 1h): off-window models
	// short-circuit to the fallback within the TTL (or until the parsed
	// availability window re-opens, whichever is sooner) instead of burning
	// a 409 roundtrip per request.
	ModelUnavailableCacheTTL time.Duration
	// WebhookURL fires best-effort alert POSTs when the token pool is
	// exhausted or a token is classified banned (issue #48, WEBHOOK_URL;
	// empty = disabled). Payload: {"event":"pool_exhausted"|"token_banned",
	// ...}; at most one POST per event type per 5m; never blocks the
	// request path.
	WebhookURL string
	// FallbackAfter is the queue-wait threshold for model fallback (issue
	// #100, FALLBACK_AFTER_MS default 10000): when a request's acquire is
	// answered with a waiting-room/queue delay at least this long AND a
	// fallback model is configured for the requested model
	// (FallbackModels), the request is re-routed to the fallback model for
	// the same token instead of surfacing 503. 0 disables fallback.
	FallbackAfter time.Duration
	// FallbackModels maps a requested model to the model served instead
	// when the queue wait reaches FallbackAfter (issue #100,
	// only when FALLBACK_MODEL is unset): the daily premium free-catalog rows
	// (deepseek-v4-pro, gpt-5.6-luna) → deepseek/deepseek-v4-flash.
	// Referral-gated models (z-ai/glm-5.2) are handled via QUOTA_FALLBACK_MODELS.
	// The proxy path fires only when the pool surfaces a waiting-room/queue delay ≥ FallbackAfter
	// for the requested model (issue #100) — 429 quota exhaustion NEVER
	// falls back (anti-ban invariant §10). The premium→flash targets
	// mirror the CLI's getRecommendedFreebuffModelId hero pick; the
	// muse→deepseek-v4-pro target mirrors the upstream
	// MUSE_SPARK_FALLBACK_MODEL_ID.
	FallbackModels map[string]string
	// ScarceSessionModels lists the irreplaceable 1-session/day models the proxy
	// keeps alive for their full hour (SCARCE_SESSION_MODELS; comma-separated).
	// Default: ["deepseek/deepseek-v4-pro", "openai/gpt-5.6-luna"].
	ScarceSessionModels []string
	// QuotaFallbackModels maps a model to its fallback model when its session
	// quota is exhausted or unentitled (QUOTA_FALLBACK_MODELS; comma-separated k=v pairs).
	// Default: {"deepseek/deepseek-v4-flash": "mimo/mimo-v2.5", "z-ai/glm-5.2": "deepseek/deepseek-v4-flash"}.
	QuotaFallbackModels map[string]string
	// AdoptCLISession, when enabled (ADOPT_CLI_SESSION=false default),
	// makes the proxy behave like the official CLI for a single account:
	// with AUTH_TOKENS empty the token is sourced from
	// ~/.config/manicode/credentials.json (same file AUTO_DISCOVER_TOKEN
	// reads) and every session manager adopts the CLI's ACTIVE session
	// instance from freebuff-instance-owner.json (re-read before each
	// refresh); while the CLI process is alive a competing session is
	// never created (issue #97).
	AdoptCLISession bool
	// WaitingRoomChain, when enabled (WAITING_ROOM_CHAIN=false default),
	// fires the reference ad-chain + streak requests before the next
	// session create after an upstream 428 waiting_room_required (issue
	// #94(b), gated stub — best-effort, never blocks the request).
	WaitingRoomChain bool
	// RateLimitPerIP / RateLimitBurst cap client request rates per source IP
	// (issue #137): RATE_LIMIT_PER_IP default 0 (disabled; e.g. 20 req/s);
	// RATE_LIMIT_BURST default 0 (defaults to 2 * RateLimitPerIP).
	RateLimitPerIP float64
	RateLimitBurst int
	// DashboardEnabled controls whether the embedded admin web UI is served
	// (DASHBOARD_ENABLED; default true). Set to false to disable all /admin
	// routes.
	DashboardEnabled bool
	// EnvFile is the .env path actually loaded ("" when none existed).
	// Resolved via ResolveEnvFile (issue #39): ./.env in the working
	// directory wins; otherwise the platform config dir is tried.
	EnvFile          string
	DiscoveredSource string // auto-discovered credentials file path (if any)
	DiscoveredEmail  string // auto-discovered account email (if any)
}

// BridgeMode reports whether the proxy runs without any AUTH_TOKENS: every
// client supplies their own FreeBuff token per request (Authorization: Bearer
// or x-api-key), and the proxy relays with that token upstream.
func (c Config) BridgeMode() bool { return len(c.AuthTokens) == 0 }

// EffectiveMode reports the routing mode label for dashboards and healthz:
// "bridge" when no AUTH_TOKENS are configured, else "pooled".
func (c Config) EffectiveMode() string {
	if c.BridgeMode() {
		return "bridge"
	}
	return "pooled"
}

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
	APIKeys            []string `json:"API_KEYS"`
	AdminToken         string   `json:"ADMIN_TOKEN"`
	CostMode           string   `json:"COST_MODE"`
	ActingUserID       string   `json:"ACTING_USER_ID"`
	// LegacyActingUserID is the pre-rename JSON key (USER_ID) — merged at
	// the end of Load when no ACTING_USER_ID source set a value (#126).
	LegacyActingUserID string `json:"USER_ID"`
	TLSFingerprint     string `json:"TLS_FINGERPRINT"`
	RegistryRefresh    string `json:"REGISTRY_REFRESH"`
	DebugDump          bool   `json:"DEBUG_DUMP"`
	LogFile            string `json:"LOG_FILE"`
	LogLevel           string `json:"LOG_LEVEL"`
	LogFormat          string `json:"LOG_FORMAT"`
	LogAccess          bool   `json:"LOG_ACCESS"`
	LogRingSize        *int   `json:"LOG_RING_SIZE"`
	MaxMessagesPerDay  *int   `json:"MAX_MESSAGES_PER_DAY"`
	// BridgeDailyLimit is the global daily chat cap across all bridge
	// entries (BRIDGE_DAILY_LIMIT; 0 = unlimited).
	BridgeDailyLimit                 *int                    `json:"BRIDGE_DAILY_LIMIT"`
	MaxSpendPerDay                   *int                    `json:"MAX_SPEND_PER_DAY"`
	IdleRotationTimeout              string                  `json:"IDLE_ROTATION_TIMEOUT"`
	SafeMode                         bool                    `json:"SAFE_MODE"`
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
	ScarceSessionModels              scarceSessionModelsList `json:"SCARCE_SESSION_MODELS"`
	QuotaFallbackModels              quotaFallbackModelsList `json:"QUOTA_FALLBACK_MODELS"`
	WebhookURL                       string                  `json:"WEBHOOK_URL"`
	FallbackAfter                    string                  `json:"FALLBACK_AFTER_MS"`
	FallbackModels                   string                  `json:"FALLBACK_MODEL"`
	AdoptCLISession                  bool                    `json:"ADOPT_CLI_SESSION"`
	WaitingRoomChain                 bool                    `json:"WAITING_ROOM_CHAIN"`
	RateLimitPerIP                   *float64                `json:"RATE_LIMIT_PER_IP"`
	RateLimitBurst                   *int                    `json:"RATE_LIMIT_BURST"`
	DashboardEnabled                 bool                    `json:"DASHBOARD_ENABLED"`
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

// scarceSessionModelsList is the raw SCARCE_SESSION_MODELS value (issue #155):
// env is a comma-separated list, JSON may be a string or an array of strings.
type scarceSessionModelsList string

func (s *scarceSessionModelsList) UnmarshalJSON(data []byte) error {
	var v string
	if err := json.Unmarshal(data, &v); err == nil {
		*s = scarceSessionModelsList(v)
		return nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*s = scarceSessionModelsList(strings.Join(arr, ","))
		return nil
	}
	return fmt.Errorf("SCARCE_SESSION_MODELS must be a comma-separated string or an array of strings, got: %s", data)
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
		SessionCallTimeout:               "30s",
		RegistryRefresh:                  "6h",
		CostMode:                         "free", // free-tier mode; omission routes requests as PAID and fresh free accounts get 402 "Out of credits" (upstream check: cost_mode !== 'free' → billing)
		MaxMessagesPerDay:                nil,
		MaxSpendPerDay:                   nil,         // 0 = unlimited advisory spend ceiling (never enforced)
		IdleRotationTimeout:              "",          // "" = disabled (unset → SAFE_MODE preset may fill)
		SafeMode:                         true,        // anti-ban presets on by default; set SAFE_MODE=false to disable
		DashboardEnabled:                 true,        // dashboard on by default; set DASHBOARD_ENABLED=false to disable
		LogAccess:                        true,        // per-request access lines on by default; LOG_ACCESS=false disables them
		LogRingSize:                      ptrInt(500), // dashboard log viewer ring capacity (T19)
		CORSAllowedOrigin:                "*",         // browser clients reach /v1/* cross-origin by default
		RequestJitter:                    "",          // "" = disabled (unset → SAFE_MODE preset may fill)
		CLIVersion:                       "0.10.7",
		TransientRetries:                 nil,   // nil = 1 (one retry after a transient transport failure; 0 disables)
		SessionPersist:                   false, // opt-in: persist session state across restarts
		SessionStateFile:                 ".freebuff-session-state.json",
		HTTP2Upstream:                    true,        // h2 ALPN matches real browsers (reference proxy-freebuff USE_HTTP2 default '1'); HTTP2_UPSTREAM=false forces h1 (#51)
		SessionCreateMaxParallelGlobal:   ptrInt(128), // #86: concurrent session admissions cap
		SessionCreateMaxParallelPerModel: ptrInt(32),  // #86: per-model concurrent admissions cap
		RunFinishQueueSize:               ptrInt(64),  // #90: bounded deferred-FINISH queue
		RunFinishInlineTimeout:           "250ms",     // #90: inline FINISH fallback bound
		RunsDrainQueueCap:                ptrInt(64),  // #55: draining-runs list cap
		RunsDrainTTL:                     "10m",       // #55: draining-runs TTL eviction
		SessionReAdmitLead:               "60s",       // #99: pre-emptive re-admit lead
		SessionProbeCacheTTL:             "15s",       // #60: admission probe cache TTL
		FallbackAfter:                    "10000",     // #100: queue-wait fallback threshold (ms)
	}
}

// ptrInt returns a pointer to n for *int raw fields with a non-nil default.
func ptrInt(n int) *int { return &n }

// defaultModelAliases are applied when MODEL_ALIASES is unset (issue #42):
// common OpenAI/Anthropic/DeepSeek client model names map to the closest
// FreeBuff free-catalog model, so a stock client works out of the box.
// gpt-4o maps to deepseek-v4-pro (the strongest agentic catalog row,
// closest to GPT-4-class expectations); deepseek-chat maps to the fast
// flash row (the DeepSeek API's own chat alias); claude-3-5-sonnet maps to
// the Claude-line fable-5 row. An explicitly-set MODEL_ALIASES (even
// empty) suppresses all defaults.
var defaultModelAliases = map[string]string{
	"deepseek-chat":     "deepseek/deepseek-v4-flash",
	"gpt-4o":            "deepseek/deepseek-v4-pro",
	"claude-3-5-sonnet": "anthropic/claude-fable-5",
}

// defaultFallbackModels returns the FALLBACK_MODEL defaults (issue #100):
// the daily premium free-catalog rows (deepseek-v4-pro, gpt-5.6-luna) fall back
// to the always-available flash model once their queue wait passes FALLBACK_AFTER_MS
// (issue #189). Trigger is queue-wait ≥ FALLBACK_AFTER_MS only — never 429s.
func defaultFallbackModels() map[string]string {
	return map[string]string{
		"deepseek/deepseek-v4-pro": "deepseek/deepseek-v4-flash",
		"openai/gpt-5.6-luna":      "deepseek/deepseek-v4-flash",
	}
}

// defaultQuotaFallbackModels returns the QUOTA_FALLBACK_MODELS defaults (issue #155, #183):
// when a model's session quota is exhausted (all 5 premium sessions used for flash)
// or unentitled (referral-only GLM 5.2 on accounts with 0 referral credits),
// the proxy falls back to an available model (flash for GLM, mimo for flash).
func defaultQuotaFallbackModels() map[string]string {
	return map[string]string{
		"deepseek/deepseek-v4-flash": "mimo/mimo-v2.5",
		"z-ai/glm-5.2":               "deepseek/deepseek-v4-flash",
	}
}

// defaultScarceSessionModels returns the SCARCE_SESSION_MODELS defaults (issue #155):
// the 1-session/day irreplaceable models kept alive for their full 1 hour.
func defaultScarceSessionModels() []string {
	return []string{
		"deepseek/deepseek-v4-pro",
		"openai/gpt-5.6-luna",
	}
}

// EnvFileCandidates returns the ordered candidate paths for the .env file
// (issue #39). The working directory wins (./.env), matching the historic
// behavior and the README rule that cwd config is authoritative. When it
// does not exist, the platform config dir is tried:
//
//	linux:   $XDG_CONFIG_HOME/freebuff-proxy/.env → ~/.config/freebuff-proxy/.env
//	windows: %APPDATA%\freebuff-proxy\.env
//	darwin:  ~/Library/Application Support/freebuff-proxy/.env
func EnvFileCandidates() []string {
	candidates := []string{filepath.Join(".", ".env")}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return candidates
	}
	var dir string
	switch runtime.GOOS {
	case "windows":
		if appdata := os.Getenv("APPDATA"); strings.TrimSpace(appdata) != "" {
			dir = appdata
		} else {
			dir = filepath.Join(home, "AppData", "Roaming")
		}
	case "darwin":
		dir = filepath.Join(home, "Library", "Application Support")
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); strings.TrimSpace(xdg) != "" {
			dir = xdg
		} else {
			dir = filepath.Join(home, ".config")
		}
	}
	return append(candidates, filepath.Join(dir, "freebuff-proxy", ".env"))
}

// ResolveEnvFile returns the first EXISTING candidate from
// EnvFileCandidates ("./.env" when present in the working directory — cwd
// wins), or "" when no candidate exists. A directory at a candidate path is
// still returned: readDotenv then fails the load (a ./.env directory must
// not silently disable the env file, legacy behavior). The resolved path is
// recorded on Config.EnvFile by Load so the startup banner can name the
// file actually read (issue #39).
func ResolveEnvFile() string {
	for _, candidate := range EnvFileCandidates() {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// readDotenv parses a dotenv file: KEY=VALUE lines, blank lines and #
// comments skipped, surrounding whitespace trimmed. Quotes are only removed
// for a matching pair wrapping the value (an unmatched quote is kept as
// literal data); inline # comments are stripped from unquoted values but
// preserved inside quoted ones. Returns nil, nil when the file does not
// exist.
func readDotenv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parseDotenv(data), nil
}

// parseDotenv splits .env content into KEY=VALUE pairs using the same lenient
// rules as readDotenv: blank lines and # comments skipped, single/double
// quotes stripped, unquoted trailing # comments trimmed.
func parseDotenv(data []byte) map[string]string {
	out := make(map[string]string)
	// Strip a leading UTF-8 BOM: PowerShell WriteAllText with a BOM-less
	// encoding must not be the only safe writer — a BOM on the first line
	// would corrupt the first key into "\ufeffKEY".
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value = strings.TrimSpace(value)
		quoted := false
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
			if end := strings.IndexByte(value[1:], value[0]); end >= 0 {
				value = value[1 : 1+end]
				quoted = true
			}
		}
		if !quoted {
			if idx := strings.IndexByte(value, '#'); idx >= 0 {
				value = strings.TrimSpace(value[:idx])
			}
		}
		out[key] = value
	}
	return out
}

func splitList(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	return compactStrings(fields)
}
func parseMap(value string) map[string]string {
	out := make(map[string]string)
	if strings.TrimSpace(value) == "" {
		return out
	}
	pairs := splitList(value)
	for _, p := range pairs {
		var parts []string
		if strings.Contains(p, "=") {
			parts = strings.SplitN(p, "=", 2)
		} else if strings.Contains(p, ":") {
			parts = strings.SplitN(p, ":", 2)
		}
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			if k != "" && v != "" {
				out[k] = v
			}
		}
	}
	return out
}

func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range compactStrings(values) {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
