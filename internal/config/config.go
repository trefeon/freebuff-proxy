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
	"net/url"
	"os"
	"path/filepath"
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
	APIKeys               []string
	AdminToken            string // bearer token required for POST /admin/reload ("" = unauthenticated in default deployments)
	HTTPProxy             string
	SOCKS5Proxy           string
	SOCKS5Proxies         []string // comma-separated list of SOCKS5 proxies (#23)
	ProxyRotation         string   // "per-token" (default), "round-robin", "random" (#23)
	CostMode              string   // "" (omit) or "free"; A/B pending, PRD §8
	TLSFingerprint        string   // "" (plain Go transport) | chrome120 | chrome126 | safari17 | safari18 | firefox120 | firefox128 | edge126 | random | auto
	RegistryRefresh       time.Duration
	DebugDump             bool
	LogFile               string
	LogLevel              string            // "" (use -v/default) or debug|info|warn|error
	MaxMessagesPerDay     int               // 0 = unlimited: per-token cap on successful chats per 24h
	IdleRotationTimeout   time.Duration     // 0 = disabled: pause rotation/refresh after this idle period
	SafeMode              bool              // true = apply recommended anti-ban safe defaults
	HybridMode            bool              // true = relay client tokens like bridge AND serve token-less requests from the pool
	ModelsHideUnavailable bool              // true = /v1/models prunes models marked unavailable (region/tier/quota)
	RequestJitter         time.Duration     // random delay range [0, RequestJitter) before upstream chat calls
	CLIVersion            string            // upstream CLI version string (default: 0.10.7)
	ModelAliases          map[string]string // map model alias -> real model ID (#25)
	TransientRetries      int               // max additional attempts after a transient transport failure (0 = disabled; default 1)
	SessionPersist        bool              // true = persist session state to disk so restart resumes unexpired sessions (SESSION_PERSIST)
	SessionStateFile      string            // path to the session state file (SESSION_STATE_FILE; default .freebuff-session-state.json)
	DiscoveredSource      string            // auto-discovered credentials file path (if any)
	DiscoveredEmail       string            // auto-discovered account email (if any)
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
	AuthTokensSet         bool     `json:"-"`
	RotationInterval      string   `json:"ROTATION_INTERVAL"`
	RequestTimeout        string   `json:"REQUEST_TIMEOUT"`
	SessionCallTimeout    string   `json:"SESSION_CALL_TIMEOUT"`
	APIKeys               []string `json:"API_KEYS"`
	AdminToken            string   `json:"ADMIN_TOKEN"`
	HTTPProxy             string   `json:"HTTP_PROXY"`
	SOCKS5Proxy           string   `json:"SOCKS5_PROXY"`
	SOCKS5Proxies         []string `json:"SOCKS5_PROXIES"`
	ProxyRotation         string   `json:"PROXY_ROTATION"`
	CostMode              string   `json:"COST_MODE"`
	TLSFingerprint        string   `json:"TLS_FINGERPRINT"`
	RegistryRefresh       string   `json:"REGISTRY_REFRESH"`
	DebugDump             bool     `json:"DEBUG_DUMP"`
	LogFile               string   `json:"LOG_FILE"`
	LogLevel              string   `json:"LOG_LEVEL"`
	MaxMessagesPerDay     *int     `json:"MAX_MESSAGES_PER_DAY"`
	IdleRotationTimeout   string   `json:"IDLE_ROTATION_TIMEOUT"`
	SafeMode              bool     `json:"SAFE_MODE"`
	HybridMode            bool     `json:"HYBRID_MODE"`
	ModelsHideUnavailable bool     `json:"MODELS_HIDE_UNAVAILABLE"`
	RequestJitter         string   `json:"REQUEST_JITTER"`
	CLIVersion            string   `json:"CLI_VERSION"`
	ModelAliases          string   `json:"MODEL_ALIASES"`
	TransientRetries      *int     `json:"TRANSIENT_RETRIES"`
	SessionPersist        bool     `json:"SESSION_PERSIST"`
	SessionStateFile      string   `json:"SESSION_STATE_FILE"`
}

func defaultRawConfig() rawConfig {
	return rawConfig{
		ListenAddr:          "127.0.0.1:3457",       // loopback by default (PRD §3); containers set LISTEN_ADDR=:3457
		UpstreamBaseURL:     "https://codebuff.com", // normalized to www.
		RotationInterval:    "6h",
		RequestTimeout:      "15m",
		SessionCallTimeout:  "30s",
		RegistryRefresh:     "6h",
		CostMode:            "free", // free-tier mode; omission routes requests as PAID and fresh free accounts get 402 "Out of credits" (upstream check: cost_mode !== 'free' → billing)
		MaxMessagesPerDay:   nil,
		IdleRotationTimeout: "",    // "" = disabled (unset → SAFE_MODE preset may fill)
		SafeMode:            true,  // anti-ban presets on by default; set SAFE_MODE=false to disable
		HybridMode:          false, // relay client tokens AND serve the pool (off by default)
		RequestJitter:       "",    // "" = disabled (unset → SAFE_MODE preset may fill)
		CLIVersion:          "0.10.7",
		TransientRetries:    nil, // nil = 1 (one retry after a transient transport failure; 0 disables)
		SessionPersist:      false, // opt-in: persist session state across restarts
		SessionStateFile:    ".freebuff-session-state.json",
	}
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
	if err := applyDotenv(&raw); err != nil {
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
	overrideString(&raw.HTTPProxy, "HTTP_PROXY")
	overrideString(&raw.SOCKS5Proxy, "SOCKS5_PROXY")
	overrideCSV(&raw.SOCKS5Proxies, "SOCKS5_PROXIES")
	overrideString(&raw.ProxyRotation, "PROXY_ROTATION")
	overrideString(&raw.CostMode, "COST_MODE")
	overrideString(&raw.TLSFingerprint, "TLS_FINGERPRINT")
	overrideString(&raw.RegistryRefresh, "REGISTRY_REFRESH")
	overrideBool(&raw.DebugDump, "DEBUG_DUMP")
	overrideString(&raw.LogFile, "LOG_FILE")
	overrideString(&raw.LogLevel, "LOG_LEVEL")
	overrideInt(&raw.MaxMessagesPerDay, "MAX_MESSAGES_PER_DAY")
	overrideString(&raw.IdleRotationTimeout, "IDLE_ROTATION_TIMEOUT")
	overrideBool(&raw.SafeMode, "SAFE_MODE")
	overrideBool(&raw.HybridMode, "HYBRID_MODE")
	overrideBool(&raw.ModelsHideUnavailable, "MODELS_HIDE_UNAVAILABLE")
	overrideString(&raw.RequestJitter, "REQUEST_JITTER")
	overrideString(&raw.CLIVersion, "CLI_VERSION")
	overrideString(&raw.ModelAliases, "MODEL_ALIASES")
	overrideInt(&raw.TransientRetries, "TRANSIENT_RETRIES")
	overrideBool(&raw.SessionPersist, "SESSION_PERSIST")
	overrideString(&raw.SessionStateFile, "SESSION_STATE_FILE")

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

	// TRANSIENT_RETRIES: nil defaults to 1 (one additional attempt after a
	// transient transport failure); an explicit 0 disables retries.
	transientRetries := 1
	if raw.TransientRetries != nil {
		transientRetries = *raw.TransientRetries
	}

	cfg := Config{
		ListenAddr:            strings.TrimSpace(raw.ListenAddr),
		UpstreamBaseURL:       upstreamBaseURL,
		AuthTokens:            dedupeStrings(raw.AuthTokens),
		RotationInterval:      rotationInterval,
		RequestTimeout:        requestTimeout,
		SessionCallTimeout:    sessionCallTimeout,
		APIKeys:               dedupeStrings(raw.APIKeys),
		AdminToken:            strings.TrimSpace(raw.AdminToken),
		HTTPProxy:             strings.TrimSpace(raw.HTTPProxy),
		SOCKS5Proxy:           strings.TrimSpace(raw.SOCKS5Proxy),
		SOCKS5Proxies:         dedupeStrings(raw.SOCKS5Proxies),
		ProxyRotation:         strings.TrimSpace(raw.ProxyRotation),
		CostMode:              strings.TrimSpace(raw.CostMode),
		TLSFingerprint:        strings.TrimSpace(raw.TLSFingerprint),
		RegistryRefresh:       registryRefresh,
		DebugDump:             raw.DebugDump,
		LogFile:               strings.TrimSpace(raw.LogFile),
		LogLevel:              strings.TrimSpace(raw.LogLevel),
		MaxMessagesPerDay:     maxMessagesPerDay,
		IdleRotationTimeout:   idleRotationTimeout,
		SafeMode:              raw.SafeMode,
		HybridMode:            raw.HybridMode,
		ModelsHideUnavailable: raw.ModelsHideUnavailable,
		RequestJitter:         requestJitter,
		CLIVersion:            strings.TrimSpace(raw.CLIVersion),
		ModelAliases:          parseMap(raw.ModelAliases),
		TransientRetries:      transientRetries,
		SessionPersist:        raw.SessionPersist,
		SessionStateFile:      strings.TrimSpace(raw.SessionStateFile),
	}

	// Auto-discover CLI token if no AUTH_TOKENS were explicitly configured
	// and AUTO_DISCOVER_TOKEN is not disabled.
	autoDiscover := true
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("AUTO_DISCOVER_TOKEN"))); v == "false" || v == "0" || v == "off" || v == "no" {
		autoDiscover = false
	}
	if autoDiscover && len(cfg.AuthTokens) == 0 && !raw.AuthTokensSet {
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
	case c.SessionPersist && strings.TrimSpace(c.SessionStateFile) == "":
		return errors.New("SESSION_STATE_FILE cannot be empty when SESSION_PERSIST is enabled")
	case c.CostMode != "" && c.CostMode != "free":
		return errors.New(`COST_MODE must be "free" or unset -- any other value (e.g. a typo) routes requests as PAID and fresh free accounts get 402 "Out of credits"`)
	case c.ProxyRotation != "" && c.ProxyRotation != "per-token" && c.ProxyRotation != "round-robin" && c.ProxyRotation != "random":
		return fmt.Errorf("PROXY_ROTATION %q must be one of: per-token, round-robin, random", c.ProxyRotation)
	case c.MaxMessagesPerDay < 0:
		return errors.New("MAX_MESSAGES_PER_DAY cannot be negative")
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
		var level slog.Level
		if err := level.UnmarshalText([]byte(c.LogLevel)); err != nil {
			return fmt.Errorf("LOG_LEVEL %q must be one of: debug, info, warn, error", c.LogLevel)
		}
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
		if err := json.Unmarshal(data, &cfg); err != nil {
			return rawConfig{}, fmt.Errorf("parse config file: %w", err)
		}
		// A non-nil AuthTokens after unmarshal means the JSON key was present
		// ([] is an explicit empty list; absent leaves it nil).
		cfg.AuthTokensSet = cfg.AuthTokens != nil
	}

	return cfg, nil
}

// applyDotenv overlays KEY=VALUE pairs from the ./.env file (when present)
// onto raw, so a local .env works like the JSON config file. A missing file
// is fine; any other read error fails the load. Real environment variables
// are applied afterwards and therefore always win.
func applyDotenv(raw *rawConfig) error {
	vals, err := readDotenv(".env")
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
	overrideStringFrom(&raw.HTTPProxy, get, "HTTP_PROXY")
	overrideStringFrom(&raw.SOCKS5Proxy, get, "SOCKS5_PROXY")
	overrideCSVFrom(&raw.SOCKS5Proxies, get, "SOCKS5_PROXIES")
	overrideStringFrom(&raw.CostMode, get, "COST_MODE")
	overrideStringFrom(&raw.TLSFingerprint, get, "TLS_FINGERPRINT")
	overrideStringFrom(&raw.RegistryRefresh, get, "REGISTRY_REFRESH")
	overrideBoolFrom(&raw.DebugDump, get, "DEBUG_DUMP")
	overrideStringFrom(&raw.LogFile, get, "LOG_FILE")
	overrideStringFrom(&raw.LogLevel, get, "LOG_LEVEL")
	overrideIntFrom(&raw.MaxMessagesPerDay, get, "MAX_MESSAGES_PER_DAY")
	overrideStringFrom(&raw.IdleRotationTimeout, get, "IDLE_ROTATION_TIMEOUT")
	// The remaining keys mirror the real-environment override set in Load.
	// AUTO_DISCOVER_TOKEN is intentionally env-only (it controls the .env
	// read itself, so honoring it from .env would be circular).
	overrideBoolFrom(&raw.SafeMode, get, "SAFE_MODE")
	overrideBoolFrom(&raw.HybridMode, get, "HYBRID_MODE")
	overrideBoolFrom(&raw.ModelsHideUnavailable, get, "MODELS_HIDE_UNAVAILABLE")
	overrideStringFrom(&raw.RequestJitter, get, "REQUEST_JITTER")
	overrideStringFrom(&raw.CLIVersion, get, "CLI_VERSION")
	overrideStringFrom(&raw.ModelAliases, get, "MODEL_ALIASES")
	overrideIntFrom(&raw.TransientRetries, get, "TRANSIENT_RETRIES")
	overrideStringFrom(&raw.ProxyRotation, get, "PROXY_ROTATION")
	overrideBoolFrom(&raw.SessionPersist, get, "SESSION_PERSIST")
	overrideStringFrom(&raw.SessionStateFile, get, "SESSION_STATE_FILE")
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
