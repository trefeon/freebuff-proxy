package wirefacts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Tool-names emitter (Wave D slice 4): derives convert/toolnames_gen.go
// from the verbatim snapshots and verifies the session status envelope.
// Snapshots are read-only: unexpected shapes fail explicitly, never silent.
const (
	toolsConstantsPath = "common/src/tools/constants.ts"
	foreignSignalsPath = "common/src/constants/foreign-client-signals.ts"
	sessionTypesPath   = "common/src/types/freebuff-session.ts"
)

// EmitTools verifies the tool-name sources plus the session envelope at
// upstreamSHA and writes the generated convert table to out (nothing on error).
func EmitTools(upstreamSHA, wireDir, registryDir string, out io.Writer) error {
	_ = registryDir
	raw, err := os.ReadFile(filepath.Join(wireDir, "snapshots.json"))
	if err != nil {
		return fmt.Errorf("wiregen: read snapshots manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("wiregen: parse snapshots manifest: %w", err)
	}
	if upstreamSHA != m.UpstreamSHA {
		return fmt.Errorf("wiregen: -upstream %q does not match manifest upstream_sha %q (testdata/wire/snapshots.json); re-pin the snapshots before regenerating", upstreamSHA, m.UpstreamSHA)
	}
	pinned := make(map[string]string, len(m.Files))
	for _, f := range m.Files {
		pinned[f.Path] = f.SHA256
	}
	mustPinned := func(rel string) ([]byte, error) {
		src, err := os.ReadFile(filepath.Join(wireDir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("wiregen: read wire snapshot %s: %w", rel, err)
		}
		want, ok := pinned[rel]
		if !ok {
			return nil, fmt.Errorf("wiregen: wire snapshot %s is not pinned in snapshots.json at upstream commit %s; pin it before regenerating", rel, m.UpstreamSHA)
		}
		if got := sha256Hex(src); got != want {
			return nil, fmt.Errorf("wiregen: wire snapshot %s hash %s != manifest %s at upstream commit %s; re-pin before regenerating", rel, got, want, m.UpstreamSHA)
		}
		return src, nil
	}
	constsSrc, err := mustPinned(toolsConstantsPath)
	if err != nil {
		return err
	}
	signalsSrc, err := mustPinned(foreignSignalsPath)
	if err != nil {
		return err
	}
	sessionSrc, err := mustPinned(sessionTypesPath)
	if err != nil {
		return err
	}
	// Whole-file drift (added interfaces, new gate codes, prose) already
	// fails at the hash gate above; the parses below pin the semantics the
	// proxy depends on.
	params := make(map[string]string, 3)
	for _, name := range []string{"toolNameParam", "endsAgentStepParam", "toolXmlName"} {
		v, err := parseToolConst(toolsConstantsPath, constsSrc, name, m.UpstreamSHA)
		if err != nil {
			return err
		}
		params[name] = v
	}
	toolNameParam, endsAgentStepParam, toolXMLName := params["toolNameParam"], params["endsAgentStepParam"], params["toolXmlName"]
	generic, err := parseGenericToolNames(signalsSrc, m.UpstreamSHA)
	if err != nil {
		return err
	}
	if err := verifySessionStatuses(sessionSrc, m.UpstreamSHA); err != nil {
		return err
	}
	src, err := format.Source(emitToolsSource(m.UpstreamSHA, toolNameParam, endsAgentStepParam, toolXMLName, generic))
	if err != nil {
		return fmt.Errorf("wiregen: format generated tool names source: %w", err)
	}
	_, err = out.Write(src)
	return err
}

// parseToolConst extracts one `export const <name> = '<word>'` string; any
// other shape fails explicitly so renames get reviewed, never propagated.
func parseToolConst(path string, src []byte, name, commit string) (string, error) {
	fail := func(format string, args ...any) (string, error) {
		return "", fmt.Errorf("wiregen: "+format+" — teach backend/internal/wirefacts/emit_tools.go before regenerating", args...)
	}
	i := strings.Index(string(src), "export const "+name)
	if i < 0 {
		return fail("%s: missing %q at upstream commit %s", path, "export const "+name, commit)
	}
	rest := strings.TrimSpace(string(src[i+len("export const "+name):]))
	if !strings.HasPrefix(rest, "=") {
		return fail("%s:%d: %q is not a const assignment at upstream commit %s", path, lineOf(src, i), name, commit)
	}
	rest = strings.TrimSpace(rest[1:])
	if len(rest) < 3 || rest[0] != '\'' {
		return fail("%s:%d: %q is not a single-quoted string at upstream commit %s", path, lineOf(src, i), name, commit)
	}
	end := strings.IndexByte(rest[1:], '\'')
	if end < 0 {
		return fail("%s:%d: %q has an unterminated string at upstream commit %s", path, lineOf(src, i), name, commit)
	}
	val := rest[1 : 1+end]
	for j := 0; j < len(val); j++ {
		if c := val[j]; c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return fail("%s:%d: %q value %q is not a plain tool token at upstream commit %s", path, lineOf(src, i), name, val, commit)
		}
	}
	if val == "" {
		return fail("%s:%d: %q is empty at upstream commit %s", path, lineOf(src, i), name, commit)
	}
	return val, nil
}

// parseGenericToolNames extracts the GENERIC_TOOL_NAMES set entries; a
// spread or non-literal fails explicitly instead of a partial table.
func parseGenericToolNames(src []byte, commit string) ([]string, error) {
	const path = foreignSignalsPath
	i := strings.Index(string(src), "GENERIC_TOOL_NAMES")
	if i < 0 {
		return nil, fmt.Errorf("wiregen: %s: missing GENERIC_TOOL_NAMES at upstream commit %s — teach backend/internal/wirefacts/emit_tools.go before regenerating", path, commit)
	}
	set := strings.Index(string(src[i:]), "new Set([")
	if set < 0 {
		return nil, fmt.Errorf("wiregen: %s:%d: GENERIC_TOOL_NAMES is not a new Set([...]) literal at upstream commit %s — teach backend/internal/wirefacts/emit_tools.go before regenerating", path, lineOf(src, i), commit)
	}
	span, ok := bracketSpan(string(src[i+set+len("new Set(["):]), '[', ']')
	if !ok {
		return nil, fmt.Errorf("wiregen: %s:%d: GENERIC_TOOL_NAMES set literal never closes at upstream commit %s — teach backend/internal/wirefacts/emit_tools.go before regenerating", path, lineOf(src, i), commit)
	}
	span = stripTSComments(span)
	var names []string
	for j := 0; j < len(span); {
		if span[j] != '\'' {
			if span[j] == '.' {
				return nil, fmt.Errorf("wiregen: %s:%d: GENERIC_TOOL_NAMES has a non-literal entry at upstream commit %s — teach backend/internal/wirefacts/emit_tools.go before regenerating", path, lineOf(src, i), commit)
			}
			j++
			continue
		}
		k := strings.IndexByte(span[j+1:], '\'')
		if k < 0 {
			return nil, fmt.Errorf("wiregen: %s:%d: GENERIC_TOOL_NAMES has an unterminated string at upstream commit %s — teach backend/internal/wirefacts/emit_tools.go before regenerating", path, lineOf(src, i), commit)
		}
		names = append(names, span[j+1:j+1+k])
		j += 1 + k + 1
	}
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if n == "" || seen[n] {
			return nil, fmt.Errorf("wiregen: %s:%d: GENERIC_TOOL_NAMES has an empty or duplicate entry at upstream commit %s — teach backend/internal/wirefacts/emit_tools.go before regenerating", path, lineOf(src, i), commit)
		}
		seen[n] = true
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("wiregen: %s:%d: GENERIC_TOOL_NAMES parsed empty at upstream commit %s — refusing to emit an empty table", path, lineOf(src, i), commit)
	}
	sort.Strings(names)
	return names, nil
}

// pinnedSessionStatuses is the session status envelope at the current
// upstream SHA: every status: '<literal>' variant clients switch on. Unknown
// fails (upstream added a shape we do not handle), missing fails (upstream
// removed one we may reference). The purchase_* trio are Desktop purchase-
// flow admission shapes (78a7ab4); they ride the default TokenOK path at
// runtime, never a WireCode.
var pinnedSessionStatuses = []string{
	"none", "active", "ended", "country_blocked", "model_locked",
	"model_unavailable", "banned", "ip_capped", "rate_limited",
	"spend_limited", "premium_slot_taken", "superseded",
	"purchase_claim_released", "purchase_in_use", "purchase_capacity",
}

// verifySessionStatuses checks the status envelope; it emits nothing and
// fails explicitly on drift.
func verifySessionStatuses(src []byte, commit string) error {
	const path = sessionTypesPath
	code := stripTSComments(string(src))
	want := make(map[string]bool, len(pinnedSessionStatuses))
	for _, p := range pinnedSessionStatuses {
		want[p] = true
	}
	seen := make(map[string]bool)
	check := func(name string) error {
		if seen[name] {
			return nil
		}
		seen[name] = true
		if !want[name] {
			return fmt.Errorf("wiregen: %s:%d: unknown session status %q at upstream commit %s — teach backend/internal/wirefacts/emit_tools.go before regenerating", path, lineOf([]byte(code), strings.Index(code, "'"+name+"'")), name, commit)
		}
		return nil
	}
	for i := 0; i < len(code); {
		j := strings.Index(code[i:], "status:")
		if j < 0 {
			break
		}
		i += j + len("status:")
		// One status: may carry same-line union arms
		// (status: 'purchase_in_use' | 'purchase_capacity'): every arm is
		// envelope, so follow '|' continuations instead of checking the
		// head literal only.
		for {
			rest := strings.TrimLeft(code[i:], " \t")
			i += len(code[i:]) - len(rest)
			if !strings.HasPrefix(rest, "'") {
				break
			}
			end := strings.IndexByte(rest[1:], '\'')
			if end < 0 {
				break
			}
			if err := check(rest[1 : 1+end]); err != nil {
				return err
			}
			i += 1 + end + 1
			tail := strings.TrimLeft(code[i:], " \t")
			if !strings.HasPrefix(tail, "|") {
				i += len(code[i:]) - len(tail)
				break
			}
			i += len(code[i:]) - len(tail) + 1
		}
	}
	for _, p := range pinnedSessionStatuses {
		if !seen[p] {
			return fmt.Errorf("wiregen: %s: pinned session status %q missing at upstream commit %s — teach backend/internal/wirefacts/emit_tools.go before regenerating", path, p, commit)
		}
	}
	return nil
}

// bracketSpan returns the interior of the first balanced pair in s.
func bracketSpan(s string, open, close byte) (string, bool) {
	depth := 1
	for j := 0; j < len(s); j++ {
		switch s[j] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[:j], true
			}
		}
	}
	return "", false
}

// stripTSComments removes // and /* */ comments (newlines preserved, so
// line numbers still match) while keeping quoted comment markers as code.
func stripTSComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '\'' || c == '"' || c == '`' {
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == c {
					j++
					break
				}
				j++
			}
			b.WriteString(s[i:j])
			i = j
			continue
		}
		if c == '/' && i+1 < len(s) {
			if s[i+1] == '/' {
				for i < len(s) && s[i] != '\n' {
					i++
				}
				continue
			}
			if s[i+1] == '*' {
				i += 2
				for i+1 < len(s) && (s[i] != '*' || s[i+1] != '/') {
					if s[i] == '\n' {
						b.WriteByte('\n')
					}
					i++
				}
				i += 2
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func lineOf(src []byte, off int) int {
	if off < 0 {
		return 1
	}
	if off > len(src) {
		off = len(src)
	}
	return 1 + bytes.Count(src[:off], []byte{'\n'})
}

// emitToolsSource renders the generated table from parsed values only.
func emitToolsSource(upstreamSHA, toolNameParam, endsAgentStepParam, toolXMLName string, generic []string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `// Code generated by cmd/wiregen -tools-out at upstream %s; DO NOT EDIT.
// Sources (verbatim snapshots, never edited):
//
//	%s (toolNameParam, endsAgentStepParam, toolXmlName)
//	%s (GENERIC_TOOL_NAMES)
//
// Session envelope verified, no code emitted:
//
//	%s
package convert

// ToolNameParam mirrors upstream toolNameParam: the JSON key carrying the
// tool name inside a codebuff_tool_call payload.
const ToolNameParam = %q

// EndsAgentStepParam mirrors upstream endsAgentStepParam: the stop sentinel
// inside a codebuff_tool_call payload, never a tool argument.
const EndsAgentStepParam = %q

// ToolXMLName mirrors upstream toolXmlName: the canonical XML tag for
// vendor tool calls in model output.
const ToolXMLName = %q

// GenericToolNames mirrors upstream GENERIC_TOOL_NAMES: tool names we define
// that third-party harnesses also ship, so they carry no signature weight in
// the foreign-client gate.
var GenericToolNames = map[string]bool{
`, upstreamSHA, toolsConstantsPath, foreignSignalsPath, sessionTypesPath, toolNameParam, endsAgentStepParam, toolXMLName)
	for _, n := range generic {
		fmt.Fprintf(&b, "\t%q: true,\n", n)
	}
	b.WriteString("}\n")
	return []byte(b.String())
}
