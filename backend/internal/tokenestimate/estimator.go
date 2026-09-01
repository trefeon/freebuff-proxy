// Package tokenestimate implements a local, deterministic token estimator
// aligned with the FreeBuff upstream context estimator (see
// CodebuffAI/freebuff packages/agent-runtime/src/util/token-counter.ts):
// a GPT-4o/o200k base tokenizer scaled by a conservative multiplier, plus
// per-message overhead, a flat per-image cost, and structured counting of
// tool calls/results so base64 media is never tokenized as plain text.
//
// Known divergence: FreeBuff's gpt-tokenizer encodes OpenAI-style special
// markers (<|endoftext|> etc.) with allowedSpecial="all", collapsing each
// to a single token. tiktoken-go/tokenizer declares a special-token map but
// never consults it during tokenization, so those markers are BPE-encoded
// as ordinary text and therefore conservatively over-counted: 9 tokens for
// <|endoftext|>/<|endofprompt|>/<|endofmask|> and 8 for the <|fim_*|>
// markers, versus 1 in the Python reference. The counts are deterministic
// and never panic; the exact values are pinned by TestCountTextSpecialMarkers.
//
// The result is an estimate for Anthropic-compatible clients, not a
// provider-exact token count.
package tokenestimate

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/tiktoken-go/tokenizer"
)

// Constants mirror the FreeBuff upstream estimator as of 2026-08-17
// (packages/agent-runtime/src/util/token-counter.ts). They are heuristics,
// not Anthropic-published formula values.
const (
	// fudgeFactor is ANTHROPIC_TOKEN_FUDGE_FACTOR: every raw token count is
	// multiplied so the estimate stays deliberately a touch above the true
	// count for context budgeting.
	fudgeFactor = 1.35
	// perMessageOverhead is PER_MESSAGE_TOKEN_OVERHEAD: role marker and
	// delimiters Anthropic adds on top of the raw content per message.
	perMessageOverhead = 8
	// imageTokenEstimate is IMAGE_TOKEN_ESTIMATE: the ceiling Anthropic bills
	// for a large image, used flat instead of counting base64 characters.
	imageTokenEstimate = 1600
	// fallbackDivisor matches the JS fallback (text.length / 3) used when
	// tokenizer encoding fails.
	fallbackDivisor = 3
)

// ErrDocument is returned when a document block is present: the proxy's
// /v1/messages conversion does not consume documents, so this estimator
// refuses to guess a PDF token count instead of faking accuracy. The server
// maps it to a distinct 400 code (unsupported_content).
var ErrDocument = errors.New("document blocks are not supported by this proxy")

// Estimator counts text with a process-shared o200k_base codec. The codec is
// created once; Count/Encode only read its vocabulary, so the instance is
// safe for concurrent use. Decode is serialized by decodeMu (issue #243):
// tiktoken-go builds a reverse-vocabulary map lazily without its own lock.
type Estimator struct {
	codec    tokenizer.Codec
	decodeMu sync.Mutex
}

var (
	codecOnce sync.Once
	codec     tokenizer.Codec
	codecErr  error
)

// New returns an Estimator backed by a shared o200k_base codec (vocabulary
// embedded in the binary; no network access).
func New() (*Estimator, error) {
	codecOnce.Do(func() {
		codec, codecErr = tokenizer.Get(tokenizer.O200kBase)
	})
	if codecErr != nil {
		return nil, codecErr
	}
	return &Estimator{codec: codec}, nil
}

// CountText estimates tokens for one text string: the o200k_base token count
// scaled by the FreeBuff multiplier, or the UTF-16-unit fallback if the
// tokenizer fails (mirrors the JS text.length/3 path).
func (e *Estimator) CountText(text string) int {
	if text == "" {
		return 0
	}
	n, err := e.codec.Count(text)
	if err != nil {
		units := len(utf16.Encode([]rune(text)))
		return (units + fallbackDivisor - 1) / fallbackDivisor
	}
	return int(math.Floor(float64(n) * fudgeFactor))
}

// CountJSON estimates tokens for a value's deterministic JSON encoding. Go's
// encoding/json sorts map keys, so repeated counts of the same semantic
// object are stable.
func (e *Estimator) CountJSON(v any) int {
	if v == nil {
		return 0
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return e.CountText(string(b))
}

// Decode converts token ids back to text. Safe for concurrent use: the
// shared codec's Decode lazily builds a reverse-vocabulary map without a
// lock, so every call is serialized here instead of crashing the process
// (issue #243; the API is safe by construction, not by comment).
func (e *Estimator) Decode(ids []uint) (string, error) {
	e.decodeMu.Lock()
	defer e.decodeMu.Unlock()
	return e.codec.Decode(ids)
}

// CountAnthropicRequest estimates input tokens for an Anthropic
// messages/count_tokens request body: system text + tool definitions + each
// message (per-message overhead plus structured content), mirroring FreeBuff's
// estimateContextTokensLocally. It returns an error for request shapes the
// proxy cannot consume (missing messages, document blocks).
func (e *Estimator) CountAnthropicRequest(raw map[string]any) (int, error) {
	total := 0
	if system, ok := raw["system"]; ok && system != nil {
		n, err := e.countSystem(system)
		if err != nil {
			return 0, err
		}
		total += n
	}
	rawMessages, ok := raw["messages"].([]any)
	if !ok {
		return 0, errors.New(`missing or invalid "messages" (want an array)`)
	}
	for _, rawMsg := range rawMessages {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			// Non-object messages are dropped by /v1/messages conversion and
			// never reach the model, so they add nothing here either.
			continue
		}
		total += perMessageOverhead
		n, err := e.countContent(msg["content"])
		if err != nil {
			return 0, err
		}
		total += n
	}
	if tools, ok := raw["tools"].([]any); ok {
		total += e.CountJSON(e.toolsForCount(tools))
	}
	return total, nil
}

// countSystem counts the top-level system field: a plain string directly, an
// array via its text blocks (JSON fallback for anything else recognized by
// the conversion surface).
func (e *Estimator) countSystem(system any) (int, error) {
	switch typed := system.(type) {
	case string:
		return e.CountText(typed), nil
	case []any:
		total := 0
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(asString(part["type"])) {
			case "text":
				total += e.CountText(asString(part["text"]))
			case "image":
				// Billed flat like every other image path; the base64 payload
				// is never tokenized (and never reaches the model: the
				// conversion drops non-text system parts).
				total += imageTokenEstimate
			case "document":
				return 0, ErrDocument
			default:
				total += e.CountJSON(part)
			}
		}
		return total, nil
	default:
		// Non-string/non-array system values are dropped by the conversion,
		// so nothing is counted.
		return 0, nil
	}
}

// countContent counts one message's content: a string directly, an array of
// content blocks structurally, or a conservative JSON fallback for any other
// accepted shape.
func (e *Estimator) countContent(content any) (int, error) {
	switch typed := content.(type) {
	case nil:
		return 0, nil
	case string:
		return e.CountText(typed), nil
	case []any:
		total := 0
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			n, err := e.countContentPart(part)
			if err != nil {
				return 0, err
			}
			total += n
		}
		return total, nil
	default:
		return e.CountJSON(typed), nil
	}
}

// countContentPart counts one Anthropic content block. Types the /v1/messages
// conversion does not understand get a JSON fallback so accepted structures
// never silently under-count.
func (e *Estimator) countContentPart(part map[string]any) (int, error) {
	switch strings.ToLower(asString(part["type"])) {
	case "text":
		return e.CountText(asString(part["text"])), nil
	case "thinking":
		// The conversion accepts the thinking field, falling back to text.
		text := asString(part["thinking"])
		if text == "" {
			text = asString(part["text"])
		}
		return e.CountText(text), nil
	case "tool_use", "server_tool_use":
		return e.CountText(asString(part["name"])) + e.CountJSON(part["input"]), nil
	case "tool_result":
		return e.countToolResult(part)
	case "image":
		return imageTokenEstimate, nil
	case "document":
		return 0, ErrDocument
	default:
		return e.CountJSON(part), nil
	}
}

// countToolResult counts a tool_result part: textual or structured result
// content, with images billed flat instead of tokenizing base64.
func (e *Estimator) countToolResult(part map[string]any) (int, error) {
	return e.countToolResultContent(part["content"])
}

func (e *Estimator) countToolResultContent(content any) (int, error) {
	switch typed := content.(type) {
	case nil:
		return 0, nil
	case string:
		return e.CountText(typed), nil
	case []any:
		total := 0
		for _, rawItem := range typed {
			switch item := rawItem.(type) {
			case string:
				total += e.CountText(item)
			case map[string]any:
				switch strings.ToLower(asString(item["type"])) {
				case "text":
					total += e.CountText(asString(item["text"]))
				case "image":
					total += imageTokenEstimate
				case "document":
					return 0, ErrDocument
				default:
					total += e.CountJSON(item)
				}
			default:
				total += e.CountJSON(item)
			}
		}
		return total, nil
	default:
		return e.CountJSON(typed), nil
	}
}

// anthropicToolCount is the tool representation the model effectively
// receives, kept in the same field order FreeBuff counts ({name,
// description, input_schema}) so the JSON is byte-stable.
type anthropicToolCount struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
}

func (e *Estimator) toolsForCount(tools []any) []anthropicToolCount {
	out := make([]anthropicToolCount, 0, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		tc := anthropicToolCount{Name: asString(tool["name"])}
		tc.Description = asString(tool["description"])
		if schema, ok := tool["input_schema"]; ok && schema != nil {
			tc.InputSchema = schema
		}
		out = append(out, tc)
	}
	return out
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
