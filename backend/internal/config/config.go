// Package config loads and validates freebuff-proxy configuration.
//
// Precedence: JSON config file (optional) < environment variables. Every key
// in the JSON file mirrors its environment variable name; values set in the
// environment always win. Auth tokens and API keys are comma-separated lists
// in the environment and JSON arrays in the file.

package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved, validated runtime configuration.
type Config struct {
	ListenAddr            string
	UpstreamBaseURL       string
	AuthTokens            []string
	RotationInterval      time.Duration
	RequestTimeout        time.Duration
	SessionCallTimeout    time.Duration
	HTTPReadTimeout       time.Duration
	APIKeys               []string
	AdminToken            string // bearer token required for POST /admin/reload (defaults to "123456" when unset/empty)
	DashboardRequireLogin bool   // true = dashboard requires password authentication (DASHBOARD_REQUIRE_LOGIN); defaults to true
	HTTP2Upstream         bool   // true = negotiate HTTP/2 with the upstream so the ALPN matches real browsers (HTTP2_UPSTREAM); false forces HTTP/1.1 (#51)
	CostMode              string // "" (omit) or "free"; A/B pending, PRD §8
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
	ActingUserID string
	// AutoDiscoverToken records the effective AUTO_DISCOVER_TOKEN knob
	// (default true): process env wins, else the DB overlay, else enabled.
	// When false, an empty AUTH_TOKENS pool stays empty (bridge mode) and
	// the CLI-credential discovery hook never fires; ADOPT_CLI_SESSION can
	// still opt into discovery on its own.
	AutoDiscoverToken bool
	TLSFingerprint    string // "" (plain Go transport) | chrome120 | chrome126 | safari17 | safari18 | firefox120 | firefox128 | edge126 | random | auto
	RegistryRefresh   time.Duration
	DebugDump         bool
	DevToolsEnabled   bool
	LogFile           string
	LogLevel          string // "" (use -v/default) or debug|info|warn|error|trace
	LogFormat         string // "text" (default) or "json"
	LogAccess         bool   // true = per-request access log lines (LOG_ACCESS; default true, an empty .env line keeps it enabled)
	// LogRingSize is the bounded in-memory log ring capacity behind the
	// dashboard log viewer (LOG_RING_SIZE; default 500, validated 50..5000).
	LogRingSize       int
	MaxMessagesPerDay int // 0 = unlimited: per-token cap on successful chats per 24h
	// MaxRequestsPerDay is the per-token cap on successful chat requests in
	// the current Pacific day (MAX_REQUESTS_PER_DAY; default 1500, 0 =
	// unlimited). Enforced in acquire like the daily message cap; resets at
	// Pacific midnight — the same instant upstream rolls its daily quota
	// windows — so a locked token unlocks in sync with the official reset.
	MaxRequestsPerDay int
	// MaxRequestsPerMinute is the per-token cap on ADMITTED chat requests in
	// a rolling 60s window (MAX_REQUESTS_PER_MINUTE; default 30, 0 =
	// unlimited). Admission counting (not success-only) throttles the exact
	// request rate upstream observes — including retries that later fail —
	// keeping each account under abuse-detection burst patterns.
	MaxRequestsPerMinute int
	// BridgeDailyLimit is the global daily chat cap across ALL bridge-mode
	// entries (BRIDGE_DAILY_LIMIT; 0 = unlimited). Enforced in AcquireBridge
	// before the per-entry check so a flood of distinct client tokens cannot
	// collectively exceed the operator's budget.
	BridgeDailyLimit    int
	MaxSpendPerDay      int64         // 0 = unlimited: ADVISORY per-token Pacific-day spend ceiling in ledger units (tokens from upstream usage blocks; issue #122). Never blocks — the upstream $ ceilings ($15 full / $5 limited / $1 elevated [SG/CN since 2026-09, was $5] / $0.50 restricted, plus $7 full / $3 limited paid floor for flagged email/egress reasons since 6341ef3, compose by minimum, server-enforced; restricted reasons take a 2x HARD mid-session cut at FREEBUFF_SPEND_CEILING_HARD_MULTIPLIER) are the real gate. Surfaced as SpendLimit/SpendPct on /healthz so operator comparisons align with the Pacific-midnight reset.
	IdleRotationTimeout time.Duration // 0 = disabled: pause rotation/refresh after this idle period
	SessionIdleEnd      time.Duration // 0 = disabled: end upstream sessions after this idle period (SESSION_IDLE_END)
	// BridgeEnabled gates bridge-mode traffic when AUTH_TOKENS are configured
	// (BRIDGE_ENABLED; default true). When enabled alongside a token pool the
	// proxy runs in hybrid mode: a request whose credential matches an
	// API_KEYS entry uses the pool, every other credential is relayed upstream
	// as a bridge token. Set BRIDGE_ENABLED=0 for a locked-down pooled-only
	// instance (the pre-hybrid behavior).
	BridgeEnabled bool
	// BridgeIdleEvict is how long a bridge entry may sit unused before the
	// maintain loop FINISHes its runs, ends its upstream session, and drops it
	// from the cache (BRIDGE_IDLE_EVICT; default 72h, sliding TTL).
	BridgeIdleEvict       time.Duration
	SafeMode              bool // true = apply recommended anti-ban safe defaults
	ModelsHideUnavailable bool // true = /v1/models prunes models marked unavailable (region/quota/lock)
	// ModelsAllow is the operator-set model allowlist (MODELS_ALLOW,
	// comma-separated). When non-empty, /v1/models lists only the allowed
	// ids and chat/messages/responses requests whose RESOLVED model (after
	// registry alias resolution and -max upgrades) is not listed are
	// rejected with 404 model_not_found ("model not allowed by
	// MODELS_ALLOW"). Empty = no restriction.
	ModelsAllow       []string
	CORSAllowedOrigin string        // Access-Control-Allow-Origin for /v1/* responses (CORS_ALLOWED_ORIGIN; default "*")
	RequestJitter     time.Duration // random delay range [0, RequestJitter) before upstream chat calls
	CLIVersion        string        // upstream CLI version string (default: 0.10.7)
	TokenRotation     string        // "drain" (default) | "round_robin" | "least_used" | "random"
	RateLimitFailover bool          // true = automatically lease another token when an in-flight request encounters 429 rate limit (RATE_LIMIT_FAILOVER; default true)
	// ModelLocks pins pool slots to models (MODEL_LOCKS, issue #325):
	// map from AUTH_TOKENS slot index to the model ids that slot may
	// serve, e.g. {0: ["z-ai/glm-5.2"]}. Slots without an entry are
	// unlocked (today's behavior). Parsed at Load; malformed values
	// reject the config.
	ModelLocks       map[int][]string
	ModelAliases     map[string]string // map model alias -> real model ID (#25)
	TransientRetries int               // max additional attempts after a transient transport failure (0 = disabled; default 1)
	SessionPersist   bool              // true = persist session state to disk so restart resumes unexpired sessions (SESSION_PERSIST)
	SessionStateFile string            // path to the session state file (SESSION_STATE_FILE; default .freebuff-session-state.json)
	// SessionCreateMaxParallelGlobal / SessionCreateMaxParallelPerModel cap
	// concurrent in-flight session admissions (issue #86): the pool's create
	// gate returns 503 when a cap is hit instead of hammering upstream.
	// SESSION_CREATE_MAX_PARALLEL_GLOBAL default 128; _PER_MODEL default 32.
	SessionCreateMaxParallelGlobal   int
	SessionCreateMaxParallelPerModel int
	// ChatMaxInflightMetered / ChatMaxInflightUnmetered cap concurrent
	// in-flight chat requests per token, split by cost class (chat burst
	// queue): metered models (a priced Freebucks row) default 1, unmetered
	// models (no price row) default 3. The pool's chat gate queues excess
	// admissions instead of hammering upstream. 0 = unlimited, mirroring
	// the SESSION_CREATE_MAX_PARALLEL convention. Live-apply (atomic
	// pointer swap, no pool rebuild).
	ChatMaxInflightMetered   int
	ChatMaxInflightUnmetered int
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
	// FALLBACK_MODEL; default empty = no fallback). Operators opt in with
	// their own pairs (e.g. meta/muse-spark-1.2-contributor=openai/gpt-5.6-luna).
	// Referral-gated models (z-ai/glm-5.2) are handled via QUOTA_FALLBACK_MODELS.
	// The proxy path fires only when the pool surfaces a waiting-room/queue delay ≥ FallbackAfter
	// for the requested model (issue #100) — 429 quota exhaustion NEVER
	// falls back (anti-ban invariant §10).
	FallbackModels map[string]string
	// QuotaFallbackModels maps a model to its fallback model when its session
	// quota is exhausted or unentitled (QUOTA_FALLBACK_MODELS; comma-separated k=v pairs).
	// Default: empty — quota exhaustion surfaces an honest 429 with no automatic
	// fallback (kept per revamp decision: Hidden, cycle-validated, depth-guarded).
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
	// MaturityEnabled is the global kill-switch for streak-maturity automation
	// (MATURITY_ENABLED; default true). When false, no per-token maturity
	// touch ever fires regardless of per-token toggles. Default ON with
	// MATURITY_DRY_RUN=true so dry-run probes run by default while live
	// slot-claiming touches remain opt-in via dry-run toggle (docs/maturity-plan.md §4).
	MaturityEnabled bool
	// MaturityDryRun validates scheduler mechanics with zero side effects
	// (MATURITY_DRY_RUN; default true): touches run only the zero-cost
	// session probe and never claim a session slot. Turn it off only after
	// the dry-run log lines prove slots, skips and throttles behave.
	MaturityDryRun bool
	// MaturityTouchModel is the touch-model fallback for maturity touches
	// (MATURITY_TOUCH_MODEL; default "" = auto). "" (or "auto") resolves
	// the cheapest served unmetered catalog row per token; an explicit
	// provider/model id overrides auto. Either way the touch never spends
	// Freebucks (fail-closed on priced rows).
	MaturityTouchModel string
	// MaturityTargetDays is the default streak target for newly-enabled
	// tokens (MATURITY_TARGET_DAYS; default 7, valid 1..28). A token whose
	// streak reaches its target stops getting touches (automation disables
	// itself; enrollment never locks).
	MaturityTargetDays int
	// QuotaAutoProbe is the master switch for the activity-aware quota
	// prober (QUOTA_AUTO_PROBE; default true): a busy pool probes every
	// QuotaProbeActiveInterval, a warming pool every 5m, an idle pool once
	// plus the QuotaProbeIdleHeartbeat heartbeat. False restores
	// pre-scheduler behavior (manual probes only).
	QuotaAutoProbe bool
	// QuotaProbeActiveInterval is how often each pooled token is
	// quota-probed while the pool is busy (traffic <2m ago;
	// QUOTA_PROBE_ACTIVE_INTERVAL; default 60s). Zero-tolerant like
	// BURST_WINDOW: empty or non-positive values fall back to the default.
	QuotaProbeActiveInterval time.Duration
	// QuotaProbeIdleHeartbeat is how often each pooled token is
	// quota-probed while the pool sits idle (no traffic for 15m+;
	// QUOTA_PROBE_IDLE_HEARTBEAT; default 30m, also the ceiling for
	// 429-backoff doubling). Zero-tolerant like BURST_WINDOW: empty or
	// non-positive values fall back to the default.
	QuotaProbeIdleHeartbeat time.Duration
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
	// CompressPrompt enables optional prompt & context compression in the
	// request normalizer (COMPRESS_PROMPT; default off). Resolved once here
	// and passed to convert.Options so the per-chunk hot path never reads
	// the environment (issue #277).
	CompressPrompt bool
	// CacheControlInjection enables DeepSeek prompt-cache cache_control
	// injection (CACHE_CONTROL_INJECTION; default on). See CompressPrompt.
	CacheControlInjection bool
	// ReasoningInContent is the think-tag label used to fold reasoning into
	// message content for clients that do not render a reasoning channel
	// (REASONING_IN_CONTENT; default "" = off). See CompressPrompt.
	ReasoningInContent string
	// BurstBalanceEnabled opts into per-model burst spreading (ADR-0023,
	// BURST_BALANCE_ENABLED; default true): while one model's sliding-window
	// admissions exceed BURST_THRESHOLD, that model's selection switches to
	// least_used across at most BURST_MAX_TOKENS accounts. False restores
	// exact drain-only selection. Live-apply (atomic pointer swap, no pool
	// rebuild).
	BurstBalanceEnabled bool
	// BurstWindow is the sliding window for counting same-model admissions
	// toward BURST_THRESHOLD (BURST_WINDOW; default 1m). Zero = unset (the
	// pool normalizes to its default); negative is rejected in Validate.
	BurstWindow time.Duration
	// BurstThreshold is the same-model admission count inside BurstWindow
	// that trips spreading for that model (BURST_THRESHOLD; default 20).
	// Zero = unset; negative is rejected in Validate.
	BurstThreshold int
	// BurstMaxTokens caps the distinct accounts one model's burst spreads
	// across (BURST_MAX_TOKENS; default 2, minimum 2 — enforced in
	// Validate). Zero = unset.
	BurstMaxTokens int
}

// DefaultAdminToken is the default dashboard admin password ("123456") used when ADMIN_TOKEN is unconfigured or empty.
const DefaultAdminToken = "123456"

// IsDefaultAdminToken reports whether AdminToken matches the factory default credentials ("123456").
func (c *Config) IsDefaultAdminToken() bool {
	return c != nil && c.AdminToken == DefaultAdminToken
}

// RequireLogin reports whether the admin dashboard enforces login authentication.
// It is true when DashboardRequireLogin is true and AdminToken is non-empty.
func (c *Config) RequireLogin() bool {
	return c != nil && c.DashboardRequireLogin && c.AdminToken != ""
}

// BridgeMode reports whether the proxy runs without any AUTH_TOKENS: every
// client supplies their own FreeBuff token per request (Authorization: Bearer
// or x-api-key), and the proxy relays with that token upstream.
func (c Config) BridgeMode() bool { return len(c.AuthTokens) == 0 }

// HybridBridgeMode reports whether the proxy runs pooled AND bridge
// simultaneously: AUTH_TOKENS are configured AND BRIDGE_ENABLED (the
// default). A request whose credential matches an API_KEYS entry uses the
// pooled path; any other credential is relayed upstream as a bridge token.
func (c Config) HybridBridgeMode() bool {
	return len(c.AuthTokens) > 0 && c.BridgeEnabled
}

// EffectiveMode reports the routing mode label for dashboards and healthz:
// "bridge" when no AUTH_TOKENS are configured, "hybrid" when AUTH_TOKENS
// are set with BRIDGE_ENABLED (the default), else "pooled".
func (c Config) EffectiveMode() string {
	switch {
	case c.BridgeMode():
		return "bridge"
	case c.HybridBridgeMode():
		return "hybrid"
	default:
		return "pooled"
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

// parseModelLocks parses MODEL_LOCKS (issue #325): semicolon/newline
// separated slot entries, each "<slot-index>:<model>[,<model>...]", e.g.
// "0:z-ai/glm-5.2;1:upstage/solar-pro4,mimo/mimo-v2.5". Slot indexes
// address AUTH_TOKENS positions. Empty input yields nil (feature off).
// Malformed entries (missing colon, bad index, empty model list) are an
// error: a silently-ignored lock would route quota to the wrong account.
func parseModelLocks(value string) (map[int][]string, error) {
	locks := make(map[int][]string)
	entries := strings.FieldsFunc(value, func(r rune) bool {
		return r == ';' || r == '\n' || r == '\r'
	})
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		parts := strings.SplitN(e, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid MODEL_LOCKS entry %q (want <slot>:<model>[,<model>...])", e)
		}
		idx, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || idx < 0 {
			return nil, fmt.Errorf("invalid MODEL_LOCKS slot %q (want non-negative index)", strings.TrimSpace(parts[0]))
		}
		models := dedupeStrings(strings.Split(parts[1], ","))
		if len(models) == 0 {
			return nil, fmt.Errorf("invalid MODEL_LOCKS entry %q (no models listed)", e)
		}
		locks[idx] = models
	}
	if len(locks) == 0 {
		return nil, nil
	}
	return locks, nil
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
