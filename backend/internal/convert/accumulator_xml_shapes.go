package convert

import (
	"encoding/json"
	"regexp"
	"strings"
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
// ToolXMLName is the upstream's own canonical XML tag (issue #144; generated
// in toolnames_gen.go from common/src/tools/constants.ts — the CLI's stream
// parser util/stream-xml-parser.ts extracts exactly this tag from model output).
var (
	xmlToolCallBlockRe = regexp.MustCompile(`(?s)<tool_call>(.*?)</tool_call>|<` + ToolXMLName + `>(.*?)</` + ToolXMLName + `>|<function_call>(.*?)</function_call>|<\|?tool[_\-]?call[_\-]?start\|?>(.*?)<\|?tool[_\-]?call[_\-]?end\|?>|<tool_calls>(.*?)</tool_calls>`)
	fencedToolCallRe   = regexp.MustCompile("(?s)```(?:json|tool_?call)?\\s*\\n?(\\{\\s*\"(?:name|function|" + ToolNameParam + ")\"\\s*:\\s*.*?\\})\\s*\\n?```")
	xmlFunctionHeadRe  = regexp.MustCompile(`(?i)<function[=\s]+["']?([^>"\s]+)["']?>`)
	xmlParamRe         = regexp.MustCompile(`(?s)<parameter[=\s]+["']?([^>"\s]+)["']?>(.*?)</parameter>|<param[=\s]+["']?([^>"\s]+)["']?>(.*?)</param>`)
	danglingToolTagsRe = regexp.MustCompile(`(?i)</?(?:tool_call|tool_calls|` + "` + ToolXMLName + `" + `|function_call|function|parameter|param|\|?tool[_\-]?call[_\-]?(?:start|end)\|?)(?:[=\s][^>]*)?>`)
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
	// or the vendor's canonical ToolXMLName JSON keyed by ToolNameParam
	// (generated in toolnames_gen.go from common/src/tools/constants.ts):
	// {"cb_tool_name":"bash","command":"pwd","cb_easp":true}.
	if strings.HasPrefix(raw, "{") && strings.HasSuffix(raw, "}") {
		var jObj map[string]any
		if err := json.Unmarshal([]byte(raw), &jObj); err == nil {
			name, _ := jObj["name"].(string)
			if name == "" {
				name, _ = jObj[ToolNameParam].(string)
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
					// input (ToolNameParam and the EndsAgentStepParam stop
					// sentinel are envelope params, never arguments — mirror
					// of the vendor's parseToolCallContent delete pair).
					args := make(map[string]any, len(jObj))
					for k, v := range jObj {
						if k == ToolNameParam || k == EndsAgentStepParam {
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
	xmlShapeCodebuff:     "</" + ToolXMLName + ">",
	xmlShapeFunctionCall: "</function_call>",
}

// xmlStreamCloserBytes mirrors xmlStreamClosers as byte slices for the hot
// closerEnd lookup (bytes.Index over the pooled buffer — no per-chunk string
// conversion).
var xmlStreamCloserBytes = map[xmlStreamShape][]byte{
	xmlShapeToolCall:     []byte("</tool_call>"),
	xmlShapeToolCalls:    []byte("</tool_calls>"),
	xmlShapeCodebuff:     []byte("</" + ToolXMLName + ">"),
	xmlShapeFunctionCall: []byte("</function_call>"),
}

// xmlStreamLiteralOpeners lists the literal opener tags that may split
// across fragments (partial-opener withholding). The fence opener is
// handled separately (xmlStreamFencePending / xmlStreamFenceBrace).
var xmlStreamLiteralOpeners = []string{
	"<tool_call>",
	"<tool_calls>",
	"<" + ToolXMLName + ">",
	"<function_call>",
	"<|tool_call_start|>",
	"<tool_call_start>",
}
