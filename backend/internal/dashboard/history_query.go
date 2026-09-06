package dashboard

import (
	"net/http"
	"strconv"

	"freebuff-proxy/backend/internal/store"
)

// --- history queries (ADR-0016) ---
//
// Read-only views over the history store. Every shape carries Enabled: false
// with empty rows when the dashboard runs live-only (nil store), so the SPA
// renders its live views without branching on endpoint availability.
// Missing/invalid filter params yield empty rows, never an error: the pages
// always pass them, and a 200 with no rows is the honest answer for "no
// samples yet".

type quotaSample struct {
	TS           int64   `json:"ts"`
	Limit        float64 `json:"limit"`
	Recent       float64 `json:"recent"`
	ResetAt      int64   `json:"reset_at"`
	Entitlements string  `json:"entitlements,omitempty"`
}

type quotaHistoryData struct {
	Enabled   bool          `json:"enabled"`
	Token     int           `json:"token"`
	Model     string        `json:"model"`
	Snapshots []quotaSample `json:"snapshots"`
}

type maturityHistoryItem struct {
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

type maturityHistoryData struct {
	Enabled bool                  `json:"enabled"`
	Token   int                   `json:"token"`
	Events  []maturityHistoryItem `json:"events"`
}

type logHistoryItem struct {
	TS     int64  `json:"ts"`
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	Fields string `json:"fields"`
	ReqID  string `json:"req_id,omitempty"`
}

type logsHistoryData struct {
	Enabled bool             `json:"enabled"`
	Entries []logHistoryItem `json:"entries"`
}

func queryInt(r *http.Request, key string, def int) int {
	if r == nil || r.URL == nil {
		return def
	}
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	return v
}

func queryInt64(r *http.Request, key string, def int64) int64 {
	if r == nil || r.URL == nil {
		return def
	}
	v, err := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	if err != nil {
		return def
	}
	return v
}

func (d *Dashboard) quotaHistoryData(r *http.Request) quotaHistoryData {
	qd := quotaHistoryData{Token: -1}
	if r != nil && r.URL != nil {
		qd.Token = queryInt(r, "token", -1)
		qd.Model = r.URL.Query().Get("model")
	}
	if d.hist == nil {
		return qd
	}
	qd.Enabled = true
	if qd.Token < 0 || qd.Model == "" {
		return qd
	}
	rows, err := d.hist.QuotaHistory(qd.Token, qd.Model, queryInt64(r, "since", 0), queryInt(r, "limit", 500))
	if err != nil {
		d.logger.Warn("quota history query failed", "err", err)
		return qd
	}
	for _, s := range rows {
		qd.Snapshots = append(qd.Snapshots, quotaSample{
			TS:           s.TS,
			Limit:        s.Limit,
			Recent:       s.Recent,
			ResetAt:      s.ResetAt,
			Entitlements: s.Entitlements,
		})
	}
	return qd
}

func (d *Dashboard) maturityHistoryData(r *http.Request) maturityHistoryData {
	md := maturityHistoryData{Token: -1}
	if r != nil && r.URL != nil {
		md.Token = queryInt(r, "token", -1)
	}
	if d.hist == nil {
		return md
	}
	md.Enabled = true
	if md.Token < 0 {
		return md
	}
	rows, err := d.hist.MaturityHistory(md.Token, queryInt64(r, "since", 0), queryInt(r, "limit", 200))
	if err != nil {
		d.logger.Warn("maturity history query failed", "err", err)
		return md
	}
	for _, e := range rows {
		md.Events = append(md.Events, maturityHistoryItem{TS: e.TS, Kind: e.Kind, Detail: e.Detail})
	}
	return md
}

func (d *Dashboard) logsHistoryData(r *http.Request) logsHistoryData {
	ld := logsHistoryData{}
	if d.hist == nil {
		return ld
	}
	ld.Enabled = true
	f := store.LogFilter{Limit: queryInt(r, "limit", 500)}
	if r != nil && r.URL != nil {
		q := r.URL.Query()
		f.Level = q.Get("level")
		f.Contains = q.Get("msg")
		f.ReqID = q.Get("req_id")
		f.Since = queryInt64(r, "since", 0)
		f.Until = queryInt64(r, "until", 0)
	}
	rows, err := d.hist.QueryLogs(f)
	if err != nil {
		d.logger.Warn("logs history query failed", "err", err)
		return ld
	}
	for _, e := range rows {
		ld.Entries = append(ld.Entries, logHistoryItem{
			TS: e.TS, Level: e.Level, Msg: e.Msg, Fields: e.Fields, ReqID: e.ReqID,
		})
	}
	return ld
}
