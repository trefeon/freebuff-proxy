package config

// KeyDef describes one manageable configuration key for the admin settings
// UI. The catalog is the single description of the operator-facing env
// surface: the dashboard builds its form deterministically from it, so
// every key the config loader parses must be present here (enforced by
// TestCatalogCoversApplyDotenvKeys), and only documented keys may appear.
type KeyDef struct {
	// Key is the environment-variable name (also the JSON -config key).
	Key string `json:"key"`
	// Group is the UI section: "general", "pool", "quota", "upstream", or
	// "security". The dashboard renders groups in that fixed order.
	Group string `json:"group"`
	// Kind selects the form control: "bool", "select", "int", "text",
	// "secret", or "list".
	Kind string `json:"kind"`
	// Enum holds the select choices for kind "select" (empty otherwise).
	Enum []string `json:"enum,omitempty"`
	// Default is the documented default value ("" when unset).
	Default string `json:"default"`
	// RestartOnly marks keys that are snapshotted when the upstream clients,
	// session managers, run managers, and notifier are constructed at boot:
	// a config save reloads the in-memory config, but the live objects keep
	// the values they were built with until the process restarts. The set
	// exactly equals the server's restartOnlyConfigKeys (enforced by
	// TestRestartOnlyCatalogMatchesServer).
	RestartOnly bool   `json:"restart_only"`
	Secret      bool   `json:"secret"`
	Essential   bool   `json:"essential"`
	Hidden      bool   `json:"hidden,omitempty"`
	Description string `json:"description"`
}

// Catalog group names, in the order the UI renders them.
const (
	GroupGeneral  = "general"
	GroupPool     = "pool"
	GroupQuota    = "quota"
	GroupUpstream = "upstream"
	GroupSecurity = "security"
)

// catalogGroupOrder is the fixed UI section order (general, pool, quota,
// upstream, security) and the order the catalog is emitted in.
var catalogGroupOrder = []string{GroupGeneral, GroupPool, GroupQuota, GroupUpstream, GroupSecurity}

// Catalog returns the ordered configuration catalog: grouped by
// catalogGroupOrder, keys alphabetical within each group. The returned
// slice is a copy — callers may sort or annotate it without corrupting the
// source.
func Catalog() []KeyDef {
	return append([]KeyDef(nil), keyCatalog...)
}

// ConfigEnvKeys returns every environment variable config reads, derived
// from the catalog plus the legacy USER_ID alias (#126). It is the single
// source of truth for the operator env surface, so testutil can strip
// ambient proxy env vars without hand-maintaining a parallel list (issue
// #281).
func ConfigEnvKeys() []string {
	keys := make([]string, 0, len(keyCatalog))
	for _, def := range keyCatalog {
		keys = append(keys, def.Key)
	}
	keys = append(keys, "USER_ID")
	return keys
}

// keyCatalog is the single source of truth for the operator-facing env
// surface. Every key mirrored by applyDotenv and every env-only documented
// key must appear here exactly once (enforced by
// TestCatalogCoversApplyDotenvKeys).
var keyCatalog = []KeyDef{
	// ── general ──────────────────────────────────────────────────────────
	{Key: "ACTING_USER_ID", Group: GroupGeneral, Kind: "text", RestartOnly: true, Hidden: true,
		Default:     "",
		Description: `Optional FreeBuff account id sent as x-freebuff-acting-user-id. BAN RISK: only the token's own account id is safe (any other value impersonates another user). Pre-rename name USER_ID still works. Empty = header omitted.`},
	{Key: "AUTO_DISCOVER_TOKEN", Group: GroupGeneral, Kind: "bool", Hidden: true,
		Default:     "true",
		Description: `When AUTH_TOKENS is empty, read credentials from the official CLI login files (false disables). Environment-only: it controls whether .env is read at all, so a .env line is inert outside Docker.`},
	{Key: "CLI_VERSION", Group: GroupGeneral, Kind: "text", Hidden: true,
		Default:     "0.10.7",
		Description: `Upstream CLI version string; informational only — parsed and shown on the dashboard, no wire impact.`},
	{Key: "DASHBOARD_ENABLED", Group: GroupGeneral, Kind: "bool", RestartOnly: true, Hidden: true,
		Default:     "true",
		Description: `Serve the embedded admin dashboard at /admin (false disables all /admin routes with 404). Restart-only: /admin routes are registered once at boot and cannot be torn down at runtime.`},
	{Key: "DEBUG_DUMP", Group: GroupGeneral, Kind: "bool", RestartOnly: true, Hidden: true,
		Default:     "false",
		Description: `Persist redacted traffic dumps to ./dump/ (mode 0600, debugging only).`},
	{Key: "DEVTOOLS_ENABLED", Group: GroupGeneral, Kind: "bool", Hidden: true,
		Default:     "false",
		Description: `Show the Dev Tools page (batch chat, session spawner) in the admin dashboard. Default off: it is a manual testing surface that hammers /v1/*.`},
	{Key: "LISTEN_ADDR", Group: GroupGeneral, Kind: "text",
		Default:     "127.0.0.1:3457",
		Description: `Host and port to bind (loopback by default; containers set :3457 for all interfaces).`},
	{Key: "LOG_ACCESS", Group: GroupGeneral, Kind: "bool", Hidden: true,
		Default:     "true",
		Description: `Log one access line per HTTP request (healthz/metrics/OPTIONS are rate-limited to 1/min regardless).`},
	{Key: "LOG_FILE", Group: GroupGeneral, Kind: "text", Hidden: true,
		Default:     "",
		Description: `Append log lines to a file (e.g. ./logs/proxy.log); empty = stderr.`},
	{Key: "LOG_FORMAT", Group: GroupGeneral, Kind: "select", Enum: []string{"text", "json"}, Hidden: true,
		Default:     "text",
		Description: `Log format: text (key=value, colored) or json (one JSON object per line).`},
	{Key: "LOG_LEVEL", Group: GroupGeneral, Kind: "select", Enum: []string{"debug", "info", "warn", "error", "trace"}, Essential: true,
		Default:     "info",
		Description: `Log level (trace = wire-level bodies).`},
	{Key: "LOG_RING_SIZE", Group: GroupGeneral, Kind: "int", Hidden: true,
		Default:     "500",
		Description: `In-memory log ring capacity behind the dashboard log viewer (50-5000).`},
	{Key: "SAFE_MODE", Group: GroupGeneral, Kind: "bool", Essential: true,
		Default:     "true",
		Description: `Apply the anti-ban preset when a knob is left unset: idle-run rotation 30m, request jitter 200ms. TLS is CLI-faithful (plain Go/Bun baseline; set TLS_FINGERPRINT=auto for browser-evasion on datacenter IPs). Keep on.`},

	// ── pool ─────────────────────────────────────────────────────────────
	{Key: "ADOPT_CLI_SESSION", Group: GroupPool, Kind: "bool", RestartOnly: true, Hidden: true,
		Default:     "false",
		Description: `Adopt the upstream CLI's active session (from freebuff-instance-owner.json) instead of creating a new one; with empty AUTH_TOKENS the token is sourced from the CLI credentials file.`},
	{Key: "API_KEYS", Group: GroupPool, Kind: "list", Secret: true, Hidden: true,
		Default:     "",
		Description: `Comma-separated client keys required for /v1/* (empty = open; ignored in bridge mode). Managed in the Overview and Client API Keys section.`},
	{Key: "AUTH_TOKENS", Group: GroupPool, Kind: "list", Secret: true, Hidden: true,
		Default:     "",
		Description: `Comma-separated upstream FreeBuff tokens. Managed in the Tokens page and Device Login.`},
	{Key: "BRIDGE_ENABLED", Group: GroupPool, Kind: "bool", Essential: true,
		Default:     "true",
		Description: `With AUTH_TOKENS set, accept bridge-mode clients (their own token relayed) alongside the pool — hybrid mode. 0 = locked-down pooled-only instance.`},
	{Key: "BRIDGE_IDLE_EVICT", Group: GroupPool, Kind: "text", Hidden: true,
		Default:     "72h",
		Description: `How long a bridge entry may sit unused before its runs are FINISHed and it is evicted from the cache (sliding TTL; zero or invalid → 72h).`},
	{Key: "IDLE_ROTATION_TIMEOUT", Group: GroupPool, Kind: "text", Hidden: true,
		Default:     "0",
		Description: `Finish runs after this idle period (0 = disabled; SAFE_MODE sets 30m when unset).`},
	{Key: "MODEL_LOCKS", Group: GroupPool, Kind: "text",
		Default:     "",
		Description: `Pin pool slots to models (slot-indexed allowlist, e.g. "0:z-ai/glm-5.2;1:deepseek/deepseek-v4-flash,mimo/mimo-v2.5"). Slots without an entry serve any model. Applies live on reload; malformed values reject the config.`},
	{Key: "MODEL_UNAVAILABLE_CACHE_TTL", Group: GroupPool, Kind: "text", Hidden: true,
		Default:     "1h",
		Description: `How long a model_unavailable admission refusal is remembered per model (off-window models short-circuit to the fallback within the TTL).`},
	{Key: "RATE_LIMIT_BURST", Group: GroupPool, Kind: "int", Hidden: true,
		Default:     "0",
		Description: `Burst request capacity per client IP (0 = defaults to 2 × RATE_LIMIT_PER_IP).`},
	{Key: "RATE_LIMIT_FAILOVER", Group: GroupPool, Kind: "bool", Hidden: true,
		Default:     "true",
		Description: `Automatically rotate and lease another available token when an in-flight request encounters a 429 rate limit (default true).`},
	{Key: "RATE_LIMIT_PER_IP", Group: GroupPool, Kind: "text", Essential: true,
		Default:     "0",
		Description: `Requests/second allowed per client IP (0 = disabled; e.g. 20).`},
	{Key: "ROTATION_INTERVAL", Group: GroupPool, Kind: "text", RestartOnly: true, Hidden: true,
		Default:     "6h",
		Description: `Agent-run rotation interval.`},
	{Key: "RUNS_DRAIN_QUEUE_CAP", Group: GroupPool, Kind: "int", RestartOnly: true, Hidden: true,
		Default:     "64",
		Description: `Draining-runs list cap; entries beyond it are force-dropped (FINISH is best-effort).`},
	{Key: "RUNS_DRAIN_TTL", Group: GroupPool, Kind: "text", RestartOnly: true, Hidden: true,
		Default:     "10m",
		Description: `Draining-runs TTL eviction window.`},
	{Key: "RUN_FINISH_INLINE_TIMEOUT", Group: GroupPool, Kind: "text", RestartOnly: true, Hidden: true,
		Default:     "250ms",
		Description: `Synchronous inline FINISH fallback bound when the deferred-FINISH queue is full.`},
	{Key: "RUN_FINISH_QUEUE_SIZE", Group: GroupPool, Kind: "int", RestartOnly: true, Hidden: true,
		Default:     "64",
		Description: `Bounded deferred-FINISH worker queue for rotated/drained runs (full queue falls back to a synchronous FINISH).`},
	{Key: "SESSION_CREATE_MAX_PARALLEL_GLOBAL", Group: GroupPool, Kind: "int", Hidden: true,
		Default:     "128",
		Description: `Cap on concurrent in-flight session admissions (wait-or-503) across all models.`},
	{Key: "SESSION_CREATE_MAX_PARALLEL_PER_MODEL", Group: GroupPool, Kind: "int", Hidden: true,
		Default:     "32",
		Description: `Per-model cap on concurrent in-flight session admissions.`},
	{Key: "SESSION_IDLE_END", Group: GroupPool, Kind: "text", Hidden: true,
		Default:     "0",
		Description: `End upstream sessions after this idle period, releasing the token's daily admission slot while the proxy sits unused (0 = disabled, opt-in).`},
	{Key: "SESSION_PERSIST", Group: GroupPool, Kind: "bool", RestartOnly: true, Essential: true,
		Default:     "true",
		Description: `Persist session state AND active agent runs to disk so a restart resumes them instead of re-creating (default true). Set to false to disable.`},
	{Key: "SESSION_PROBE_CACHE_TTL", Group: GroupPool, Kind: "text", Hidden: true,
		Default:     "15s",
		Description: `Reuse the last successful session state (skip redundant session poll GETs) within this window.`},
	{Key: "SESSION_RE_ADMIT_LEAD", Group: GroupPool, Kind: "text", Hidden: true,
		Default:     "60s",
		Description: `Re-admit a session pre-emptively when less than this remains: the request rides the old session while the refresh runs in the background.`},
	{Key: "SESSION_STATE_FILE", Group: GroupPool, Kind: "text", RestartOnly: true, Hidden: true,
		Default:     ".freebuff-session-state.json",
		Description: `Path of the session state file (used when SESSION_PERSIST=true; token-keyed, 0600, raw tokens never written).`},
	{Key: "TOKEN_ROTATION", Group: GroupPool, Kind: "select", Enum: []string{"drain", "round_robin", "least_used", "random"}, Hidden: true,
		Default:     "drain",
		Description: `Token selection strategy: drain (exhaust a token's session before rotating), round_robin, least_used, or random. Managed interactively on the Tokens page.`},
	{Key: "WAITING_ROOM_CHAIN", Group: GroupPool, Kind: "bool", Hidden: true,
		Default:     "false",
		Description: `After an upstream 428 waiting_room_required, fire the reference ad-chain + streak requests before the next session create (best-effort, never blocks).`},

	// ── quota ────────────────────────────────────────────────────────────
	{Key: "BRIDGE_DAILY_LIMIT", Group: GroupQuota, Kind: "int", Hidden: true,
		Default:     "0",
		Description: `Global daily chat cap across ALL bridge-mode entries (0 = unlimited).`},
	{Key: "FALLBACK_AFTER_MS", Group: GroupQuota, Kind: "int", Hidden: true,
		Default:     "0",
		Description: `Queue-wait threshold (ms) before re-routing to FALLBACK_MODEL; 0 (default) disables fallback. Queue-wait only — never on 429 quota exhaustion.`},
	{Key: "FALLBACK_MODEL", Group: GroupQuota, Kind: "list", Hidden: true,
		Default:     "",
		Description: `Map model=fallback (comma-separated) to re-route a request when its queue wait passes FALLBACK_AFTER_MS. Empty default: no fallback.`},
	{Key: "MAX_MESSAGES_PER_DAY", Group: GroupQuota, Kind: "int", Hidden: true,
		Default:     "0",
		Description: `Per-token daily cap on successful chats (0 = unlimited; the upstream 429 lock is the real enforcement).`},
	{Key: "MAX_SPEND_PER_DAY", Group: GroupQuota, Kind: "int", Hidden: true,
		Default:     "0",
		Description: `Advisory per-token Pacific-day spend ceiling in ledger units (0 = unlimited). Never enforced — surfacing only, on /healthz.`},
	{Key: "QUOTA_FALLBACK_MODELS", Group: GroupQuota, Kind: "list", Hidden: true,
		Default:     "",
		Description: `Map model=fallback when its session quota is exhausted or unentitled (comma-separated k=v pairs). Empty default: no fallback (surfaces honest 429 with guidance to switch model).`},
	// ── upstream ─────────────────────────────────────────────────────────
	{Key: "CACHE_CONTROL_INJECTION", Group: GroupUpstream, Kind: "bool", Hidden: true,
		Default:     "true",
		Description: `Add {"type":"ephemeral"} cache_control to the stable context prefix on DeepSeek requests (prompt-cache cost reduction; default on). Set CACHE_CONTROL_INJECTION=false to disable.`},
	{Key: "COMPRESS_PROMPT", Group: GroupUpstream, Kind: "bool", Hidden: true,
		Default:     "false",
		Description: `Apply optional prompt & context compression: middle user/assistant turns beyond the trailing budget are dropped and summarized by one marker (default off).`},
	{Key: "COST_MODE", Group: GroupUpstream, Kind: "select", Enum: []string{"free"}, RestartOnly: true, Hidden: true,
		Default:     "free",
		Description: `MUST stay "free": any other value routes requests as PAID and fresh free accounts get 402 "Out of credits".`},
	{Key: "HTTP2_UPSTREAM", Group: GroupUpstream, Kind: "bool", RestartOnly: true, Hidden: true,
		Default:     "true",
		Description: `Negotiate HTTP/2 with the upstream so the ALPN matches real browsers; false forces HTTP/1.1.`},
	{Key: "MODELS_ALLOW", Group: GroupUpstream, Kind: "list", Essential: true,
		Default:     "",
		Description: `Comma-separated model allowlist: when set, only these resolved model ids are served and any other request is rejected with 404 model_not_found. Empty = all models allowed.`},
	{Key: "MODELS_HIDE_UNAVAILABLE", Group: GroupUpstream, Kind: "bool", Hidden: true,
		Default:     "false",
		Description: `/v1/models prunes models marked unavailable (region/tier demotion, quota exhaustion); off by default so a stale signal never hides a working model.`},
	{Key: "MODEL_ALIASES", Group: GroupUpstream, Kind: "list", Essential: true,
		Default:     "",
		Description: `Map aliases to real model IDs (e.g. gpt-4o:openai/gpt-5.6-luna; comma-separated). No built-in aliases — clients must map explicitly.`},
	{Key: "REASONING_IN_CONTENT", Group: GroupUpstream, Kind: "text",
		Default:     "",
		Description: `Fold reasoning_content into message content as <tag>...</tag> for clients that do not render a reasoning channel. "true" uses <think>; a tag word (e.g. "thinking") sets the label; empty/off disables.`},
	{Key: "REGISTRY_REFRESH", Group: GroupUpstream, Kind: "text", Hidden: true,
		Default:     "6h",
		Description: `Model catalog refresh interval.`},
	{Key: "REQUEST_JITTER", Group: GroupUpstream, Kind: "text", RestartOnly: true, Hidden: true,
		Default:     "0s",
		Description: `Random delay range [0, REQUEST_JITTER) before upstream chat calls; SAFE_MODE sets 200ms when unset; 0 disables.`},
	{Key: "REQUEST_TIMEOUT", Group: GroupUpstream, Kind: "text", RestartOnly: true, Hidden: true,
		Default:     "15m",
		Description: `Upstream request timeout.`},
	{Key: "SESSION_CALL_TIMEOUT", Group: GroupUpstream, Kind: "text", RestartOnly: true, Hidden: true,
		Default:     "30s",
		Description: `Session call timeout.`},
	{Key: "TLS_FINGERPRINT", Group: GroupUpstream, Kind: "select",
		Enum:        []string{"auto", "chrome120", "chrome126", "safari17", "safari18", "firefox120", "firefox128", "edge126", "random"},
		RestartOnly: true, Default: "", Hidden: true,
		Description: `TLS fingerprint for upstream egress (empty = plain Go/Bun baseline, CLI-faithful; auto/browser values use utls to mimic browser JA3 for WAF evasion on datacenter IPs).`},
	{Key: "TRANSIENT_RETRIES", Group: GroupUpstream, Kind: "int", RestartOnly: true, Hidden: true,
		Default:     "1",
		Description: `Max additional attempts after a transient transport failure (never retries 429/403/401; 0 disables).`},
	{Key: "UPSTREAM_BASE_URL", Group: GroupUpstream, Kind: "text", RestartOnly: true, Hidden: true,
		Default:     "https://www.codebuff.com",
		Description: `Upstream API endpoint (codebuff.com is normalized to www.codebuff.com).`},

	// ── security ─────────────────────────────────────────────────────────
	{Key: "ADMIN_TOKEN", Group: GroupSecurity, Kind: "secret", Secret: true, Hidden: true,
		Default:     "123456",
		Description: `Login password for the admin dashboard and the bearer token POST /admin/reload requires. Managed via the Security settings card.`},
	{Key: "CORS_ALLOWED_ORIGIN", Group: GroupSecurity, Kind: "text", Hidden: true,
		Default:     "*",
		Description: `Access-Control-Allow-Origin for /v1/* responses.`},
	{Key: "DASHBOARD_REQUIRE_LOGIN", Group: GroupSecurity, Kind: "bool", Hidden: true,
		Default:     "true",
		Description: `Enforce password authentication for the admin dashboard. When false, loopback clients access the dashboard without login (open mode). When true, dashboard requires password. Defaults to true.`},
	{Key: "WEBHOOK_URL", Group: GroupSecurity, Kind: "text", Secret: true, RestartOnly: true, Hidden: true,
		Default:     "",
		Description: `Best-effort alert POSTs for pool_exhausted, token_banned, and agent_model_mismatch_escalation (empty = disabled; at most one POST per event type per 5m; may carry credentials in the URL userinfo).`},
}
