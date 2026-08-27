package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

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
	overrideInt(&raw.BridgeDailyLimit, "BRIDGE_DAILY_LIMIT")
	overrideInt(&raw.MaxSpendPerDay, "MAX_SPEND_PER_DAY")
	overrideString(&raw.IdleRotationTimeout, "IDLE_ROTATION_TIMEOUT")
	overrideString(&raw.SessionIdleEnd, "SESSION_IDLE_END")
	overrideBool(&raw.SafeMode, "SAFE_MODE")
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
	overrideString(&raw.ModelUnavailableCacheTTL, "MODEL_UNAVAILABLE_CACHE_TTL")
	overrideString((*string)(&raw.ScarceSessionModels), "SCARCE_SESSION_MODELS")
	overrideString((*string)(&raw.QuotaFallbackModels), "QUOTA_FALLBACK_MODELS")
	overrideString(&raw.WebhookURL, "WEBHOOK_URL")
	overrideString(&raw.FallbackAfter, "FALLBACK_AFTER_MS")
	overrideString(&raw.FallbackModels, "FALLBACK_MODEL")
	overrideBool(&raw.AdoptCLISession, "ADOPT_CLI_SESSION")
	overrideBool(&raw.WaitingRoomChain, "WAITING_ROOM_CHAIN")
	overrideFloat(&raw.RateLimitPerIP, "RATE_LIMIT_PER_IP")
	overrideInt(&raw.RateLimitBurst, "RATE_LIMIT_BURST")
	overrideString(&raw.TokenRotation, "TOKEN_ROTATION")
	overrideBool(&raw.DashboardEnabled, "DASHBOARD_ENABLED")

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
	// SESSION_PROBE_CACHE_TTL / MODEL_UNAVAILABLE_CACHE_TTL are zero-tolerant
	// durations: "" or "0" fall back to the documented default (a zero inline
	// timeout would make the inline fallback useless; a zero re-admit lead
	// would spin a re-admit on every request).
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
	// MODEL_UNAVAILABLE_CACHE_TTL is zero-tolerant like the other session
	// knobs: "" or "0" fall back to the documented 1h default.
	modelUnavailableCacheTTL := time.Hour
	if v := strings.TrimSpace(raw.ModelUnavailableCacheTTL); v != "" {
		modelUnavailableCacheTTL, err = parseDuration(v, "MODEL_UNAVAILABLE_CACHE_TTL")
		if err != nil {
			return Config{}, err
		}
		if modelUnavailableCacheTTL <= 0 {
			modelUnavailableCacheTTL = time.Hour
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
	// SESSION_IDLE_END is zero-tolerant like IDLE_ROTATION_TIMEOUT: "" or "0"
	// both mean disabled (opt-in knob — ending a session costs a fresh
	// daily-slot admission when traffic resumes).
	sessionIdleEnd := time.Duration(0)
	if strings.TrimSpace(raw.SessionIdleEnd) != "" && strings.TrimSpace(raw.SessionIdleEnd) != "0" {
		sessionIdleEnd, err = parseDuration(raw.SessionIdleEnd, "SESSION_IDLE_END")
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

	// BRIDGE_DAILY_LIMIT (B5): global daily chat cap across ALL bridge
	// entries. 0 = unlimited (default). Explicit values always win.
	bridgeDailyLimit := 0
	if raw.BridgeDailyLimit != nil {
		bridgeDailyLimit = *raw.BridgeDailyLimit
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

	// SCARCE_SESSION_MODELS defaults (issue #155): irreplaceable 1-session/day
	// models kept alive for their full 1 hour window.
	scarceSessionModels := splitList(string(raw.ScarceSessionModels))
	if len(scarceSessionModels) == 0 && string(raw.ScarceSessionModels) == "" {
		scarceSessionModels = defaultScarceSessionModels()
	}

	// QUOTA_FALLBACK_MODELS defaults (issue #155): when a model's session
	// quota is exhausted, fall back to an unlimited model (flash → mimo).
	quotaFallbackModels := parseMap(string(raw.QuotaFallbackModels))
	if len(quotaFallbackModels) == 0 && string(raw.QuotaFallbackModels) == "" {
		quotaFallbackModels = defaultQuotaFallbackModels()
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

	adminToken := strings.TrimSpace(raw.AdminToken)
	if adminToken == "" {
		adminToken = DefaultAdminToken
	}

	tokenRotation := strings.ToLower(strings.TrimSpace(raw.TokenRotation))
	switch tokenRotation {
	case "", "drain":
		tokenRotation = "drain"
	case "round_robin", "roundrobin", "rr":
		tokenRotation = "round_robin"
	case "least_used", "leastused":
		tokenRotation = "least_used"
	case "random", "rand":
		tokenRotation = "random"
	default:
		return Config{}, fmt.Errorf("invalid TOKEN_ROTATION: %q (must be drain, round_robin, least_used, or random)", raw.TokenRotation)
	}

	cfg := Config{
		ListenAddr:                       strings.TrimSpace(raw.ListenAddr),
		UpstreamBaseURL:                  upstreamBaseURL,
		AuthTokens:                       dedupeStrings(raw.AuthTokens),
		RotationInterval:                 rotationInterval,
		RequestTimeout:                   requestTimeout,
		SessionCallTimeout:               sessionCallTimeout,
		TokenRotation:                    tokenRotation,
		APIKeys:                          dedupeStrings(raw.APIKeys),
		AdminToken:                       adminToken,
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
		BridgeDailyLimit:                 bridgeDailyLimit,
		MaxSpendPerDay:                   maxSpendPerDay,
		IdleRotationTimeout:              idleRotationTimeout,
		SessionIdleEnd:                   sessionIdleEnd,
		SafeMode:                         raw.SafeMode,
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
		ModelUnavailableCacheTTL:         modelUnavailableCacheTTL,
		WebhookURL:                       strings.TrimSpace(raw.WebhookURL),
		FallbackAfter:                    fallbackAfter,
		FallbackModels:                   fallbackModels,
		AdoptCLISession:                  raw.AdoptCLISession,
		ScarceSessionModels:              dedupeStrings(scarceSessionModels),
		QuotaFallbackModels:              quotaFallbackModels,
		WaitingRoomChain:                 raw.WaitingRoomChain,
		RateLimitPerIP:                   rateLimitPerIP,
		RateLimitBurst:                   rateLimitBurst,
		DashboardEnabled:                 raw.DashboardEnabled,
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
	overrideStringFrom(&raw.TokenRotation, get, "TOKEN_ROTATION")
	overrideCSVFrom(&raw.APIKeys, get, "API_KEYS")
	overrideStringFrom(&raw.AdminToken, get, "ADMIN_TOKEN")
	overrideStringFrom(&raw.CostMode, get, "COST_MODE")
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
	overrideIntFrom(&raw.BridgeDailyLimit, get, "BRIDGE_DAILY_LIMIT")
	overrideIntFrom(&raw.MaxSpendPerDay, get, "MAX_SPEND_PER_DAY")
	overrideStringFrom(&raw.IdleRotationTimeout, get, "IDLE_ROTATION_TIMEOUT")
	overrideStringFrom(&raw.SessionIdleEnd, get, "SESSION_IDLE_END")
	// The remaining keys mirror the real-environment override set in Load.
	// AUTO_DISCOVER_TOKEN is intentionally env-only (it controls the .env
	// read itself, so honoring it from .env would be circular).
	overrideBoolFrom(&raw.SafeMode, get, "SAFE_MODE")
	overrideBoolFrom(&raw.ModelsHideUnavailable, get, "MODELS_HIDE_UNAVAILABLE")
	overrideStringFrom((*string)(&raw.ModelsAllow), get, "MODELS_ALLOW")
	overrideStringFrom(&raw.CORSAllowedOrigin, get, "CORS_ALLOWED_ORIGIN")
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
	overrideStringFrom(&raw.ModelUnavailableCacheTTL, get, "MODEL_UNAVAILABLE_CACHE_TTL")
	overrideStringFrom(&raw.WebhookURL, get, "WEBHOOK_URL")
	overrideStringFrom((*string)(&raw.ScarceSessionModels), get, "SCARCE_SESSION_MODELS")
	overrideStringFrom((*string)(&raw.QuotaFallbackModels), get, "QUOTA_FALLBACK_MODELS")
	overrideStringFrom(&raw.FallbackAfter, get, "FALLBACK_AFTER_MS")
	overrideStringFrom(&raw.FallbackModels, get, "FALLBACK_MODEL")
	overrideBoolFrom(&raw.AdoptCLISession, get, "ADOPT_CLI_SESSION")
	overrideBoolFrom(&raw.WaitingRoomChain, get, "WAITING_ROOM_CHAIN")
	overrideFloatFrom(&raw.RateLimitPerIP, get, "RATE_LIMIT_PER_IP")
	overrideIntFrom(&raw.RateLimitBurst, get, "RATE_LIMIT_BURST")
	overrideBoolFrom(&raw.DashboardEnabled, get, "DASHBOARD_ENABLED")
	return nil
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
