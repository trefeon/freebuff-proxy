package convert

import (
	"os"
	"strings"
)

// Options carries the per-call knob set for the convert entry points. It
// replaces the package-level os.Getenv reads (COMPRESS_PROMPT,
// CACHE_CONTROL_INJECTION, REASONING_IN_CONTENT) and the test-shrinkable
// package globals (maxSchemaNodes, compressKeepLast,
// compressMaxContentBytes), so a caller resolves them once (e.g. from
// backend/internal/config) instead of on the per-chunk hot path.
type Options struct {
	// CompressPrompt enables optional prompt & context compression (#58):
	// middle user/assistant content turns beyond the trailing budget are
	// dropped and summarized by one marker message. Default off.
	CompressPrompt bool
	// CacheControlInjection enables DeepSeek prompt-cache cache_control
	// injection (#84). Default enabled.
	CacheControlInjection bool
	// ReasoningInContent is the think-tag label when reasoning_content is
	// folded into delta.content (#44), or "" when disabled. An empty value
	// keeps the reasoning channel separate.
	ReasoningInContent string
	// MaxSchemaNodes is the per-request tool-schema normalization node
	// budget: beyond it remaining structure is returned unchanged.
	MaxSchemaNodes int
	// ReasoningLookup restores cached reasoning content for assistant
	// messages in multi-turn requests (issue #251): threaded per-call so two
	// Servers in one process never share one hook (the old process-global
	// SetReasoningLookup was silently overwritten by the second New).
	ReasoningLookup ReasoningLookupFn

	// CompressKeepLast is the number of trailing messages always kept
	// during compression.
	CompressKeepLast int
	// CompressMaxContentBytes caps string content on kept user/assistant
	// turns (never the last message, never tool results).
	CompressMaxContentBytes int
}

// Default option values. Exported so callers that resolve the three feature
// modes from config.Config (issue #277) can build Options without the
// deprecated env shim.
const (
	DefaultMaxSchemaNodes          = 100_000
	DefaultCompressKeepLast        = 10
	DefaultCompressMaxContentBytes = 8 << 10
)

// DefaultOptions returns the option set matching today's package-wide
// defaults for the convert knobs.
//
// DEPRECATED: the three env-driven fields are read from COMPRESS_PROMPT,
// CACHE_CONTROL_INJECTION and REASONING_IN_CONTENT as a compatibility shim
// until server/engine wiring constructs Options from config.Config (issue
// #277). Remove DefaultOptions and the env reads once every caller passes
// explicit Options.
func DefaultOptions() Options {
	return Options{
		CompressPrompt:          compressEnabledFromEnv(),
		CacheControlInjection:   cacheControlInjectionEnabledFromEnv(),
		ReasoningInContent:      reasoningInContentModeFromEnv(),
		MaxSchemaNodes:          DefaultMaxSchemaNodes,
		CompressKeepLast:        DefaultCompressKeepLast,
		CompressMaxContentBytes: DefaultCompressMaxContentBytes,
	}
}

// compressEnabledFromEnv reports whether prompt compression is on
// (COMPRESS_PROMPT=true). Deprecated: prefer Options.CompressPrompt.
func compressEnabledFromEnv() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("COMPRESS_PROMPT")))
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// cacheControlInjectionEnabledFromEnv reports whether cache_control
// injection is on. Default ON; set CACHE_CONTROL_INJECTION=false (or
// 0/off/no/disabled) to disable. Deprecated: prefer
// Options.CacheControlInjection.
func cacheControlInjectionEnabledFromEnv() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CACHE_CONTROL_INJECTION")))
	switch v {
	case "0", "false", "off", "no", "disabled":
		return false
	}
	return true
}

// reasoningInContentModeFromEnv returns the think-tag label when reasoning
// folding is enabled, or "" when off. The env var REASONING_IN_CONTENT may
// be a boolean (true → "think") or an explicit tag word ("thinking" →
// "thinking"). Deprecated: prefer Options.ReasoningInContent.
func reasoningInContentModeFromEnv() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("REASONING_IN_CONTENT")))
	switch v {
	case "", "0", "false", "off", "no", "disabled":
		return ""
	case "1", "true", "yes", "on":
		return reasoningInContentTag
	}
	return v
}
