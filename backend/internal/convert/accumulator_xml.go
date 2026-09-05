package convert

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// xmlToolCallRegex matches XML-based tool calls such as:
//
//	<tool_call>
//	<function=bash>
//	<parameter=command>...</parameter>
//	</function>
//	</tool_call>
//
// or <tool_call>{"name":"...","arguments":{...}}</tool_call>
//
// codebuff_tool_call is the upstream's own canonical XML tag (issue #144;
// reference common/src/tools/constants.ts — the CLI's stream parser
// util/stream-xml-parser.ts extracts exactly this tag from model output).
var (
	xmlToolCallBlockRe = regexp.MustCompile(`(?s)<tool_call>(.*?)</tool_call>|<codebuff_tool_call>(.*?)</codebuff_tool_call>|<function_call>(.*?)</function_call>|<\|?tool[_\-]?call[_\-]?start\|?>(.*?)<\|?tool[_\-]?call[_\-]?end\|?>|<tool_calls>(.*?)</tool_calls>`)
	fencedToolCallRe   = regexp.MustCompile("(?s)```(?:json|tool_?call)?\\s*\\n?(\\{\\s*\"(?:name|function|cb_tool_name)\"\\s*:\\s*.*?\\})\\s*\\n?```")
	xmlFunctionHeadRe  = regexp.MustCompile(`(?i)<function[=\s]+["']?([^>"\s]+)["']?>`)
	xmlParamRe         = regexp.MustCompile(`(?s)<parameter[=\s]+["']?([^>"\s]+)["']?>(.*?)</parameter>|<param[=\s]+["']?([^>"\s]+)["']?>(.*?)</param>`)
	danglingToolTagsRe = regexp.MustCompile(`(?i)</?(?:tool_call|tool_calls|codebuff_tool_call|function_call|function|parameter|param|\|?tool[_\-]?call[_\-]?(?:start|end)\|?)(?:[=\s][^>]*)?>`)
)

// extractXMLToolCalls parses text-based tool calls (Hermes/Qwen/MiMo XML format)
// that were emitted into content instead of native OpenAI tool_calls fields.
// It returns the cleaned content string and the extracted tool calls.
func extractXMLToolCalls(content string) (string, []*toolCall) {
	return extractXMLToolCallsBytes([]byte(content))
}

// extractXMLToolCallsBytes is the []byte-input form of extractXMLToolCalls.
// The streaming extractor feeds its pooled buffer straight in, avoiding a
// per-closed-block string conversion (issue #165). Submatch boundaries from
// the initial FindAll* pass are reused directly instead of re-matching each
// block with Find*Submatch.
func extractXMLToolCallsBytes(content []byte) (string, []*toolCall) {
	matches := xmlToolCallBlockRe.FindAllSubmatchIndex(content, -1)
	fencedMatches := fencedToolCallRe.FindAllSubmatchIndex(content, -1)
	if len(matches) == 0 && len(fencedMatches) == 0 {
		return string(content), nil
	}

	var calls []*toolCall

	// 1. Check XML block matches (<tool_call>...</tool_call>).
	// FindAllSubmatchIndex reports each alternation group's span; the
	// matching branch's group (1..5) carries the raw payload.
	for _, loc := range matches {
		raw := ""
		for g := 1; g <= 5 && 2*g+1 < len(loc); g++ {
			if loc[2*g] >= 0 && loc[2*g+1] > loc[2*g] {
				raw = string(content[loc[2*g]:loc[2*g+1]])
				break
			}
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}

		if tc := parseToolCallRaw(raw); tc != nil {
			calls = append(calls, tc)
		}
	}

	// 2. Check fenced code blocks (```json {"name": "..."} ```)
	if len(calls) == 0 {
		for _, loc := range fencedMatches {
			if len(loc) >= 4 && loc[2] >= 0 && loc[3] > loc[2] {
				raw := strings.TrimSpace(string(content[loc[2]:loc[3]]))
				if tc := parseToolCallRaw(raw); tc != nil {
					calls = append(calls, tc)
				}
			}
		}
	}

	if len(calls) == 0 {
		return string(content), nil
	}

	// Clean the tool_call blocks from content
	cleaned := xmlToolCallBlockRe.ReplaceAll(content, nil)
	cleaned = fencedToolCallRe.ReplaceAll(cleaned, nil)
	return strings.TrimSpace(string(cleaned)), calls
}

// parseToolCallRaw parses a single raw tool call string in either JSON or XML format.
func parseToolCallRaw(raw string) *toolCall {
	// Try direct JSON: {"name":"...", "arguments":{...}} / {"function":{...}} /
	// or the vendor's canonical codebuff_tool_call JSON keyed by cb_tool_name
	// (reference common/src/tools/constants.ts toolNameParam):
	// {"cb_tool_name":"bash","command":"pwd","cb_easp":true}.
	if strings.HasPrefix(raw, "{") && strings.HasSuffix(raw, "}") {
		var jObj map[string]any
		if err := json.Unmarshal([]byte(raw), &jObj); err == nil {
			name, _ := jObj["name"].(string)
			if name == "" {
				name, _ = jObj["cb_tool_name"].(string)
			}
			if name == "" {
				if fnObj, ok := jObj["function"].(map[string]any); ok {
					name, _ = fnObj["name"].(string)
				} else {
					name, _ = jObj["function"].(string)
				}
			}
			if name != "" {
				var argsStr string
				if argsObj, ok := jObj["arguments"].(map[string]any); ok {
					if b, err := json.Marshal(argsObj); err == nil {
						argsStr = string(b)
					}
				} else if aStr, ok := jObj["arguments"].(string); ok {
					argsStr = aStr
				} else {
					// Vendor canonical shape: the remaining keys ARE the tool
					// input (cb_tool_name and the cb_easp stop sentinel are
					// envelope params, never arguments — mirror of the
					// vendor's parseToolCallContent delete pair).
					args := make(map[string]any, len(jObj))
					for k, v := range jObj {
						if k == "cb_tool_name" || k == "cb_easp" {
							continue
						}
						args[k] = v
					}
					if b, err := json.Marshal(args); err == nil {
						argsStr = string(b)
					}
				}
				if argsStr == "" {
					argsStr = "{}"
				}
				return &toolCall{
					ID:   "call_" + randHex(12),
					Type: "function",
					Function: toolFunction{
						Name:      name,
						Arguments: argsStr,
					},
				}
			}
		}
	}

	// Try XML format: <function=NAME><parameter=KEY>VAL</parameter></function>
	fnMatch := xmlFunctionHeadRe.FindStringSubmatch(raw)
	if len(fnMatch) >= 2 {
		fnName := strings.TrimSpace(fnMatch[1])
		paramMatches := xmlParamRe.FindAllStringSubmatch(raw, -1)
		argsMap := make(map[string]any)
		for _, pm := range paramMatches {
			pName := pm[1]
			pVal := pm[2]
			if pName == "" && len(pm) > 4 {
				pName = pm[3]
				pVal = pm[4]
			}
			pName = strings.TrimSpace(pName)
			pVal = strings.TrimSpace(pVal)
			argsMap[pName] = pVal
		}
		argsBytes, _ := json.Marshal(argsMap)
		return &toolCall{
			ID:   "call_" + randHex(12),
			Type: "function",
			Function: toolFunction{
				Name:      fnName,
				Arguments: string(argsBytes),
			},
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Streaming XML tool-call extraction (parity with the non-streaming
// accumulator's extractXMLToolCalls at Finish).
//
// Models such as MiMo/Hermes/Qwen emit tool calls as XML/JSON text blocks
// inline in delta.content (<tool_call>, <codebuff_tool_call>,
// <function_call>, <|tool_call_start|>, or fenced JSON) instead of native
// delta.tool_calls. The accumulator can parse a complete response at Finish;
// streaming relays need everything incrementally because a block may span
// many SSE fragments and text BEFORE a block must be relayed immediately.
// ---------------------------------------------------------------------------

// maxStreamXMLBuffer bounds how much content one candidate tool-call block
// may buffer before it is flushed as plain text — false-positive recovery for
// prose that merely mentions an opener tag. A var so tests can shrink it.
var maxStreamXMLBuffer = 64 * 1024

var (
	xmlStreamPipeOpenRe  = regexp.MustCompile(`<\|?tool[_\-]?call[_\-]?start\|?>`)
	xmlStreamPipeCloseRe = regexp.MustCompile(`<\|?tool[_\-]?call[_\-]?end\|?>`)
)

// xmlStreamShape identifies which block form an open candidate belongs to.
type xmlStreamShape int

const (
	xmlShapeNone xmlStreamShape = iota
	xmlShapeToolCall
	xmlShapeToolCalls
	xmlShapeCodebuff
	xmlShapeFunctionCall
	xmlShapePipe
	xmlShapeFence
	// xmlShapePending: buf holds a PARTIAL opener (a fragment ended
	// mid-tag, e.g. "<tool_ca"); the next fragment completes or refutes it.
	xmlShapePending
)

// xmlStreamClosers maps literal block shapes to their closing tag.
var xmlStreamClosers = map[xmlStreamShape]string{
	xmlShapeToolCall:     "</tool_call>",
	xmlShapeToolCalls:    "</tool_calls>",
	xmlShapeCodebuff:     "</codebuff_tool_call>",
	xmlShapeFunctionCall: "</function_call>",
}

// xmlStreamCloserBytes mirrors xmlStreamClosers as byte slices for the hot
// closerEnd lookup (bytes.Index over the pooled buffer — no per-chunk string
// conversion).
var xmlStreamCloserBytes = map[xmlStreamShape][]byte{
	xmlShapeToolCall:     []byte("</tool_call>"),
	xmlShapeToolCalls:    []byte("</tool_calls>"),
	xmlShapeCodebuff:     []byte("</codebuff_tool_call>"),
	xmlShapeFunctionCall: []byte("</function_call>"),
}

// streamBufPool recycles the extractor's per-block accumulation buffer
// (issue #165: cut per-chunk heap allocations). Each stream owns one
// XMLToolCallExtractor and an extractor holds at most one pooled buffer at a
// time, so concurrent streams never touch the same backing array. Buffers
// that outgrew maxStreamXMLBuffer are dropped on release instead of pooled,
// so a pathological block cannot pin an oversized allocation in the pool.
var streamBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 512)
		return &b
	},
}

// acquireStreamBuf borrows a buffer from the pool, reset to length 0.
func acquireStreamBuf() []byte {
	b := *streamBufPool.Get().(*[]byte)
	return b[:0]
}

// releaseStreamBuf returns b to the pool for reuse, or drops it when it
// outgrew maxStreamXMLBuffer (bounded reuse — see streamBufPool).
func releaseStreamBuf(b []byte) {
	if cap(b) > maxStreamXMLBuffer {
		return
	}
	streamBufPool.Put(&b)
}

// XMLToolCallExtractor incrementally converts XML-based tool calls embedded
// in streamed content into native toolCall values. One instance per stream;
// not safe for concurrent use.
//
// Lifecycle: Feed(contentFragment) for every content delta in order; Flush()
// once at stream end (before the terminal frame). Feed returns the text that
// is safe to relay immediately (everything before the earliest candidate
// opener, plus any false-positive block that failed to parse) and any
// completed tool calls. Text inside a candidate block is withheld until the
// block closes or the buffer bound is exceeded.
type XMLToolCallExtractor struct {
	// buf accumulates the open candidate block. It is borrowed from
	// streamBufPool while a block is open and released (nil) once the block
	// closes, the buffer bound trips, or Flush runs.
	buf        []byte
	shape      xmlStreamShape
	fenceBrace int // xmlShapeFence: index in buf just past the opening '{'; -1 while the opener still awaits a '{' from a later fragment
}

// Feed processes one content fragment. It returns the fragment's safe text
// (possibly shorter than the input while a candidate block is open) and any
// tool calls that completed within it.
func (x *XMLToolCallExtractor) Feed(fragment string) (string, []*toolCall) {
	if x.shape == xmlShapePending {
		// A previous fragment ended with a partial opener (e.g. "<tool_ca" +
		// "ll>..."). Give the merged text one full-opener chance before
		// treating the withheld suffix as plain text.
		x.buf = append(x.buf, fragment...)
		s := string(x.buf)
		x.releaseBuf()
		x.shape = xmlShapeNone
		if idx := x.findOpener(s); idx >= 0 {
			fragment = s[idx:]
		} else if n := partialOpenerLen(s); n > 0 {
			// Still an opener prefix mid-tag: keep holding the (bounded)
			// suffix and emit only what precedes it.
			hold := s[len(s)-n:]
			x.buf = acquireStreamBuf()
			x.buf = append(x.buf, hold...)
			x.shape = xmlShapePending
			return s[:len(s)-n], nil
		} else {
			// Refuted: the withheld text was plain prose.
			return s, nil
		}
	}
	if len(x.buf) == 0 && x.findOpener(fragment) < 0 {
		// Fragment ends mid-opener: withhold the partial suffix so an opener
		// split across fragments still extracts (vendor findPartialTagMatch
		// parity). The hold is bounded by the longest opener, and the next
		// fragment emits or completes it — never silently drops it.
		if n := partialOpenerLen(fragment); n > 0 {
			hold := fragment[len(fragment)-n:]
			x.buf = acquireStreamBuf()
			x.buf = append(x.buf, hold...)
			x.shape = xmlShapePending
			return fragment[:len(fragment)-n], nil
		}
		return fragment, nil
	}
	var text strings.Builder
	var calls []*toolCall
	rest := fragment
	for {
		if len(x.buf) > 0 {
			// A candidate block is open: absorb the whole fragment (or the
			// remainder after a block just closed) before looking for its
			// closer — the closer may land in any later fragment.
			x.buf = append(x.buf, rest...)
			rest = ""
		} else {
			idx := x.findOpener(rest)
			if idx < 0 {
				text.WriteString(rest)
				return text.String(), calls
			}
			text.WriteString(rest[:idx])
			x.buf = acquireStreamBuf()
			x.buf = append(x.buf, rest[idx:]...)
			rest = ""
		}
		if end := x.closerEnd(); end >= 0 {
			// Parse the block straight from the pooled buffer; only the
			// remainder crosses a string boundary, and only before the
			// buffer is released — the pool may hand the backing array to
			// another stream's extractor the moment it is returned.
			_, parsed := extractXMLToolCallsBytes(x.buf[:end])
			if len(parsed) > 0 {
				calls = append(calls, parsed...)
			} else {
				text.WriteString(string(x.buf[:end])) // false positive: keep as plain text
			}
			rest = string(x.buf[end:])
			x.releaseBuf()
			x.shape = xmlShapeNone
			continue
		}
		if len(x.buf) > maxStreamXMLBuffer {
			text.WriteString(string(x.buf))
			x.releaseBuf()
			x.shape = xmlShapeNone
			return text.String(), calls
		}
		return text.String(), calls
	}
}

// Flush releases any still-open candidate block at stream end: complete but
// unclosed blocks are still parsed; the remainder is returned as text with
// dangling tool tags scrubbed (mirroring the accumulator's Finish).
func (x *XMLToolCallExtractor) Flush() (string, []*toolCall) {
	if len(x.buf) == 0 {
		return "", nil
	}
	// Parse from the pooled buffer before releasing it back to the pool.
	cleaned, calls := extractXMLToolCallsBytes(x.buf)
	x.releaseBuf()
	x.shape = xmlShapeNone
	if len(calls) > 0 {
		return cleaned, calls
	}
	// No calls parsed: extractXMLToolCallsBytes returned the buffer unchanged,
	// so scrubbing it is equivalent to scrubbing the raw buffered text.
	return danglingToolTagsRe.ReplaceAllString(cleaned, ""), nil
}

// releaseBuf returns the extractor's pooled buffer (if any) to the pool.
func (x *XMLToolCallExtractor) releaseBuf() {
	if b := x.buf; b != nil {
		x.buf = nil
		releaseStreamBuf(b)
	}
}

// findOpener returns the index of the earliest candidate block opener in s,
// or -1. For fenced blocks the opener counts once a '{' (after optional
// json/tool_call tag and whitespace) is visible — in this fragment, or, for
// a qualifying tag fence that ends the fragment, when a later fragment
// supplies the '{' (see xmlStreamFencePending).
func (x *XMLToolCallExtractor) findOpener(s string) int {
	best := -1
	// Literal openers.
	for shape, open := range map[xmlStreamShape]string{
		xmlShapeToolCall:     "<tool_call>",
		xmlShapeToolCalls:    "<tool_calls>",
		xmlShapeCodebuff:     "<codebuff_tool_call>",
		xmlShapeFunctionCall: "<function_call>",
	} {
		if i := strings.Index(s, open); i >= 0 && (best < 0 || i < best) {
			best = i
			x.shape = shape
		}
	}
	// Pipe form: <|tool_call_start|> / <tool_call_start>.
	if loc := xmlStreamPipeOpenRe.FindStringIndex(s); loc != nil && (best < 0 || loc[0] < best) {
		best = loc[0]
		x.shape = xmlShapePipe
	}
	// Fenced JSON: ```json {"name"... (opener counts once a '{' follows the
	// optional tag+whitespace — in this fragment, or via a split fence whose
	// qualifying tag ends the fragment and whose '{' arrives in a later one).
	// Only the earliest qualifying fence can win; a fence that loses to an
	// earlier literal must not clobber the chosen shape.
	for from := 0; from < len(s); {
		i := strings.Index(s[from:], "```")
		if i < 0 {
			break
		}
		i += from
		if brace := xmlStreamFenceBrace([]byte(s), i+3); brace >= 0 {
			if best < 0 || i < best {
				best = i
				x.shape = xmlShapeFence
				// fenceBrace indexes buf, which Feed slices from best —
				// never the fragment s itself (brace and best are both
				// indexes into s; buf starts at best).
				x.fenceBrace = brace - best + 1
			}
			break // later fences are later still; the earliest one decided
		}
		// No '{' visible yet. A fence that ends the fragment with only a
		// qualifying tool-call tag after it (```json\n) may be a split
		// opener: hold it and let a later fragment supply the '{'. Plain
		// code fences (```go, ```py) and fences followed by real content
		// never qualify and are scanned past.
		if xmlStreamFencePending(s, i+3) {
			if best < 0 || i < best {
				best = i
				x.shape = xmlShapeFence
				x.fenceBrace = -1 // awaiting the '{' in a later fragment
			}
			break
		}
		from = i + 3
	}
	return best
}

// xmlStreamFenceBrace returns the index of the '{' that follows a fence
// opener token (optional json/tool_call tag + whitespace), or -1 when the
// fragment ends before a '{' is visible. A plain code fence (```go, ```py)
// never counts as a candidate opener.
func xmlStreamFenceBrace(s []byte, from int) int {
	if from >= len(s) {
		return -1
	}
	pos := from
	for pos < len(s) && (s[pos] == '-' || s[pos] == '_' || s[pos] >= 'a' && s[pos] <= 'z' || s[pos] >= 'A' && s[pos] <= 'Z') {
		pos++ // optional language tag (json, tool_call, tool-call, …)
	}
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\r' || s[pos] == '\n') {
		pos++
	}
	if pos < len(s) && s[pos] == '{' {
		return pos
	}
	return -1
}

// xmlStreamFencePending reports whether the text after a fence is only a
// qualifying tool-call language tag (json/tool_call family) plus whitespace
// and then the end of the fragment — the '{' that completes the opener may
// still arrive in a later fragment. A plain code fence (```go, ```py) or a
// fence followed by real content is never a pending opener.
func xmlStreamFencePending(s string, from int) bool {
	pos := from
	for pos < len(s) && (s[pos] == '-' || s[pos] == '_' || s[pos] >= 'a' && s[pos] <= 'z' || s[pos] >= 'A' && s[pos] <= 'Z') {
		pos++
	}
	if !isStreamFenceTag(s[from:pos]) {
		return false
	}
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\r' || s[pos] == '\n') {
		pos++
	}
	return pos == len(s)
}

// xmlStreamLiteralOpeners lists the literal opener tags that may split
// across fragments (partial-opener withholding). The fence opener is
// handled separately (xmlStreamFencePending / xmlStreamFenceBrace).
var xmlStreamLiteralOpeners = []string{
	"<tool_call>",
	"<tool_calls>",
	"<codebuff_tool_call>",
	"<function_call>",
	"<|tool_call_start|>",
	"<tool_call_start>",
}

// partialOpenerLen returns the length of the longest suffix of s that is a
// PROPER prefix of any literal opener (a complete opener never qualifies),
// or 0. It drives partial-opener withholding: a fragment ending mid-tag
// (e.g. "<tool_ca") is held so a later fragment can complete the opener.
func partialOpenerLen(s string) int {
	best := 0
	for _, open := range xmlStreamLiteralOpeners {
		maxL := len(s)
		if maxL >= len(open) {
			maxL = len(open) - 1
		}
		for l := maxL; l > best; l-- {
			if s[len(s)-l:] == open[:l] {
				best = l
				break
			}
		}
	}
	return best
}

// isStreamFenceTag reports whether a fence language tag is a tool-call
// family tag (json / tool_call variants), as opposed to a plain code
// language (go, py, bash, …) that must never open a block.
func isStreamFenceTag(tag string) bool {
	switch strings.ToLower(tag) {
	case "json", "tool_call", "toolcall", "tool-call", "codebuff_tool_call", "codebuff-tool-call":
		return true
	}
	return false
}

// xmlFenceCloseEnd returns the index just past the first closing fence in s
// at or after from that appears OUTSIDE a JSON string value (quotes toggle
// string state, backslash escapes are honored), or -1. A "```" inside a
// string value (e.g. {"a": "x ``` y"}) must not close the block.
func xmlFenceCloseEnd(s []byte, from int) int {
	inStr := false
	for i := from; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\\' {
				i++ // skip the escaped character
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '`':
			if i+2 < len(s) && s[i+1] == '`' && s[i+2] == '`' {
				return i + 3
			}
		}
	}
	return -1
}

// closerEnd returns the index just past the open candidate's closing tag in
// buf, or -1 while the block is still open.
func (x *XMLToolCallExtractor) closerEnd() int {
	switch x.shape {
	case xmlShapeToolCall, xmlShapeToolCalls, xmlShapeCodebuff, xmlShapeFunctionCall:
		if i := bytes.Index(x.buf, xmlStreamCloserBytes[x.shape]); i >= 0 {
			return i + len(xmlStreamClosers[x.shape])
		}
	case xmlShapePipe:
		if loc := xmlStreamPipeCloseRe.FindIndex(x.buf); loc != nil {
			return loc[1]
		}
	case xmlShapeFence:
		if x.fenceBrace < 0 {
			// Split opener: the '{' has not arrived yet. Confirm it against
			// the accumulated buffer (the fence always sits at index 0 of
			// buf); until the '{' shows, the block is still a candidate
			// and stays open (the 64 KiB bound / Flush recover false hits).
			if brace := xmlStreamFenceBrace(x.buf, 3); brace >= 0 {
				x.fenceBrace = brace + 1
			} else {
				return -1
			}
		}
		if i := xmlFenceCloseEnd(x.buf, x.fenceBrace); i >= 0 {
			return i
		}
	}
	return -1
}

// ToolCallDeltaFragment renders one extracted tool call as a native OpenAI
// streaming delta fragment, ready to append to delta["tool_calls"].
func ToolCallDeltaFragment(index int, tc *toolCall) map[string]any {
	return map[string]any{
		"index": index,
		"id":    tc.ID,
		"type":  tc.Type,
		"function": map[string]any{
			"name":      tc.Function.Name,
			"arguments": tc.Function.Arguments,
		},
	}
}
