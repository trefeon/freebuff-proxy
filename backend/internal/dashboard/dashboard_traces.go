package dashboard

import (
	"strconv"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/phasetiming"
	"freebuff-proxy/backend/internal/store"
)

// --- traces ---

type tracesData struct {
	Enabled bool         `json:"enabled"`
	Traces  []traceEntry `json:"traces"`
}

type traceEntry struct {
	Time   string    `json:"time"`
	Token  string    `json:"token"`
	Model  string    `json:"model"`
	Status string    `json:"status"`
	Ms     string    `json:"ms"`
	Error  string    `json:"error"`
	Phases []PhaseKV `json:"phases,omitempty"`
}

// tracesData merges the live ring with the history store so the Traces
// console view survives restarts: ring traces first, then "chat trace" rows
// spilled to the DB fill up to the cap. Dedupe reuses the log key — a
// trace already spilled never renders twice.
func (d *Dashboard) tracesData() tracesData {
	td := tracesData{Enabled: d.logs != nil || d.hist != nil}
	seen := map[string]bool{}
	if d.logs != nil {
		for _, e := range d.logs.Recent(200) {
			if e.Message != "chat trace" {
				continue
			}
			seen[logDedupeKey(e.Time, e.Level, e.Message, e.Fields)] = true
			td.Traces = append(td.Traces, traceFromFields(e.Time, e.Fields))
		}
	}
	if d.hist != nil && len(td.Traces) < 200 {
		rows, err := d.hist.QueryLogs(store.LogFilter{Contains: "chat trace", Limit: 200})
		if err != nil {
			d.logger.Warn("traces query failed", "err", err)
			return td
		}
		for _, e := range rows {
			if e.Msg != "chat trace" {
				continue
			}
			fields := strings.Split(e.Fields, "\n")
			ts := time.UnixMilli(e.TS).UTC().Format(time.RFC3339)
			if seen[logDedupeKey(ts, e.Level, e.Msg, fields)] {
				continue
			}
			td.Traces = append(td.Traces, traceFromFields(ts, fields))
			if len(td.Traces) >= 200 {
				break
			}
		}
	}
	return td
}

// traceFromFields parses one retained "chat trace" record into its display
// row. Shared by the ring and DB paths so both render identically.
func traceFromFields(timeStr string, fields []string) traceEntry {
	entry := traceEntry{Time: timeStr, Status: "ok"}
	var phaseMap map[string]int64
	for _, f := range fields {
		key, value, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch key {
		case "token":
			entry.Token = value
		case "model":
			entry.Model = value
		case "status":
			entry.Status = value
		case "ms":
			entry.Ms = value + "ms"
		case "error":
			entry.Error = value
		default:
			if phaseNames[key] {
				if phaseMap == nil {
					phaseMap = make(map[string]int64, 5)
				}
				if v, err := strconv.ParseInt(value, 10, 64); err == nil {
					phaseMap[key] = v
				}
			}
		}
	}
	if phaseMap != nil {
		entry.Phases = PhaseList(phaseMap)
	}
	if entry.Token == "" {
		entry.Token = "—"
	}
	return entry
}

// phaseNames is the phase-timing field set a trace row renders.
var phaseNames = map[string]bool{
	phasetiming.AcquireMS:        true,
	phasetiming.SessionRefreshMS: true,
	phasetiming.RunAcquireMS:     true,
	phasetiming.UpstreamTTFBMS:   true,
	phasetiming.TotalMS:          true,
}
