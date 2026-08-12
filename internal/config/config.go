// Package config loads and validates freebuff-proxy configuration.
//
// Precedence: JSON config file (optional) < environment variables. Every key
// in the JSON file mirrors its environment variable name; values set in the
// environment always win. Auth tokens and API keys are comma-separated lists
// in the environment and JSON arrays in the file.
package config

import (
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
	ListenAddr          string
	UpstreamBaseURL     string
	AuthTokens          []string
	RotationInterval    time.Duration
	RequestTimeout      time.Duration
	SessionCallTimeout  time.Duration
	APIKeys             []string
	HTTPProxy           string
	SOCKS5Proxy         string
	CostMode            string // "" (omit) or "free"; A/B pending, PRD §8
	TLSFingerprint      string // "" (plain Go transport) | chrome120 | chrome126 | safari17 | safari18 | firefox120 | firefox128 | edge126 | random | auto
	RegistryRefresh     time.Duration
	DebugDump           bool
	LogFile             string
	LogLevel            string        // "" (use -v/default) or debug|info|warn|error
	MaxMessagesPerDay   int           // 0 = unlimited: per-token cap on successful chats per 24h
	IdleRotationTimeout time.Duration // 0 = disabled: pause rotation/refresh after this idle period
	SafeMode            bool          // true = apply recommended anti-ban safe defaults
	RequestJitter       time.Duration // random delay range [0, RequestJitter) before upstream chat calls
	CLIVersion          string        // upstream CLI version string (default: 0.10.7)
}
// BridgeMode reports whether the proxy runs without any AUTH_TOKENS: every
// client supplies their own FreeBuff token per request (Authorization: Bearer
// or x-api-key), and the proxy relays with that token upstream.
func (c Config) BridgeMode() bool { return len(c.AuthTokens) == 0 }

// rawConfig mirrors the JSON file / env keys as strings so that parsing and
// validation happen once, after all overrides are applied.
type rawConfig struct {
	ListenAddr          string   `json:"LISTEN_ADDR"`
	UpstreamBaseURL     string   `json:"UPSTREAM_BASE_URL"`
	AuthTokens          []string `json:"AUTH_TOKENS"`
	RotationInterval    string   `json:"ROTATION_INTERVAL"`
	RequestTimeout      string   `json:"REQUEST_TIMEOUT"`
	SessionCallTimeout  string   `json:"SESSION_CALL_TIMEOUT"`
	APIKeys             []string `json:"API_KEYS"`
	HTTPProxy           string   `json:"HTTP_PROXY"`
	SOCKS5Proxy         string   `json:"SOCKS5_PROXY"`
	CostMode            string   `json:"COST_MODE"`
	TLSFingerprint      string   `json:"TLS_FINGERPRINT"`
	RegistryRefresh     string   `json:"REGISTRY_REFRESH"`
	DebugDump           bool     `json:"DEBUG_DUMP"`
	LogFile             string   `json:"LOG_FILE"`
	LogLevel            string   `json:"LOG_LEVEL"`
	MaxMessagesPerDay   int      `json:"MAX_MESSAGES_PER_DAY"`
	IdleRotationTimeout string   `json:"IDLE_ROTATION_TIMEOUT"`
	SafeMode            bool     `json:"SAFE_MODE"`
	RequestJitter       string   `json:"REQUEST_JITTER"`
	CLIVersion          string   `json:"CLI_VERSION"`
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
		MaxMessagesPerDay:   0,      // 0 = unlimited
		IdleRotationTimeout: "0",    // 0 = disabled
		SafeMode:            false,
		RequestJitter:       "0s",
		CLIVersion:          "0.10.7",
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
	overrideCSV(&raw.AuthTokens, "AUTH_TOKENS")
	overrideString(&raw.RotationInterval, "ROTATION_INTERVAL")
	overrideString(&raw.RequestTimeout, "REQUEST_TIMEOUT")
	overrideString(&raw.SessionCallTimeout, "SESSION_CALL_TIMEOUT")
	overrideCSV(&raw.APIKeys, "API_KEYS")
	overrideString(&raw.HTTPProxy, "HTTP_PROXY")
	overrideString(&raw.SOCKS5Proxy, "SOCKS5_PROXY")
	overrideString(&raw.CostMode, "COST_MODE")
	overrideString(&raw.TLSFingerprint, "TLS_FINGERPRINT")
	overrideString(&raw.RegistryRefresh, "REGISTRY_REFRESH")
	overrideBool(&raw.DebugDump, "DEBUG_DUMP")
	overrideString(&raw.LogFile, "LOG_FILE")
	overrideString(&raw.LogLevel, "LOG_LEVEL")
	overrideInt(&raw.MaxMessagesPerDay, "MAX_MESSAGES_PER_DAY")
	overrideString(&raw.IdleRotationTimeout, "IDLE_ROTATION_TIMEOUT")
	overrideBool(&raw.SafeMode, "SAFE_MODE")
	overrideString(&raw.RequestJitter, "REQUEST_JITTER")
	overrideString(&raw.CLIVersion, "CLI_VERSION")
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
	idleRotationTimeout := time.Duration(0)
	if strings.TrimSpace(raw.IdleRotationTimeout) != "" && strings.TrimSpace(raw.IdleRotationTimeout) != "0" {
		idleRotationTimeout, err = parseDuration(raw.IdleRotationTimeout, "IDLE_ROTATION_TIMEOUT")
		if err != nil {
			return Config{}, err
		}
	}
	requestJitter := time.Duration(0)
	if strings.TrimSpace(raw.RequestJitter) != "" {
		requestJitter, err = parseDuration(raw.RequestJitter, "REQUEST_JITTER")
		if err != nil {
			return Config{}, err
		}
	}
	upstreamBaseURL, err := normalizeUpstreamBaseURL(raw.UpstreamBaseURL)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		ListenAddr:          strings.TrimSpace(raw.ListenAddr),
		UpstreamBaseURL:     upstreamBaseURL,
		AuthTokens:          dedupeStrings(raw.AuthTokens),
		RotationInterval:    rotationInterval,
		RequestTimeout:      requestTimeout,
		SessionCallTimeout:  sessionCallTimeout,
		APIKeys:             dedupeStrings(raw.APIKeys),
		HTTPProxy:           strings.TrimSpace(raw.HTTPProxy),
		SOCKS5Proxy:         strings.TrimSpace(raw.SOCKS5Proxy),
		CostMode:            strings.TrimSpace(raw.CostMode),
		TLSFingerprint:      strings.TrimSpace(raw.TLSFingerprint),
		RegistryRefresh:     registryRefresh,
		DebugDump:           raw.DebugDump,
		LogFile:             strings.TrimSpace(raw.LogFile),
		LogLevel:            strings.TrimSpace(raw.LogLevel),
		MaxMessagesPerDay:   raw.MaxMessagesPerDay,
		IdleRotationTimeout: idleRotationTimeout,
		SafeMode:            raw.SafeMode,
		RequestJitter:       requestJitter,
		CLIVersion:          strings.TrimSpace(raw.CLIVersion),
	}

	// SafeMode presets: when SAFE_MODE=true, apply recommended defaults
	// for any zero-valued account-safety knob.
	if cfg.SafeMode {
		if cfg.MaxMessagesPerDay == 0 {
			cfg.MaxMessagesPerDay = 150
		}
		if cfg.IdleRotationTimeout == 0 {
			cfg.IdleRotationTimeout = 30 * time.Minute
		}
		if cfg.RequestJitter == 0 {
			cfg.RequestJitter = 2 * time.Second
		}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
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
	}

	for i, tok := range c.AuthTokens {
		if strings.HasPrefix(strings.ToLower(tok), "bearer ") {
			return fmt.Errorf("AUTH_TOKENS token #%d starts with 'Bearer ' prefix -- remove 'Bearer ' (the proxy adds it upstream automatically)", i+1)
		}
		if tok == "cb_xxx" || tok == "cb_yyy" || tok == "YOUR_TOKEN_HERE" {
			return fmt.Errorf("AUTH_TOKENS token #%d is a placeholder %q -- replace with a real FreeBuff token from https://freebuff.llm.pm", i+1, tok)
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
	overrideStringFrom(&raw.ListenAddr, get, "LISTEN_ADDR")
	overrideStringFrom(&raw.UpstreamBaseURL, get, "UPSTREAM_BASE_URL")
	overrideCSVFrom(&raw.AuthTokens, get, "AUTH_TOKENS")
	overrideStringFrom(&raw.RotationInterval, get, "ROTATION_INTERVAL")
	overrideStringFrom(&raw.RequestTimeout, get, "REQUEST_TIMEOUT")
	overrideStringFrom(&raw.SessionCallTimeout, get, "SESSION_CALL_TIMEOUT")
	overrideCSVFrom(&raw.APIKeys, get, "API_KEYS")
	overrideStringFrom(&raw.HTTPProxy, get, "HTTP_PROXY")
	overrideStringFrom(&raw.SOCKS5Proxy, get, "SOCKS5_PROXY")
	overrideStringFrom(&raw.CostMode, get, "COST_MODE")
	overrideStringFrom(&raw.TLSFingerprint, get, "TLS_FINGERPRINT")
	overrideStringFrom(&raw.RegistryRefresh, get, "REGISTRY_REFRESH")
	overrideBoolFrom(&raw.DebugDump, get, "DEBUG_DUMP")
	overrideStringFrom(&raw.LogFile, get, "LOG_FILE")
	overrideStringFrom(&raw.LogLevel, get, "LOG_LEVEL")
	overrideIntFrom(&raw.MaxMessagesPerDay, get, "MAX_MESSAGES_PER_DAY")
	overrideStringFrom(&raw.IdleRotationTimeout, get, "IDLE_ROTATION_TIMEOUT")
	return nil
}

// readDotenv parses a dotenv file: KEY=VALUE lines, blank lines and #
// comments skipped, surrounding whitespace and matching quotes trimmed.
// Returns nil, nil when the file does not exist.
func readDotenv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	out := make(map[string]string)
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
		value = strings.Trim(value, `"'`)
		out[key] = value
	}
	return out, nil
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
func overrideInt(target *int, envName string) {
	overrideIntFrom(target, os.Getenv, envName)
}

func overrideIntFrom(target *int, get func(string) string, envName string) {
	if value := strings.TrimSpace(get(envName)); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			*target = parsed
		}
	}
}

func splitList(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	return compactStrings(fields)
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
