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
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"freebuff-proxy/internal/telemetry"
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
	LogRingSize           int
	MaxMessagesPerDay     int           // 0 = unlimited: per-token cap on successful chats per 24h
	MaxSpendPerDay        int64         // 0 = unlimited: ADVISORY per-token Pacific-day spend ceiling in ledger units (tokens from upstream usage blocks; issue #122). Never blocks — the upstream $ ceilings ($15 full / $5 limited / $0.50 restricted, compose by minimum, server-enforced) are the real gate. Surfaced as SpendLimit/SpendPct on /healthz so operator comparisons align with the Pacific-midnight reset.
	IdleRotationTimeout   time.Duration // 0 = disabled: pause rotation/refresh after this idle period
	SafeMode              bool          // true = apply recommended anti-ban safe defaults
	HybridMode            bool          // true = relay client tokens like bridge AND serve token-less requests from the pool
	ModelsHideUnavailable bool          // true = /v1/models prunes models marked unavailable (region/tier/quota)
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
	// FALLBACK_MODEL=model1=fallback1,model2=fallback2). Defaults (applied
	// only when FALLBACK_MODEL is unset): the premium free-catalog rows
	// (deepseek-v4-pro, gpt-5.6-luna, minimax-m3, claude-fable-5,
	// glm-5.2) → deepseek/deepseek-v4-flash, mirroring the CLI's hero flip
	// to the unlimited flash model once the premium daily pool runs out
	// (reference freebuff-models.ts getRecommendedFreebuffModelId).
	FallbackModels map[string]string
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
	// PreferMaxModels maps standard model IDs to their -max extended context
	// variants (PREFER_MAX_MODELS).
	PreferMaxModels bool
	// AccessTier is the account access tier the proxy assumes for -max model
	// upgrades (ACCESS_TIER, or learned at runtime from the upstream session
	// probe/admission response's accessTier field): "full" (default when
	// empty) or "limited". A limited tier may only reach -max variants that
	// are explicitly in registry.LimitedTierModels — none today — so the
	// registry keeps the base model instead of tripping upstream 403
	// free_mode_invalid_agent_model (the -max agent roots require full
	// access). Empty = unknown = treated as full.
	AccessTier string
	// AccessTierExplicit records that AccessTier came from a configured
	// source (ACCESS_TIER env/.env/JSON), so runtime session-probe
	// observations never override the operator's explicit choice.
	AccessTierExplicit bool
	// ProvisionedModels is the set of model ids upstream actually
	// provisioned for the pooled token(s), learned from the session
	// probe/admission response's rateLimitsByModel map (keys = model ids).
	// The -max upgrade gate (registry.maxUpgradeAllowed) refuses variants
	// absent from this set: upstream provisions -max roots "per-account"
	// rather than for every full-tier token, so a full tier with only base
	// models provisioned would otherwise trip 403 free_mode_invalid_
	// agent_model on every upgraded request — the ban amplifier (issue
	// #140). Empty = unknown = the tier gate alone decides (historic
	// behavior). Never operator-set; JSON/env cannot populate it.
	ProvisionedModels map[string]bool `json:"-"`
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
// "hybrid" when HYBRID_MODE is set, "bridge" when no AUTH_TOKENS are
// configured, else "pooled". Hybrid wins over bridge so a hybrid config with
// zero tokens still reports hybrid (token-less requests 502 until a token is
// added, while client-token requests relay like bridge).
func (c Config) EffectiveMode() string {
	if c.HybridMode {
		return "hybrid"
	}
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
	LegacyActingUserID               string          `json:"USER_ID"`
	TLSFingerprint                   string          `json:"TLS_FINGERPRINT"`
	RegistryRefresh                  string          `json:"REGISTRY_REFRESH"`
	DebugDump                        bool            `json:"DEBUG_DUMP"`
	LogFile                          string          `json:"LOG_FILE"`
	LogLevel                         string          `json:"LOG_LEVEL"`
	LogFormat                        string          `json:"LOG_FORMAT"`
	LogAccess                        bool            `json:"LOG_ACCESS"`
	LogRingSize                      *int            `json:"LOG_RING_SIZE"`
	MaxMessagesPerDay                *int            `json:"MAX_MESSAGES_PER_DAY"`
	MaxSpendPerDay                   *int            `json:"MAX_SPEND_PER_DAY"`
	IdleRotationTimeout              string          `json:"IDLE_ROTATION_TIMEOUT"`
	SafeMode                         bool            `json:"SAFE_MODE"`
	HybridMode                       bool            `json:"HYBRID_MODE"`
	ModelsHideUnavailable            bool            `json:"MODELS_HIDE_UNAVAILABLE"`
	ModelsAllow                      modelsAllowList `json:"MODELS_ALLOW"`
	CORSAllowedOrigin                string          `json:"CORS_ALLOWED_ORIGIN"`
	RequestJitter                    string          `json:"REQUEST_JITTER"`
	CLIVersion                       string          `json:"CLI_VERSION"`
	ModelAliases                     string          `json:"MODEL_ALIASES"`
	TransientRetries                 *int            `json:"TRANSIENT_RETRIES"`
	SessionPersist                   bool            `json:"SESSION_PERSIST"`
	SessionStateFile                 string          `json:"SESSION_STATE_FILE"`
	HTTP2Upstream                    bool            `json:"HTTP2_UPSTREAM"`
	SessionCreateMaxParallelGlobal   *int            `json:"SESSION_CREATE_MAX_PARALLEL_GLOBAL"`
	SessionCreateMaxParallelPerModel *int            `json:"SESSION_CREATE_MAX_PARALLEL_PER_MODEL"`
	RunFinishQueueSize               *int            `json:"RUN_FINISH_QUEUE_SIZE"`
	RunFinishInlineTimeout           string          `json:"RUN_FINISH_INLINE_TIMEOUT"`
	RunsDrainQueueCap                *int            `json:"RUNS_DRAIN_QUEUE_CAP"`
	RunsDrainTTL                     string          `json:"RUNS_DRAIN_TTL"`
	SessionReAdmitLead               string          `json:"SESSION_RE_ADMIT_LEAD"`
	SessionProbeCacheTTL             string          `json:"SESSION_PROBE_CACHE_TTL"`
	WebhookURL                       string          `json:"WEBHOOK_URL"`
	FallbackAfter                    string          `json:"FALLBACK_AFTER_MS"`
	FallbackModels                   string          `json:"FALLBACK_MODEL"`
	AdoptCLISession                  bool            `json:"ADOPT_CLI_SESSION"`
	WaitingRoomChain                 bool            `json:"WAITING_ROOM_CHAIN"`
	RateLimitPerIP                   *float64        `json:"RATE_LIMIT_PER_IP"`
	RateLimitBurst                   *int            `json:"RATE_LIMIT_BURST"`
	PreferMaxModels                  bool            `json:"PREFER_MAX_MODELS"`
	AccessTier                       string          `json:"ACCESS_TIER"`
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
		LogAccess:                        true,        // per-request access lines on by default; LOG_ACCESS=false disables them
		LogRingSize:                      ptrInt(500), // dashboard log viewer ring capacity (T19)
		HybridMode:                       false,       // relay client tokens AND serve the pool (off by default)
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
// the premium free-catalog rows fall back to the always-available flash
// model once their queue wait passes FALLBACK_AFTER_MS — the proxy-side
// mirror of the CLI hero flip to the unlimited flash model when the premium
// daily pool runs out (reference freebuff-models.ts
// getRecommendedFreebuffModelId). Capacity-gated rows (e.g.
// meta/muse-spark-*) are not in the free catalog, so operators extend via
// FALLBACK_MODEL themselves; the reference MUSE_SPARK_FALLBACK_MODEL_ID
// pattern (→ deepseek-v4-pro) is documented in the README.
func defaultFallbackModels() map[string]string {
	return map[string]string{
		"deepseek/deepseek-v4-pro": "deepseek/deepseek-v4-flash",
		"openai/gpt-5.6-luna":      "deepseek/deepseek-v4-flash",
		"minimax/minimax-m3":       "deepseek/deepseek-v4-flash",
		"anthropic/claude-fable-5": "deepseek/deepseek-v4-flash",
		"z-ai/glm-5.2":             "deepseek/deepseek-v4-flash",
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

// Load resolves configuration from the optional JSON file at configPath
// ("" skips the file), the optional ./.env file (when present), and
// environment overrides, then validates it. Precedence, lowest to highest:
// built-in defaults < JSON file (-config) < ./.env < real environment
// (.env is an environment file, so it follows the README rule that the
// environment overrides the JSON config).
func Load(configPath string) (Config, error) {
	raw, err := loadRaw(configPath)
	if err != nil {
		return Config{}, err
	}
	envFileUsed := ResolveEnvFile()
	if err := applyDotenv(&raw, envFileUsed); err != nil {
		return Config{}, err
	}

	overrideString(&raw.ListenAddr, "LISTEN_ADDR")
	overrideString(&raw.UpstreamBaseURL, "UPSTREAM_BASE_URL")
	// AUTH_TOKENS is presence-sensitive: an empty value in the real
	// environment is an explicit bridge-mode choice (systemd/Docker unit
	// files set AUTH_TOKENS= to force bridge mode). Unlike other keys, an
	// empty value must not be skipped — it records presence so CLI
	// auto-discovery cannot refill the pool, mirroring applyDotenv's
	// AUTH_TOKENS handling for .env. When the variable is absent, the
	// JSON/.env value (if any) stands unchanged.
	if v, ok := os.LookupEnv("AUTH_TOKENS"); ok {
		raw.AuthTokens = splitList(v)
		raw.AuthTokensSet = true
	}
	overrideString(&raw.RotationInterval, "ROTATION_INTERVAL")
	overrideString(&raw.RequestTimeout, "REQUEST_TIMEOUT")
	overrideString(&raw.SessionCallTimeout, "SESSION_CALL_TIMEOUT")
	overrideCSV(&raw.APIKeys, "API_KEYS")
	overrideString(&raw.AdminToken, "ADMIN_TOKEN")
	overrideString(&raw.CostMode, "COST_MODE")
	// ACTING_USER_ID / legacy USER_ID (#126): the alias is read from the
	// SAME env source as the primary, so a real-environment USER_ID beats a
	// lower-precedence .env/JSON ACTING_USER_ID instead of being silently
	// dropped; ACTING_USER_ID wins when both are set in one source.
	overrideStringAlias(&raw.ActingUserID, os.Getenv, "ACTING_USER_ID", "USER_ID")
	overrideString(&raw.TLSFingerprint, "TLS_FINGERPRINT")
	overrideString(&raw.RegistryRefresh, "REGISTRY_REFRESH")
	overrideBool(&raw.DebugDump, "DEBUG_DUMP")
	overrideString(&raw.LogFile, "LOG_FILE")
	overrideString(&raw.LogLevel, "LOG_LEVEL")
	overrideString(&raw.LogFormat, "LOG_FORMAT")
	overrideBool(&raw.LogAccess, "LOG_ACCESS")
	overrideInt(&raw.LogRingSize, "LOG_RING_SIZE")
	overrideInt(&raw.MaxMessagesPerDay, "MAX_MESSAGES_PER_DAY")
	overrideInt(&raw.MaxSpendPerDay, "MAX_SPEND_PER_DAY")
	overrideString(&raw.IdleRotationTimeout, "IDLE_ROTATION_TIMEOUT")
	overrideBool(&raw.SafeMode, "SAFE_MODE")
	overrideBool(&raw.HybridMode, "HYBRID_MODE")
	overrideBool(&raw.ModelsHideUnavailable, "MODELS_HIDE_UNAVAILABLE")
	overrideString((*string)(&raw.ModelsAllow), "MODELS_ALLOW")
	overrideString(&raw.CORSAllowedOrigin, "CORS_ALLOWED_ORIGIN")
	overrideString(&raw.RequestJitter, "REQUEST_JITTER")
	overrideString(&raw.CLIVersion, "CLI_VERSION")
	overrideString(&raw.ModelAliases, "MODEL_ALIASES")
	overrideInt(&raw.TransientRetries, "TRANSIENT_RETRIES")
	overrideBool(&raw.SessionPersist, "SESSION_PERSIST")
	overrideString(&raw.SessionStateFile, "SESSION_STATE_FILE")
	overrideBool(&raw.HTTP2Upstream, "HTTP2_UPSTREAM")
	overrideInt(&raw.SessionCreateMaxParallelGlobal, "SESSION_CREATE_MAX_PARALLEL_GLOBAL")
	overrideInt(&raw.SessionCreateMaxParallelPerModel, "SESSION_CREATE_MAX_PARALLEL_PER_MODEL")
	overrideInt(&raw.RunFinishQueueSize, "RUN_FINISH_QUEUE_SIZE")
	overrideString(&raw.RunFinishInlineTimeout, "RUN_FINISH_INLINE_TIMEOUT")
	overrideInt(&raw.RunsDrainQueueCap, "RUNS_DRAIN_QUEUE_CAP")
	overrideString(&raw.RunsDrainTTL, "RUNS_DRAIN_TTL")
	overrideString(&raw.SessionReAdmitLead, "SESSION_RE_ADMIT_LEAD")
	overrideString(&raw.SessionProbeCacheTTL, "SESSION_PROBE_CACHE_TTL")
	overrideString(&raw.WebhookURL, "WEBHOOK_URL")
	overrideString(&raw.FallbackAfter, "FALLBACK_AFTER_MS")
	overrideString(&raw.FallbackModels, "FALLBACK_MODEL")
	overrideBool(&raw.AdoptCLISession, "ADOPT_CLI_SESSION")
	overrideBool(&raw.WaitingRoomChain, "WAITING_ROOM_CHAIN")
	overrideFloat(&raw.RateLimitPerIP, "RATE_LIMIT_PER_IP")
	overrideInt(&raw.RateLimitBurst, "RATE_LIMIT_BURST")
	overrideBool(&raw.PreferMaxModels, "PREFER_MAX_MODELS")
	overrideString(&raw.AccessTier, "ACCESS_TIER")

	parseDuration := func(raw, name string) (time.Duration, error) {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return 0, fmt.Errorf("parse %s: %w", name, err)
		}
		return d, nil
	}

	rotationInterval, err := parseDuration(raw.RotationInterval, "ROTATION_INTERVAL")
	if err != nil {
		return Config{}, err
	}
	requestTimeout, err := parseDuration(raw.RequestTimeout, "REQUEST_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	sessionCallTimeout, err := parseDuration(raw.SessionCallTimeout, "SESSION_CALL_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	registryRefresh, err := parseDuration(raw.RegistryRefresh, "REGISTRY_REFRESH")
	if err != nil {
		return Config{}, err
	}
	// RUN_FINISH_INLINE_TIMEOUT / RUNS_DRAIN_TTL / SESSION_RE_ADMIT_LEAD /
	// SESSION_PROBE_CACHE_TTL are zero-tolerant durations: "" or "0" fall
	// back to the documented default (a zero inline timeout would make the
	// inline fallback useless; a zero re-admit lead would spin a re-admit
	// on every request).
	runFinishInlineTimeout := 250 * time.Millisecond
	if v := strings.TrimSpace(raw.RunFinishInlineTimeout); v != "" {
		runFinishInlineTimeout, err = parseDuration(v, "RUN_FINISH_INLINE_TIMEOUT")
		if err != nil {
			return Config{}, err
		}
		if runFinishInlineTimeout <= 0 {
			runFinishInlineTimeout = 250 * time.Millisecond
		}
	}
	runsDrainTTL := 10 * time.Minute
	if v := strings.TrimSpace(raw.RunsDrainTTL); v != "" {
		runsDrainTTL, err = parseDuration(v, "RUNS_DRAIN_TTL")
		if err != nil {
			return Config{}, err
		}
		if runsDrainTTL <= 0 {
			runsDrainTTL = 10 * time.Minute
		}
	}
	sessionReAdmitLead := 60 * time.Second
	if v := strings.TrimSpace(raw.SessionReAdmitLead); v != "" {
		sessionReAdmitLead, err = parseDuration(v, "SESSION_RE_ADMIT_LEAD")
		if err != nil {
			return Config{}, err
		}
		if sessionReAdmitLead <= 0 {
			sessionReAdmitLead = 60 * time.Second
		}
	}
	sessionProbeCacheTTL := 15 * time.Second
	if v := strings.TrimSpace(raw.SessionProbeCacheTTL); v != "" {
		sessionProbeCacheTTL, err = parseDuration(v, "SESSION_PROBE_CACHE_TTL")
		if err != nil {
			return Config{}, err
		}
		if sessionProbeCacheTTL <= 0 {
			sessionProbeCacheTTL = 15 * time.Second
		}
	}
	sessionCreateMaxGlobal := 128
	if raw.SessionCreateMaxParallelGlobal != nil {
		sessionCreateMaxGlobal = *raw.SessionCreateMaxParallelGlobal
	}
	sessionCreateMaxPerModel := 32
	if raw.SessionCreateMaxParallelPerModel != nil {
		sessionCreateMaxPerModel = *raw.SessionCreateMaxParallelPerModel
	}
	runFinishQueueSize := 64
	if raw.RunFinishQueueSize != nil {
		runFinishQueueSize = *raw.RunFinishQueueSize
	}
	runsDrainQueueCap := 64
	if raw.RunsDrainQueueCap != nil {
		runsDrainQueueCap = *raw.RunsDrainQueueCap
	}
	// IDLE_ROTATION_TIMEOUT is zero-tolerant: "" or "0" both mean disabled.
	// idleRotationSet distinguishes "explicitly disabled" from "not
	// configured" so the SafeMode preset only fills truly unset knobs.
	idleRotationSet := strings.TrimSpace(raw.IdleRotationTimeout) != ""
	idleRotationTimeout := time.Duration(0)
	if idleRotationSet && strings.TrimSpace(raw.IdleRotationTimeout) != "0" {
		idleRotationTimeout, err = parseDuration(raw.IdleRotationTimeout, "IDLE_ROTATION_TIMEOUT")
		if err != nil {
			return Config{}, err
		}
	}
	requestJitterSet := strings.TrimSpace(raw.RequestJitter) != ""
	requestJitter := time.Duration(0)
	if requestJitterSet {
		requestJitter, err = parseDuration(raw.RequestJitter, "REQUEST_JITTER")
		if err != nil {
			return Config{}, err
		}
	}
	upstreamBaseURL, err := normalizeUpstreamBaseURL(raw.UpstreamBaseURL)
	if err != nil {
		return Config{}, err
	}

	// MAX_MESSAGES_PER_DAY defaults to 0 (unlimited): the upstream 429 lock
	// is the real quota enforcement — rate-limited tokens are locked in
	// memory until the reset window, so no local cap is needed to prevent
	// spam traffic. Explicit values always win.
	maxMessagesPerDay := 0
	if raw.MaxMessagesPerDay != nil {
		maxMessagesPerDay = *raw.MaxMessagesPerDay
	}

	// MAX_SPEND_PER_DAY (issue #122): advisory per-token Pacific-day spend
	// ceiling in ledger units, default 0 (unlimited). Deliberately NOT
	// enforced — the upstream $ ceilings are server-side and the proxy
	// cannot know the account's restricted cohort; surfaced as
	// SpendLimit/SpendPct on /healthz.
	maxSpendPerDay := int64(0)
	if raw.MaxSpendPerDay != nil {
		maxSpendPerDay = int64(*raw.MaxSpendPerDay)
	}

	// TRANSIENT_RETRIES: nil defaults to 1 (one additional attempt after a
	// transient transport failure); an explicit 0 disables retries.
	transientRetries := 1
	if raw.TransientRetries != nil {
		transientRetries = *raw.TransientRetries
	}

	// LOG_RING_SIZE: nil (unset/empty) defaults to 500; an explicit value
	// must stay within 50..5000 (validated in Validate).
	logRingSize := 500
	// RATE_LIMIT_PER_IP / RATE_LIMIT_BURST (issue #137): per-source-IP rate
	// limiter to protect upstream from bursts and spam. 0 = disabled.
	rateLimitPerIP := 0.0
	if raw.RateLimitPerIP != nil {
		rateLimitPerIP = *raw.RateLimitPerIP
	}
	rateLimitBurst := 0
	if raw.RateLimitBurst != nil {
		rateLimitBurst = *raw.RateLimitBurst
	}
	if raw.LogRingSize != nil {
		logRingSize = *raw.LogRingSize
	}

	// FALLBACK_AFTER_MS (issue #100): milliseconds, ""/0 = disabled. Any
	// parse failure fails the load — a typo silently disabling model
	// fallback would be worse than surfacing it.
	fallbackAfter := time.Duration(0)
	if v := strings.TrimSpace(raw.FallbackAfter); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("parse FALLBACK_AFTER_MS: %w", err)
		}
		if ms < 0 {
			return Config{}, errors.New("FALLBACK_AFTER_MS cannot be negative (0 disables model fallback)")
		}
		fallbackAfter = time.Duration(ms) * time.Millisecond
	}

	// MODEL_ALIASES defaults (issue #42): when the operator has not set any
	// aliases, common OpenAI/Anthropic/DeepSeek client names map to the
	// closest FreeBuff free-catalog model so a stock client works out of
	// the box. An explicit (even empty) value never gets the defaults.
	modelAliases := parseMap(raw.ModelAliases)
	if len(modelAliases) == 0 {
		for alias, real := range defaultModelAliases {
			modelAliases[alias] = real
		}
	}

	// FALLBACK_MODEL defaults (issue #100): when unset, the premium free-
	// catalog rows fall back to the always-available flash model once their
	// queue wait passes FALLBACK_AFTER_MS (mirrors the CLI hero flip,
	// reference freebuff-models.ts getRecommendedFreebuffModelId: premium
	// exhausted → unlimited flash). Operators extend with their own
	// capacity-gated rows (e.g. meta/muse-spark-* → deepseek-v4-pro per the
	// reference MUSE_SPARK_FALLBACK_MODEL_ID).
	fallbackModels := parseMap(raw.FallbackModels)
	if len(fallbackModels) == 0 {
		fallbackModels = defaultFallbackModels()
	}

	// Backward-compat (#126): a JSON config carrying the pre-rename USER_ID
	// key still works when no ACTING_USER_ID source (env/.env/JSON) set a
	// value. Weakest source — env and .env override it via the aliases above.
	if raw.ActingUserID == "" {
		raw.ActingUserID = raw.LegacyActingUserID
	}

	// LOG_FORMAT default: empty means the text format (the historic output).
	logFormat := strings.TrimSpace(raw.LogFormat)
	if logFormat == "" {
		logFormat = "text"
	}

	cfg := Config{
		ListenAddr:                       strings.TrimSpace(raw.ListenAddr),
		UpstreamBaseURL:                  upstreamBaseURL,
		AuthTokens:                       dedupeStrings(raw.AuthTokens),
		RotationInterval:                 rotationInterval,
		RequestTimeout:                   requestTimeout,
		SessionCallTimeout:               sessionCallTimeout,
		APIKeys:                          dedupeStrings(raw.APIKeys),
		AdminToken:                       strings.TrimSpace(raw.AdminToken),
		HTTP2Upstream:                    raw.HTTP2Upstream,
		CostMode:                         strings.TrimSpace(raw.CostMode),
		ActingUserID:                     strings.TrimSpace(raw.ActingUserID),
		TLSFingerprint:                   strings.TrimSpace(raw.TLSFingerprint),
		RegistryRefresh:                  registryRefresh,
		DebugDump:                        raw.DebugDump,
		LogFile:                          strings.TrimSpace(raw.LogFile),
		LogLevel:                         strings.TrimSpace(raw.LogLevel),
		LogFormat:                        logFormat,
		LogAccess:                        raw.LogAccess,
		LogRingSize:                      logRingSize,
		MaxMessagesPerDay:                maxMessagesPerDay,
		MaxSpendPerDay:                   maxSpendPerDay,
		IdleRotationTimeout:              idleRotationTimeout,
		SafeMode:                         raw.SafeMode,
		HybridMode:                       raw.HybridMode,
		ModelsHideUnavailable:            raw.ModelsHideUnavailable,
		ModelsAllow:                      splitList(string(raw.ModelsAllow)),
		CORSAllowedOrigin:                strings.TrimSpace(raw.CORSAllowedOrigin),
		RequestJitter:                    requestJitter,
		CLIVersion:                       strings.TrimSpace(raw.CLIVersion),
		ModelAliases:                     modelAliases,
		TransientRetries:                 transientRetries,
		SessionPersist:                   raw.SessionPersist,
		SessionStateFile:                 strings.TrimSpace(raw.SessionStateFile),
		SessionCreateMaxParallelGlobal:   sessionCreateMaxGlobal,
		SessionCreateMaxParallelPerModel: sessionCreateMaxPerModel,
		RunFinishQueueSize:               runFinishQueueSize,
		RunFinishInlineTimeout:           runFinishInlineTimeout,
		RunsDrainQueueCap:                runsDrainQueueCap,
		RunsDrainTTL:                     runsDrainTTL,
		SessionReAdmitLead:               sessionReAdmitLead,
		SessionProbeCacheTTL:             sessionProbeCacheTTL,
		WebhookURL:                       strings.TrimSpace(raw.WebhookURL),
		FallbackAfter:                    fallbackAfter,
		FallbackModels:                   fallbackModels,
		AdoptCLISession:                  raw.AdoptCLISession,
		WaitingRoomChain:                 raw.WaitingRoomChain,
		RateLimitPerIP:                   rateLimitPerIP,
		RateLimitBurst:                   rateLimitBurst,
		PreferMaxModels:                  raw.PreferMaxModels,
		AccessTier:                       strings.TrimSpace(raw.AccessTier),
		AccessTierExplicit:               strings.TrimSpace(raw.AccessTier) != "",
		EnvFile:                          envFileUsed,
	}

	// Auto-discover CLI token if no AUTH_TOKENS were explicitly configured
	// and AUTO_DISCOVER_TOKEN is not disabled. ADOPT_CLI_SESSION (issue
	// #97) also opts into discovery: the operator explicitly asked to run
	// like the CLI, so AUTO_DISCOVER_TOKEN=false must not silently leave the
	// pool empty.
	autoDiscover := true
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("AUTO_DISCOVER_TOKEN"))); v == "false" || v == "0" || v == "off" || v == "no" {
		autoDiscover = false
	}
	if (autoDiscover || cfg.AdoptCLISession) && len(cfg.AuthTokens) == 0 && !raw.AuthTokensSet {
		if token, email, srcPath, ok := discoverCLIToken(); ok {
			cfg.AuthTokens = []string{token}
			cfg.DiscoveredSource = srcPath
			cfg.DiscoveredEmail = email
			// An operator running without AUTH_TOKENS intends bridge mode;
			// auto-discovery silently flipping to pooled mode is surprising,
			// so warn loudly and name the off switch.
			slog.Warn("auto-discovery filled empty AUTH_TOKENS from CLI login: bridge mode switched to pooled mode",
				"file", srcPath,
				"email", email,
				"hint", "set AUTO_DISCOVER_TOKEN=false to disable auto-discovery")
		}
	}

	// SafeMode presets: when SAFE_MODE=true, apply recommended defaults for
	// account-safety knobs that were NOT explicitly configured. Explicit
	// "0"/disabled values always win (IDLE_ROTATION_TIMEOUT=0 or
	// REQUEST_JITTER=0 stay disabled). MAX_MESSAGES_PER_DAY is never preset:
	// it defaults to 0 (unlimited); the upstream 429 lock enforces quotas.
	if cfg.SafeMode {
		if !idleRotationSet && cfg.IdleRotationTimeout == 0 {
			cfg.IdleRotationTimeout = 30 * time.Minute
		}
		if !requestJitterSet && cfg.RequestJitter == 0 {
			cfg.RequestJitter = 2 * time.Second
		}
		if cfg.TLSFingerprint == "" {
			cfg.TLSFingerprint = "auto"
		}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// discoverCLIToken auto-discovers FreeBuff credentials from official CLI login files.
func discoverCLIToken() (string, string, string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", "", "", false
	}
	candidates := []string{
		filepath.Join(home, ".config", "manicode", "credentials.json"),
		filepath.Join(home, ".config", "codebuff", "credentials.json"),
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// Strip a leading UTF-8 BOM (Windows credential writers can add one)
		// or json.Unmarshal fails and auto-discovery silently skips the file.
		data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
		var parsed map[string]any
		if err := json.Unmarshal(data, &parsed); err != nil {
			continue
		}
		acct, ok := parsed["default"].(map[string]any)
		if !ok {
			for _, v := range parsed {
				if m, ok := v.(map[string]any); ok && m["authToken"] != nil {
					acct = m
					break
				}
			}
		}
		if acct != nil {
			token, _ := acct["authToken"].(string)
			email, _ := acct["email"].(string)
			token = strings.TrimSpace(token)
			if token != "" {
				return token, email, path, true
			}
		}
	}
	return "", "", "", false
}

// Validate checks the resolved configuration. It must be called before use.
// Includes actionable fix suggestions for common misconfigurations (#16).
func (c Config) Validate() error {
	switch {
	case c.ListenAddr == "":
		return errors.New("LISTEN_ADDR cannot be empty")
	case !strings.Contains(c.ListenAddr, ":"):
		return fmt.Errorf("LISTEN_ADDR %q missing port separator ':' (did you mean '127.0.0.1:3457' or ':3457'?)", c.ListenAddr)
	case c.UpstreamBaseURL == "":
		return errors.New("UPSTREAM_BASE_URL cannot be empty")
	case c.RotationInterval <= 0:
		return errors.New("ROTATION_INTERVAL must be greater than zero")
	case c.RequestTimeout <= 0:
		return errors.New("REQUEST_TIMEOUT must be greater than zero")
	case c.SessionCallTimeout <= 0:
		return errors.New("SESSION_CALL_TIMEOUT must be greater than zero")
	case c.RegistryRefresh <= 0:
		return errors.New("REGISTRY_REFRESH must be greater than zero")
	case c.RequestJitter < 0:
		return errors.New("REQUEST_JITTER cannot be negative")
	case c.TransientRetries < 0:
		return errors.New("TRANSIENT_RETRIES cannot be negative")
	case c.SessionCreateMaxParallelGlobal < 0 || c.SessionCreateMaxParallelPerModel < 0:
		return errors.New("SESSION_CREATE_MAX_PARALLEL_GLOBAL/PER_MODEL cannot be negative (0 = unlimited)")
	case c.RunFinishQueueSize < 0 || c.RunsDrainQueueCap < 0:
		return errors.New("RUN_FINISH_QUEUE_SIZE/RUNS_DRAIN_QUEUE_CAP cannot be negative (0 = default)")
	case c.SessionPersist && strings.TrimSpace(c.SessionStateFile) == "":
		return errors.New("SESSION_STATE_FILE cannot be empty when SESSION_PERSIST is enabled")
	case c.CostMode != "" && c.CostMode != "free":
		return errors.New(`COST_MODE must be "free" or unset -- any other value (e.g. a typo) routes requests as PAID and fresh free accounts get 402 "Out of credits"`)
	case c.MaxMessagesPerDay < 0:
		return errors.New("MAX_MESSAGES_PER_DAY cannot be negative")
	case c.MaxSpendPerDay < 0:
		return errors.New("MAX_SPEND_PER_DAY cannot be negative")
	case c.LogRingSize != 0 && (c.LogRingSize < 50 || c.LogRingSize > 5000):
		return errors.New("LOG_RING_SIZE must be between 50 and 5000 (default 500)")
	case c.RateLimitPerIP < 0:
		return errors.New("RATE_LIMIT_PER_IP cannot be negative")
	case c.RateLimitBurst < 0:
		return errors.New("RATE_LIMIT_BURST cannot be negative")
	}

	if c.WebhookURL != "" {
		u, err := url.Parse(c.WebhookURL)
		if err != nil {
			return fmt.Errorf("WEBHOOK_URL %q is not a valid URL: %w", c.WebhookURL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("WEBHOOK_URL %q must be an http(s) URL", c.WebhookURL)
		}
		if u.Host == "" {
			return fmt.Errorf("WEBHOOK_URL %q has no host", c.WebhookURL)
		}
	}

	_, portStr, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return fmt.Errorf("LISTEN_ADDR %q is invalid: %w", c.ListenAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("LISTEN_ADDR %q has invalid port %q (must be an integer in 1-65535)", c.ListenAddr, portStr)
	}

	// HYBRID_MODE deliberately has no constraint: hybrid with zero
	// AUTH_TOKENS is legal — the dashboard warns that token-less requests
	// will 502 until a token is added, while client-token requests relay
	// like bridge mode.

	for i, tok := range c.AuthTokens {
		if strings.HasPrefix(strings.ToLower(tok), "bearer ") {
			return fmt.Errorf("AUTH_TOKENS token #%d starts with 'Bearer ' prefix -- remove 'Bearer ' (the proxy adds it upstream automatically)", i+1)
		}
		if tok == "cb_xxx" || tok == "cb_yyy" || tok == "YOUR_TOKEN_HERE" {
			return fmt.Errorf("AUTH_TOKENS token #%d is a placeholder %q -- replace with a real FreeBuff token (run: freebuff)", i+1, tok)
		}
	}

	if c.TLSFingerprint != "" {
		switch strings.ToLower(c.TLSFingerprint) {
		case "chrome120", "chrome126", "safari17", "safari18", "firefox120", "firefox128", "edge126", "random", "auto":
			// valid
		default:
			return fmt.Errorf("TLS_FINGERPRINT %q must be one of: chrome120, chrome126, safari17, safari18, firefox120, firefox128, edge126, random, auto", c.TLSFingerprint)
		}
	}

	if c.LogLevel != "" {
		if _, ok := telemetry.ParseLevel(c.LogLevel); !ok {
			return fmt.Errorf("LOG_LEVEL %q must be one of: debug, info, warn, error, trace", c.LogLevel)
		}
	}
	switch c.LogFormat {
	case "", "text", "json":
		// "" never survives From (it defaults to "text"), accepted for
		// direct Config construction.
	default:
		return fmt.Errorf("LOG_FORMAT %q must be one of: text, json", c.LogFormat)
	}

	u, err := url.Parse(c.UpstreamBaseURL)
	if err != nil {
		return fmt.Errorf("UPSTREAM_BASE_URL %q is not a valid URL: %w", c.UpstreamBaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("UPSTREAM_BASE_URL %q must be an http(s) URL", c.UpstreamBaseURL)
	}
	if u.Host == "" {
		return fmt.Errorf("UPSTREAM_BASE_URL %q has no host", c.UpstreamBaseURL)
	}
	return nil
}

// normalizeUpstreamBaseURL trims a trailing slash, requires an http(s) URL,
// and rewrites the host codebuff.com to www.codebuff.com (the API only serves
// the www host; the bare host redirects).
func normalizeUpstreamBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(strings.TrimRight(raw, "/"))
	if raw == "" {
		return "", errors.New("UPSTREAM_BASE_URL cannot be empty")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("UPSTREAM_BASE_URL %q is not a valid URL: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("UPSTREAM_BASE_URL %q must be an http(s) URL", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("UPSTREAM_BASE_URL %q has no host", raw)
	}

	if strings.EqualFold(parsed.Host, "codebuff.com") {
		parsed.Host = "www.codebuff.com"
	}

	return strings.TrimRight(parsed.String(), "/"), nil
}

func loadRaw(configPath string) (rawConfig, error) {
	cfg := defaultRawConfig()

	if configPath != "" {
		path, err := filepath.Abs(configPath)
		if err != nil {
			return rawConfig{}, fmt.Errorf("resolve config path: %w", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return rawConfig{}, fmt.Errorf("read config file: %w", err)
		}
		// Strip a leading UTF-8 BOM (Windows editors/PowerShell writers add
		// one) or json.Unmarshal fails with "invalid character '\ufeff'".
		// Every other file reader in the package (discoverCLIToken,
		// parseDotenv) already strips it; this was the missed case (B3).
		data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
		if err := json.Unmarshal(data, &cfg); err != nil {
			return rawConfig{}, fmt.Errorf("parse config file: %w", err)
		}
		// A non-nil AuthTokens after unmarshal means the JSON key was present
		// ([] is an explicit empty list; absent leaves it nil).
		cfg.AuthTokensSet = cfg.AuthTokens != nil
	}

	return cfg, nil
}

// applyDotenv overlays KEY=VALUE pairs from the resolved .env file (when
// present) onto raw, so a local .env works like the JSON config file. path
// is the resolved env file ("" = none found, from ResolveEnvFile). A
// missing file is fine; any other read error fails the load. Real
// environment variables are applied afterwards and therefore always win.
func applyDotenv(raw *rawConfig, path string) error {
	if path == "" {
		return nil
	}
	vals, err := readDotenv(path)
	if err != nil || vals == nil {
		return err
	}
	get := func(name string) string { return vals[name] }
	// An empty AUTH_TOKENS= line in .env is an explicit bridge-mode choice
	// (the dashboard mode switch persists exactly this): record presence so
	// auto-discovery cannot refill it, AND clear whatever the JSON config
	// provided (the empty value must beat the JSON list). Unlike other keys,
	// AUTH_TOKENS must NOT skip empty overrides.
	if v, ok := vals["AUTH_TOKENS"]; ok {
		raw.AuthTokens = splitList(v)
		raw.AuthTokensSet = true
	}
	overrideStringFrom(&raw.ListenAddr, get, "LISTEN_ADDR")
	overrideStringFrom(&raw.UpstreamBaseURL, get, "UPSTREAM_BASE_URL")
	overrideStringFrom(&raw.RotationInterval, get, "ROTATION_INTERVAL")
	overrideStringFrom(&raw.RequestTimeout, get, "REQUEST_TIMEOUT")
	overrideStringFrom(&raw.SessionCallTimeout, get, "SESSION_CALL_TIMEOUT")
	overrideCSVFrom(&raw.APIKeys, get, "API_KEYS")
	overrideStringFrom(&raw.AdminToken, get, "ADMIN_TOKEN")
	overrideStringFrom(&raw.CostMode, get, "COST_MODE")
	// ACTING_USER_ID / legacy USER_ID (#126), same-source: a .env USER_ID
	// beats a JSON ACTING_USER_ID (dotenv outranks JSON), ACTING_USER_ID
	// wins when both are in the .env.
	overrideStringAlias(&raw.ActingUserID, get, "ACTING_USER_ID", "USER_ID")
	overrideStringFrom(&raw.TLSFingerprint, get, "TLS_FINGERPRINT")
	overrideStringFrom(&raw.RegistryRefresh, get, "REGISTRY_REFRESH")
	overrideBoolFrom(&raw.DebugDump, get, "DEBUG_DUMP")
	overrideStringFrom(&raw.LogFile, get, "LOG_FILE")
	overrideStringFrom(&raw.LogLevel, get, "LOG_LEVEL")
	overrideStringFrom(&raw.LogFormat, get, "LOG_FORMAT")
	overrideBoolFrom(&raw.LogAccess, get, "LOG_ACCESS")
	overrideIntFrom(&raw.LogRingSize, get, "LOG_RING_SIZE")
	overrideIntFrom(&raw.MaxMessagesPerDay, get, "MAX_MESSAGES_PER_DAY")
	overrideIntFrom(&raw.MaxSpendPerDay, get, "MAX_SPEND_PER_DAY")
	overrideStringFrom(&raw.IdleRotationTimeout, get, "IDLE_ROTATION_TIMEOUT")
	// The remaining keys mirror the real-environment override set in Load.
	// AUTO_DISCOVER_TOKEN is intentionally env-only (it controls the .env
	// read itself, so honoring it from .env would be circular).
	overrideBoolFrom(&raw.SafeMode, get, "SAFE_MODE")
	overrideBoolFrom(&raw.HybridMode, get, "HYBRID_MODE")
	overrideBoolFrom(&raw.ModelsHideUnavailable, get, "MODELS_HIDE_UNAVAILABLE")
	overrideStringFrom((*string)(&raw.ModelsAllow), get, "MODELS_ALLOW")
	overrideStringFrom(&raw.RequestJitter, get, "REQUEST_JITTER")
	overrideStringFrom(&raw.CLIVersion, get, "CLI_VERSION")
	overrideStringFrom(&raw.ModelAliases, get, "MODEL_ALIASES")
	overrideIntFrom(&raw.TransientRetries, get, "TRANSIENT_RETRIES")
	overrideBoolFrom(&raw.SessionPersist, get, "SESSION_PERSIST")
	overrideStringFrom(&raw.SessionStateFile, get, "SESSION_STATE_FILE")
	overrideBoolFrom(&raw.HTTP2Upstream, get, "HTTP2_UPSTREAM")
	overrideIntFrom(&raw.SessionCreateMaxParallelGlobal, get, "SESSION_CREATE_MAX_PARALLEL_GLOBAL")
	overrideIntFrom(&raw.SessionCreateMaxParallelPerModel, get, "SESSION_CREATE_MAX_PARALLEL_PER_MODEL")
	overrideIntFrom(&raw.RunFinishQueueSize, get, "RUN_FINISH_QUEUE_SIZE")
	overrideStringFrom(&raw.RunFinishInlineTimeout, get, "RUN_FINISH_INLINE_TIMEOUT")
	overrideIntFrom(&raw.RunsDrainQueueCap, get, "RUNS_DRAIN_QUEUE_CAP")
	overrideStringFrom(&raw.RunsDrainTTL, get, "RUNS_DRAIN_TTL")
	overrideStringFrom(&raw.SessionReAdmitLead, get, "SESSION_RE_ADMIT_LEAD")
	overrideStringFrom(&raw.SessionProbeCacheTTL, get, "SESSION_PROBE_CACHE_TTL")
	overrideStringFrom(&raw.WebhookURL, get, "WEBHOOK_URL")
	overrideStringFrom(&raw.FallbackAfter, get, "FALLBACK_AFTER_MS")
	overrideStringFrom(&raw.FallbackModels, get, "FALLBACK_MODEL")
	overrideBoolFrom(&raw.AdoptCLISession, get, "ADOPT_CLI_SESSION")
	overrideBoolFrom(&raw.WaitingRoomChain, get, "WAITING_ROOM_CHAIN")
	overrideFloatFrom(&raw.RateLimitPerIP, get, "RATE_LIMIT_PER_IP")
	overrideIntFrom(&raw.RateLimitBurst, get, "RATE_LIMIT_BURST")
	overrideBoolFrom(&raw.PreferMaxModels, get, "PREFER_MAX_MODELS")
	overrideStringFrom(&raw.AccessTier, get, "ACCESS_TIER")
	return nil
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

func overrideString(target *string, envName string) {
	overrideStringFrom(target, os.Getenv, envName)
}

func overrideStringFrom(target *string, get func(string) string, envName string) {
	if value := strings.TrimSpace(get(envName)); value != "" {
		*target = value
	}
}

// overrideStringAlias overrides target from source get, preferring the
// primary env name and falling back to the legacy alias name when the
// primary is empty at THIS source. Both names are read from the same
// source, so cross-source precedence (JSON < .env < env) holds for either
// name (#126): a higher-precedence USER_ID beats a lower-precedence
// ACTING_USER_ID instead of being silently dropped, while ACTING_USER_ID
// wins when both appear in one source.
func overrideStringAlias(target *string, get func(string) string, primary, alias string) {
	value := strings.TrimSpace(get(primary))
	if value == "" {
		value = strings.TrimSpace(get(alias))
	}
	if value != "" {
		*target = value
	}
}

func overrideCSV(target *[]string, envName string) {
	overrideCSVFrom(target, os.Getenv, envName)
}

func overrideCSVFrom(target *[]string, get func(string) string, envName string) {
	if value := strings.TrimSpace(get(envName)); value != "" {
		*target = splitList(value)
	}
}

// overrideBool sets target from DEBUG_DUMP-style env vars; unset or
// unrecognized values leave the file/default value untouched.
func overrideBool(target *bool, envName string) {
	overrideBoolFrom(target, os.Getenv, envName)
}

func overrideBoolFrom(target *bool, get func(string) string, envName string) {
	switch strings.ToLower(strings.TrimSpace(get(envName))) {
	case "1", "true", "yes", "on":
		*target = true
	case "0", "false", "no", "off":
		*target = false
	}
}

// overrideInt sets target from MAX_MESSAGES_PER_DAY-style env vars; unset or
// unparseable values leave the file/default value untouched.
func overrideInt(target **int, envName string) {
	overrideIntFrom(target, os.Getenv, envName)
}

func overrideIntFrom(target **int, get func(string) string, envName string) {
	if value := strings.TrimSpace(get(envName)); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			*target = &parsed
		}
	}
}

// overrideFloat sets target from RATE_LIMIT_PER_IP-style env vars; unset or
// unparseable values leave the file/default value untouched.
func overrideFloat(target **float64, envName string) {
	overrideFloatFrom(target, os.Getenv, envName)
}

func overrideFloatFrom(target **float64, get func(string) string, envName string) {
	if value := strings.TrimSpace(get(envName)); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			*target = &parsed
		}
	}
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
