// Package convert implements pure OpenAI request/response normalization for
// the freebuff-proxy bridge.
//
// It performs no I/O: every function is a pure transformation over JSON
// decoded from a request body or SSE frame. The primary entry points (the
// *Opts variants) never read the process environment, touch the filesystem,
// or make network calls; the only external inputs are passed in explicitly
// (Options, a ReasoningLookupFn, an Accumulator's per-call config).
// DefaultOptions is a deprecated compatibility shim that still reads
// COMPRESS_PROMPT / CACHE_CONTROL_INJECTION / REASONING_IN_CONTENT from the
// environment until callers supply config-derived options (issue #277).
//
// The one internal dependency is backend/internal/modelcat (per-model
// reasoning-effort ladders and model classification). Envelope injection
// (codebuff_metadata, forced stream, x-freebuff headers) is deliberately out
// of scope — that lives in backend/internal/upstream.
//
// Everything here is a pure function over JSON: request sanitization
// (parameter whitelist, developer→system role rewrite, tool-schema
// normalization), SSE frame encoding, per-chunk stream sanitization, and the
// non-streaming response accumulator.
package convert

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

var fallbackCounter atomic.Uint64

// randHex returns n random bytes hex-encoded (16 bytes → the 32 hex chars of
// a uuid4 hex, 12 bytes → 24 hex chars). Falls back to time+counter if
// crypto/rand fails, which practically never happens.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("%x%x", time.Now().UnixNano(), fallbackCounter.Add(1))
}

// ---------------------------------------------------------------------------
// Optional prompt & context compression (issue #58).
//
// Env-gated (COMPRESS_PROMPT=true, default off) and deliberately
// conservative: only plain user/assistant content turns strictly inside the
// middle of the conversation are dropped, tool calls/results and the last
// message are never touched, and long content is capped with an explicit
// summary marker. Tests may shrink the budget vars.
// ---------------------------------------------------------------------------

const (
	// compressMarkerPrefix/Suffix form the summary marker inserted where the
	// truncation begins: "[truncated by freebuff-proxy compression; N earlier
	// messages omitted]".
	compressMarkerPrefix = "[truncated by freebuff-proxy compression; "
	compressMarkerSuffix = " earlier messages omitted]"
	// compressContentMarker is appended to a kept message whose content was
	// capped.
	compressContentMarker = "[truncated by freebuff-proxy compression]"
)

// compressMessages compresses a message list in place: middle user/assistant
// content turns beyond the trailing budget are dropped and summarized by ONE
// marker message; long content on kept user/assistant turns is capped. Tool
// results, assistant tool_calls, system messages and the last message are
// never dropped or truncated. Returns the (possibly new) slice and the
// number of messages omitted.
func compressMessages(messages []any, opts Options) ([]any, int) {
	if opts.CompressKeepLast <= 0 {
		return messages, 0
	}
	capLongContents(messages, opts)
	n := len(messages)
	if n <= opts.CompressKeepLast {
		return messages, 0
	}
	keepStart := n - opts.CompressKeepLast // first index of the trailing window

	// Pass 1: count droppable middle turns and where the marker goes.
	dropped := 0
	markerIdx := -1
	for i := 0; i < keepStart; i++ {
		m, ok := messages[i].(map[string]any)
		if !ok {
			continue // non-map entry: cannot classify, keep it
		}
		if mustKeepMessage(m) {
			continue // system prompt, tool results, assistant tool_calls
		}
		dropped++
		if markerIdx < 0 {
			markerIdx = i
		}
	}
	if dropped == 0 {
		return messages, 0
	}

	// Pass 2: rebuild, replacing the dropped span with one marker message.
	out := make([]any, 0, capHint(n-dropped, 1))
	for i := 0; i < n; i++ {
		if i < keepStart {
			m, ok := messages[i].(map[string]any)
			if ok && !mustKeepMessage(m) {
				if i == markerIdx {
					out = append(out, map[string]any{
						"role":    "system",
						"content": fmt.Sprintf("%s%d%s", compressMarkerPrefix, dropped, compressMarkerSuffix),
					})
				}
				continue
			}
		}
		out = append(out, messages[i])
	}
	return out, dropped
}

func roleOf(m map[string]any) string {
	role, _ := m["role"].(string)
	return role
}

// mustKeepMessage reports whether a message must survive compression: tool
// results, assistant tool_calls and non-user/assistant roles (system,
// developer, function) are never dropped — dropping them would break the
// tool-call schema or lose instructions.
func mustKeepMessage(m map[string]any) bool {
	switch roleOf(m) {
	case "user":
		return false
	case "assistant":
		if _, has := m["tool_calls"]; has {
			return true
		}
		return false
	default:
		return true // system, developer, tool, function, unknown
	}
}

// capLongContents truncates string content longer than compressMaxContentBytes
// on kept user/assistant turns, appending the summary marker. The last
// message and tool messages are never touched.
func capLongContents(messages []any, opts Options) {
	if len(messages) == 0 {
		return
	}
	for i := 0; i < len(messages)-1; i++ { // never the last (current) message
		m, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		switch roleOf(m) {
		case "user", "assistant":
		default:
			continue
		}
		if _, has := m["tool_calls"]; has {
			continue
		}
		content, ok := m["content"].(string)
		if !ok || opts.CompressMaxContentBytes <= 0 || len(content) <= opts.CompressMaxContentBytes {
			continue
		}
		m["content"] = truncateRunes(content, opts.CompressMaxContentBytes) + "…" + compressContentMarker
	}
}

// truncateRunes cuts s to at most maxBytes bytes on a rune boundary.
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := 0
	for cut = range s {
		if cut >= maxBytes {
			break
		}
	}
	return s[:cut]
}

// ---------------------------------------------------------------------------
// DeepSeek prompt-cache cache_control injection (issue #84).
// Ported from freebuff-reverse/internal/channels/freebuff/model.go
// injectCacheControl.
// ---------------------------------------------------------------------------

// InjectCacheControl adds {"type":"ephemeral"} cache_control to every content
// block of messages at indices 2 and 3 (the stable context prefix) when the
// block does not already carry one. Messages whose content is not a block
// array (e.g. plain strings) are skipped untouched. Exportable so the CLI
// envelope builder can apply the same hints after rewriting messages.
func InjectCacheControl(messages []any) {
	for i := 2; i < len(messages) && i < 4; i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"]
		if !ok {
			continue
		}
		blocks, ok := content.([]any)
		if !ok {
			continue
		}
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if _, exists := block["cache_control"]; !exists {
				block["cache_control"] = map[string]any{"type": "ephemeral"}
			}
		}
	}
}
