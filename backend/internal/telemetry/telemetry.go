// Package telemetry provides the leveled color/file logger, redacted header
// copies, and optional request dumps for the freebuff-proxy bridge
// (PRD §3: structured logging — color terminal + file, debug dump mode).
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"freebuff-proxy/backend/internal/config"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ANSI 4-color scheme: DEBUG gray, INFO green, WARN yellow, ERROR red.
const (
	ansiGray   = "\x1b[90m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
	ansiReset  = "\x1b[0m"
)

// timeFormat mirrors slog's text handler timestamp.
const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// LevelTrace is the most verbose log level, one step below debug. slog has
// no built-in trace level; -8 sits below LevelDebug (-4). slog's String()
// renders it "DEBUG-4", so the handlers and the startup banner print TRACE
// explicitly (see levelName). Defined in config (the bottom layer validates
// LOG_LEVEL without importing telemetry); this alias keeps the
// telemetry-level name for the logging API.
const LevelTrace = config.LevelTrace

// NewLogger builds the process logger at Info level, or Debug when verbose.
// It is a convenience wrapper over New keeping the original API (text format).
func NewLogger(verbose bool, logFile string) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return New(level, logFile, "text")
}

// ParseLevel parses a LOG_LEVEL-style string into a slog level. The level
// table is owned by config (bottom layer); this wrapper forwards so the
// telemetry logging API and its callers are unchanged. The empty string
// returns ok=false (caller falls back to its default). "trace"
// (case-insensitive) maps to LevelTrace; the four slog names are accepted
// as before.
func ParseLevel(s string) (slog.Level, bool) {
	return config.ParseLevel(s)
}

// New builds the process logger at the given level. stderr gets the
// colorized text handler; when logFile is set the same lines are appended
// there via io.MultiWriter. Coloring is disabled only when a log file is
// actually opened — a single handler writes to both sinks and ANSI escapes
// in a file are noise. A log file that cannot be opened is reported on
// stderr and stderr-only logging continues, keeping its colors.
//
// format selects the handler: "json" writes one JSON object per record
// (real group nesting); anything else — including "" — is the text format.
func New(level slog.Level, logFile string, format string) *slog.Logger {
	w := io.Writer(os.Stderr)
	var file *os.File
	if logFile != "" {
		// Create the parent directory so LOG_FILE may point into a nested
		// path (e.g. ./logs/proxy.log) without a pre-existing tree. Errors
		// are ignored here: OpenFile below fails with its own report and the
		// stderr-only fallback keeps logging.
		if dir := filepath.Dir(logFile); dir != "" && dir != "." {
			_ = os.MkdirAll(dir, 0o755)
		}
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "freebuff-proxy: warning: cannot open log file %s: %v\n", logFile, err)
		} else {
			file = f
			w = io.MultiWriter(os.Stderr, f)
		}
	}

	switch format {
	case "json":
		return slog.New(&jsonHandler{w: w, level: level, file: file})
	default:
		h := &textHandler{w: w, level: level, colorize: file == nil, file: file}
		return slog.New(h)
	}
}

// levelName renders a level token. LevelTrace prints as TRACE instead of
// slog's "DEBUG-4" (the level is below DEBUG, so slog's String() appends the
// negative offset); every other level keeps slog's exact rendering.
func levelName(level slog.Level) string {
	if level == LevelTrace {
		return "TRACE"
	}
	return level.String()
}

// textHandler is a minimal slog text handler that colorizes the level token
// (time=... level=INFO msg=... key=value...). WithAttrs/WithGroup are
// copy-on-write: each returns a new handler with the base attrs/groups
// extended, so a handler never mutates the attrs it was built from. Bound
// attrs are rendered at the group depth active when they were bound —
// record attrs sit under all handler groups — mirroring slog's text
// handler (e.g. WithAttrs(pre).WithGroup("s") logs "pre=3 s.a=one"). file
// is the appended log file (nil for stderr-only), kept for tests and a
// future shutdown close.
type textHandler struct {
	mu       sync.Mutex
	level    slog.Leveler
	w        io.Writer
	colorize bool
	file     *os.File
	// attrs holds handler attrs bound via WithAttrs, each under the group
	// depth active at bind time (len(h.groups) is the current depth).
	attrs  []boundAttrs
	groups []string
}

// boundAttrs is one WithAttrs batch together with the group depth it was
// bound under.
type boundAttrs struct {
	depth int
	attrs []slog.Attr
}

func (h *textHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	line := fmt.Sprintf("time=%s level=%s msg=%s",
		r.Time.Format(timeFormat), h.levelToken(r.Level), quoteMessage(r.Message))
	if len(h.attrs) == 0 && len(h.groups) == 0 {
		// Fast path (the process logger never binds attrs): no allocation,
		// byte-identical to the pre-WithAttrs handler.
		r.Attrs(func(a slog.Attr) bool {
			line += " " + a.Key + "=" + quoteMessage(a.Value.String())
			return true
		})
	} else {
		for _, ba := range h.attrs {
			line = appendTextAttrs(line, strings.Join(h.groups[:ba.depth], "."), ba.attrs)
		}
		line = appendTextAttrs(line, strings.Join(h.groups, "."), collectAttrs(r))
	}
	_, err := io.WriteString(h.w, line+"\n")
	return err
}

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	// Explicit field-wise clone: copying the struct would copy the mutex
	// (govet copylocks). The clone gets a fresh zero mutex.
	return &textHandler{
		level:    h.level,
		w:        h.w,
		colorize: h.colorize,
		file:     h.file,
		attrs:    append(append([]boundAttrs{}, h.attrs...), boundAttrs{depth: len(h.groups), attrs: attrs}),
		groups:   append([]string{}, h.groups...),
	}
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &textHandler{
		level:    h.level,
		w:        h.w,
		colorize: h.colorize,
		file:     h.file,
		attrs:    append([]boundAttrs{}, h.attrs...),
		groups:   append(append([]string{}, h.groups...), name),
	}
}

// collectAttrs returns the record's attrs as a slice.
func collectAttrs(r slog.Record) []slog.Attr {
	attrs := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	return attrs
}

// appendTextAttrs appends " key=value" pairs for attrs to line. When prefix
// is non-empty every key is rendered prefix.key; record group attrs extend
// the prefix (g.k=v), mirroring slog's text handler. Empty attrs and empty
// groups are skipped.
func appendTextAttrs(line, prefix string, attrs []slog.Attr) string {
	for _, a := range attrs {
		if a.Equal(slog.Attr{}) {
			continue
		}
		if a.Value.Kind() == slog.KindGroup {
			group := a.Value.Group()
			if len(group) == 0 {
				continue
			}
			line = appendTextAttrs(line, joinKey(prefix, a.Key), group)
			continue
		}
		line += " " + joinKey(prefix, a.Key) + "=" + quoteMessage(a.Value.String())
	}
	return line
}

// joinKey prefixes key with prefix using slog's dotted group notation
// ("group.key"); prefix "" returns the key unchanged.
func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// FormatAttrValue renders an attribute value the way the text handler does,
// applying the shared quoting policy: strings go through quoteMessage, so an
// embedded newline/quote/space cannot forge an extra log line in any sink
// that reuses this renderer (issue #284).
func FormatAttrValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return quoteMessage(v.String())
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'g', -1, 64)
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	default:
		return v.String()
	}
}

// FormatAttrPair renders one attribute as "key=value" with the shared
// quoting policy. prefix is the dotted group prefix ("" leaves key bare).
func FormatAttrPair(prefix, key string, v slog.Value) string {
	return joinKey(prefix, key) + "=" + FormatAttrValue(v)
}

// FlattenAttrs renders an attr slice as "key=value" pairs (group keys
// dotted, empty groups and empty attrs skipped), applying the shared quoting
// policy. logring uses this to retain one dashboard field per attr with the
// same rendering the terminal/file sink uses (issue #284).
func FlattenAttrs(prefix string, attrs []slog.Attr) []string {
	var out []string
	for _, a := range attrs {
		if a.Equal(slog.Attr{}) {
			continue
		}
		if a.Value.Kind() == slog.KindGroup {
			group := a.Value.Group()
			if len(group) == 0 {
				continue
			}
			key := prefix
			if a.Key != "" {
				key = a.Key
				if prefix != "" {
					key = prefix + "." + a.Key
				}
			}
			out = append(out, FlattenAttrs(key, group)...)
			continue
		}
		out = append(out, FormatAttrPair(prefix, a.Key, a.Value))
	}
	return out
}

// levelToken renders the level marker, colorized unless the sink includes a
// file (ANSI escapes in a log file are noise).
func (h *textHandler) levelToken(level slog.Level) string {
	if !h.colorize {
		return levelName(level)
	}
	return levelColor(level) + levelName(level) + ansiReset
}

func levelColor(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return ansiRed
	case level >= slog.LevelWarn:
		return ansiYellow
	case level >= slog.LevelInfo:
		return ansiGreen
	default:
		return ansiGray
	}
}

// jsonHandler is a minimal slog JSON handler: one valid JSON object per
// record with real nesting for WithGroup and group attrs. Never colorized
// (ANSI escapes would corrupt the JSON). WithAttrs/WithGroup are
// copy-on-write like the text handler's; bound attrs are written at the
// group depth active when they were bound, record attrs under all handler
// groups, mirroring slog's JSON handler.
type jsonHandler struct {
	mu     sync.Mutex
	level  slog.Leveler
	w      io.Writer
	file   *os.File
	attrs  []boundAttrs
	groups []string
}

func (h *jsonHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *jsonHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	// Explicit field-wise clone: copying the struct would copy the mutex
	// (govet copylocks). The clone gets a fresh zero mutex.
	return &jsonHandler{
		level:  h.level,
		w:      h.w,
		file:   h.file,
		attrs:  append(append([]boundAttrs{}, h.attrs...), boundAttrs{depth: len(h.groups), attrs: attrs}),
		groups: append([]string{}, h.groups...),
	}
}

func (h *jsonHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &jsonHandler{
		level:  h.level,
		w:      h.w,
		file:   h.file,
		attrs:  append([]boundAttrs{}, h.attrs...),
		groups: append(append([]string{}, h.groups...), name),
	}
}

func (h *jsonHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	var w jsonWriter
	w.sep = []bool{false}
	w.buf.WriteByte('{')
	w.writePair("time", slog.StringValue(r.Time.Format(timeFormat)))
	w.writePair("level", slog.StringValue(levelName(r.Level)))
	w.writePair("msg", slog.StringValue(r.Message))

	// Handler attrs (bound at their group depth), then record attrs under
	// all handler groups. Group depths only grow, so groups opened for one
	// batch stay open for the next.
	openDepth := 0
	var opened []groupFrame
	writeBatch := func(depth int, attrs []slog.Attr) {
		for openDepth < depth {
			r0, rs, pp := w.openObject(h.groups[openDepth])
			opened = append(opened, groupFrame{restore: r0, restoreSep: rs, parentPrior: pp})
			openDepth++
		}
		w.writeAttrs(attrs)
	}
	for _, ba := range h.attrs {
		writeBatch(ba.depth, ba.attrs)
	}
	writeBatch(len(h.groups), collectAttrs(r))
	for len(opened) > 0 {
		f := opened[len(opened)-1]
		opened = opened[:len(opened)-1]
		w.closeObject(f.restore, f.restoreSep, f.parentPrior)
	}
	w.buf.WriteByte('}')
	w.buf.WriteByte('\n')
	_, err := io.WriteString(h.w, w.buf.String())
	return err
}

// groupFrame records where a handler group was opened so it can be closed
// — or rewound entirely — when the batch ends.
type groupFrame struct {
	restore     int
	restoreSep  int
	parentPrior bool
}

// jsonWriter emits a JSON object incrementally. sep tracks, per nesting
// depth, whether an element has already been written at that depth (a comma
// is then required before the next one).
type jsonWriter struct {
	buf bytes.Buffer
	sep []bool
}

// beforeValue writes the separator for the next element at the current
// depth and marks the depth non-empty.
func (w *jsonWriter) beforeValue() {
	if w.sep[len(w.sep)-1] {
		w.buf.WriteByte(',')
	}
	w.sep[len(w.sep)-1] = true
}

func (w *jsonWriter) writePair(key string, v slog.Value) {
	w.beforeValue()
	writeJSONString(&w.buf, key)
	w.buf.WriteByte(':')
	writeJSONValue(&w.buf, v)
}

// openObject writes "key":{ at the current depth and descends. It returns
// buffer and sep-stack positions — plus the parent's pre-open separator
// state — so closeObject can rewind the object if it turns out empty (slog
// suppresses empty groups).
func (w *jsonWriter) openObject(key string) (restore, restoreSep int, parentPrior bool) {
	restore = w.buf.Len()
	restoreSep = len(w.sep)
	parentPrior = restoreSep > 0 && w.sep[restoreSep-1]
	w.beforeValue()
	writeJSONString(&w.buf, key)
	w.buf.WriteByte(':')
	w.buf.WriteByte('{')
	w.sep = append(w.sep, false)
	return restore, restoreSep, parentPrior
}

// closeObject finishes an object opened by openObject: when the object
// received at least one element it is closed with '}'; an empty object is
// removed entirely (buffer and sep stack rewound, including the parent's
// separator state, since the comma before it was rewound too) so no empty
// groups leak into the output.
func (w *jsonWriter) closeObject(restore, restoreSep int, parentPrior bool) {
	if w.sep[len(w.sep)-1] {
		w.buf.WriteByte('}')
		w.sep = w.sep[:len(w.sep)-1]
		return
	}
	w.buf.Truncate(restore)
	w.sep = w.sep[:restoreSep]
	w.sep[restoreSep-1] = parentPrior
}

// writeAttrs writes attrs into the object at the current depth. Group
// attrs open nested objects (rewound when empty), giving JSON handlers
// their real nesting.
func (w *jsonWriter) writeAttrs(attrs []slog.Attr) {
	for _, a := range attrs {
		if a.Equal(slog.Attr{}) {
			continue
		}
		if a.Value.Kind() == slog.KindGroup {
			group := a.Value.Group()
			if len(group) == 0 {
				continue
			}
			restore, restoreSep, parentPrior := w.openObject(a.Key)
			w.writeAttrs(group)
			w.closeObject(restore, restoreSep, parentPrior)
			continue
		}
		w.writePair(a.Key, a.Value.Resolve())
	}
}

// writeJSONValue writes v as its JSON representation. Strings are quoted
// and escaped; numbers and booleans are raw; durations and times render as
// strings (RFC3339-ms for times, matching the record time); Any values go
// through json.Marshal with a string fallback when they cannot marshal.
func writeJSONValue(buf *bytes.Buffer, v slog.Value) {
	switch v.Kind() {
	case slog.KindString:
		writeJSONString(buf, v.String())
	case slog.KindInt64:
		buf.WriteString(strconv.FormatInt(v.Int64(), 10))
	case slog.KindUint64:
		buf.WriteString(strconv.FormatUint(v.Uint64(), 10))
	case slog.KindFloat64:
		if f := v.Float64(); math.IsNaN(f) || math.IsInf(f, 0) {
			buf.WriteString("null")
		} else {
			buf.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
		}
	case slog.KindBool:
		if v.Bool() {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case slog.KindDuration:
		writeJSONString(buf, v.Duration().String())
	case slog.KindTime:
		writeJSONString(buf, v.Time().Format(timeFormat))
	case slog.KindAny:
		data, err := json.Marshal(v.Any())
		if err != nil {
			writeJSONString(buf, fmt.Sprint(v.Any()))
			return
		}
		buf.Write(data)
	}
}

// writeJSONString writes s as a quoted, escaped JSON string. json.Marshal
// of a string cannot fail; invalid UTF-8 is replaced with U+FFFD, so the
// output is always valid JSON.
func writeJSONString(buf *bytes.Buffer, s string) {
	data, _ := json.Marshal(s)
	buf.Write(data)
}

// quoteMessage quotes multi-word messages so one line stays one record.
// Values containing quotes, newlines, tabs, carriage returns or other
// control characters are quoted too: an attr value (model name, URL path)
// with an embedded newline — or an injected "level=ERROR" token — would
// otherwise forge additional log lines, and a trailing \r corrupts the
// appended log file. strconv.Quote escapes all of them safely.
func quoteMessage(msg string) string {
	if needsQuote(msg) {
		return strconv.Quote(msg)
	}
	return msg
}

// needsQuote reports whether s contains characters that would break
// one-record-per-line logging when written unquoted.
func needsQuote(s string) bool {
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '"' || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// sensitiveHeaders are redacted in dumps and request logs; keys are compared
// lower-cased so direct (non-canonical) header assignments are covered too.
var sensitiveHeaders = map[string]struct{}{
	"authorization":      {},
	"x-api-key":          {},
	"x-codebuff-api-key": {},
	"cookie":             {},
	"set-cookie":         {},
}

// RedactHeaders returns a copy of h with the values of sensitive headers
// (Authorization, x-api-key, x-codebuff-api-key, Cookie, Set-Cookie, and
// every x-freebuff-* header) replaced by "[redacted]". The input header is
// not modified.
func RedactHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		copied := make([]string, len(vs))
		copy(copied, vs)
		if isSensitiveHeader(k) {
			for i := range copied {
				copied[i] = "[redacted]"
			}
		}
		out[k] = copied
	}
	return out
}

// isSensitiveHeader reports whether a header key must be redacted: the
// fixed secret set plus every x-freebuff-* header (session tokens, instance
// ids, model and acting-user metadata are all sensitive request context).
func isSensitiveHeader(k string) bool {
	lower := strings.ToLower(k)
	if _, ok := sensitiveHeaders[lower]; ok {
		return true
	}
	return strings.HasPrefix(lower, "x-freebuff-")
}

// cbTokenRE matches FreeBuff token values (the cb_ prefix the CLI mints;
// the payload uses the same base64url-plus-punctuation alphabet as Bearer
// tokens, so the charset must match bearerTokenRE's).
var cbTokenRE = regexp.MustCompile(`cb_[A-Za-z0-9._~+/=-]+`)

// bearerTokenRE matches Authorization-style "Bearer <token>" sequences
// (tokens are base64url + . _ ~ + / = characters).
var bearerTokenRE = regexp.MustCompile(`Bearer [A-Za-z0-9._~+/=-]+`)

// RedactSecrets replaces FreeBuff token values embedded in s with
// "[redacted]": cb_-prefixed tokens and "Bearer <token>" sequences, both
// anywhere in the string. Apply it to every logged upstream body so raw
// token material can never reach the log sink.
func RedactSecrets(s string) string {
	s = cbTokenRE.ReplaceAllString(s, "[redacted]")
	s = bearerTokenRE.ReplaceAllString(s, "[redacted]")
	return s
}

// sanitizeName makes a request path safe to embed in a dump file name on
// every platform: separators, dots and each character that is invalid in
// Windows file names are replaced with underscores. The 60-rune cap is
// truncation-safe: a byte slice cut could split a multi-byte UTF-8 sequence
// and produce an invalid file name, so the cap counts runes instead.
func sanitizeName(p string) string {
	for _, r := range `/\:*?"<>|.` {
		p = strings.ReplaceAll(p, string(r), "_")
	}
	if r := []rune(p); len(r) > 60 {
		p = string(r[:60])
	}
	return p
}

// truncate shortens s to at most n runes (UTF-8-safe: multi-byte sequences
// are never split) plus an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
