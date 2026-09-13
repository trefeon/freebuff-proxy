package dashboard

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/store"
)

// --- logs ---

// Console window bounds. logConsoleLimit is the row ceiling one response may
// carry (a busy hour can hold far more rows than anyone reads, and the ring
// merge must not emit an unbounded payload); it sits well under the store's
// maxLogLimit. Window values are clamped into [floor, ceiling]: below the
// floor the view is useless, above the ceiling the query scans for rows the
// operator cannot read anyway.
const (
	logConsoleLimit          = 2000
	minLogConsoleWindowQuery = time.Minute
	maxLogConsoleWindowQuery = 7 * 24 * time.Hour
)

type logsData struct {
	Enabled   bool   `json:"enabled"`
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	HasFilter bool   `json:"has_filter"`
	// Window is the effective view window (Go duration string) this response
	// covers, so the UI can label what it is showing. Truncated reports that
	// the row ceiling was hit and older rows inside Window are not shown.
	Window    string     `json:"window"`
	Truncated bool       `json:"truncated"`
	Entries   []logEntry `json:"entries"`
}

type logEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
	Fields  string `json:"fields"`
}

// consoleWindow resolves the view window for one request. The ?window= query
// parameter (Go duration) overrides the LOG_CONSOLE_WINDOW knob for that
// request; absent, unparsable, or non-positive values fall back to the knob,
// and the result is clamped into the documented range. It is a VIEW window
// only: the spill keeps storing everything LOG_TABLE_RETENTION allows.
func (d *Dashboard) consoleWindow(r *http.Request) time.Duration {
	window := config.DefaultLogConsoleWindow
	if d.cfg != nil {
		if v := d.cfg().LogConsoleWindow; v > 0 {
			window = v
		}
	}
	if r != nil && r.URL != nil {
		if raw := strings.TrimSpace(r.URL.Query().Get("window")); raw != "" {
			if v, err := time.ParseDuration(raw); err == nil && v > 0 {
				window = v
			}
		}
	}
	if window < minLogConsoleWindowQuery {
		window = minLogConsoleWindowQuery
	}
	if window > maxLogConsoleWindowQuery {
		window = maxLogConsoleWindowQuery
	}
	return window
}

// logsData merges the live ring with the history store so the console view
// survives restarts: ring rows come first (newest-first), then DB-only rows
// from inside the view window fill up to the row ceiling. The spill path
// round-trips ring timestamps at second precision, so the dedupe key is
// exact — a row already spilled to the DB never renders twice.
func (d *Dashboard) logsData(r *http.Request) logsData {
	ld := logsData{Enabled: d.logs != nil || d.hist != nil}
	window := d.consoleWindow(r)
	ld.Window = window.String()
	level := ""
	msg := ""
	if r != nil && r.URL != nil {
		level = strings.TrimSpace(r.URL.Query().Get("level"))
		msg = strings.TrimSpace(r.URL.Query().Get("msg"))
	}
	ld.Level = strings.ToLower(level)
	ld.Msg = msg
	ld.HasFilter = level != "" || msg != ""
	msgLower := strings.ToLower(msg)
	seen := map[string]bool{}
	if d.logs != nil {
		for _, e := range d.logs.Recent(logConsoleLimit) {
			if level != "" && !strings.EqualFold(e.Level, level) {
				continue
			}
			if msg != "" && !strings.Contains(strings.ToLower(e.Message), msgLower) {
				continue
			}
			seen[logDedupeKey(e.Time, e.Level, e.Message, e.Fields)] = true
			ld.Entries = append(ld.Entries, logEntry{
				Time:    e.Time,
				Level:   e.Level,
				Message: e.Message,
				Fields:  strings.Join(e.Fields, "  "),
			})
		}
	}
	if d.hist != nil && len(ld.Entries) < logConsoleLimit {
		rows, err := d.hist.QueryLogs(store.LogFilter{
			Since:    store.Millis(time.Now().Add(-window)),
			Level:    level,
			Contains: msg,
			Limit:    logConsoleLimit,
		})
		if err != nil {
			d.logger.Warn("logs query failed", "err", err)
			return ld
		}
		for _, e := range rows {
			fields := strings.Split(e.Fields, "\n")
			ts := time.UnixMilli(e.TS).UTC().Format(time.RFC3339)
			if seen[logDedupeKey(ts, e.Level, e.Msg, fields)] {
				continue
			}
			ld.Entries = append(ld.Entries, logEntry{
				Time:    ts,
				Level:   e.Level,
				Message: e.Msg,
				Fields:  strings.Join(fields, "  "),
			})
			if len(ld.Entries) >= logConsoleLimit {
				break
			}
		}
	}
	ld.Truncated = len(ld.Entries) >= logConsoleLimit
	return ld
}

// logDedupeKey identifies one log row across the ring and DB forms. Ring
// timestamps render in local time while DB rows render in UTC, so both
// sides normalize to epoch seconds (the ring renders at second precision
// and the spill preserves it exactly); level matches case-insensitively.
func logDedupeKey(timeStr, level, msg string, fields []string) string {
	ts := timeStr
	if t, err := time.Parse(time.RFC3339, timeStr); err == nil {
		ts = strconv.FormatInt(t.Unix(), 10)
	}
	return ts + "|" + strings.ToLower(level) + "|" + msg + "|" + strings.Join(fields, "\n")
}
