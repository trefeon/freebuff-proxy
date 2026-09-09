package dashboard

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/store"
)

// --- logs ---

type logsData struct {
	Enabled   bool       `json:"enabled"`
	Level     string     `json:"level"`
	Msg       string     `json:"msg"`
	HasFilter bool       `json:"has_filter"`
	Entries   []logEntry `json:"entries"`
}

type logEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
	Fields  string `json:"fields"`
}

// logsData merges the live ring with the history store so the console view
// survives restarts: ring rows come first (newest-first), then DB-only rows
// fill up to the cap. The spill path round-trips ring timestamps at
// second precision, so the dedupe key is exact — a row already spilled to
// the DB never renders twice.
func (d *Dashboard) logsData(r *http.Request) logsData {
	ld := logsData{Enabled: d.logs != nil || d.hist != nil}
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
		for _, e := range d.logs.Recent(200) {
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
	if d.hist != nil && len(ld.Entries) < 200 {
		rows, err := d.hist.QueryLogs(store.LogFilter{Level: level, Contains: msg, Limit: 200})
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
			if len(ld.Entries) >= 200 {
				break
			}
		}
	}
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
