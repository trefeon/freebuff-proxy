package dashboard

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/store"
)

// History spill path (ADR-0016): the logring fan-out tap feeds a buffered
// channel, and one background goroutine batch-inserts into the history store.
// The tap never blocks the log path (a full buffer drops with a counter);
// retention and lifecycle stay out of request handlers.

const (
	spillBufSize      = 1024
	spillFlushSize    = 100
	spillFlushEvery   = time.Second
	spillShutdownWait = 5 * time.Second
)

// WithHistory attaches the history store and starts the log spill consumer.
// A nil store is a no-op: the dashboard runs live-only. The caller owns the
// store handle; Close stops the consumer (flushing first) and closes it.
func WithHistory(st *store.Store) Option {
	return func(d *Dashboard) {
		if st == nil {
			return
		}
		d.hist = st
		d.spillCh = make(chan logring.Entry, spillBufSize)
		d.spillDone = make(chan struct{})
		d.spillWg.Add(1)
		go d.spillLoop()
		if d.logs != nil {
			d.logs.SetSpill(d.enqueueSpill)
		}
	}
}

// enqueueSpill is the logring tap: non-blocking, drops on a full buffer.
func (d *Dashboard) enqueueSpill(e logring.Entry) {
	if d.spillCh == nil {
		return
	}
	select {
	case d.spillCh <- e:
	default:
		d.spillDropped.Add(1)
	}
}

func (d *Dashboard) spillLoop() {
	defer d.spillWg.Done()
	batch := make([]store.LogEntry, 0, spillFlushSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := d.hist.AppendLogs(batch); err != nil {
			d.logger.Warn("history spill failed", "err", err, "rows", len(batch))
		}
		batch = batch[:0]
	}
	tick := time.NewTicker(spillFlushEvery)
	defer tick.Stop()
	for {
		select {
		case e, ok := <-d.spillCh:
			if !ok {
				flush()
				return
			}
			batch = append(batch, spillEntry(e))
			if len(batch) >= spillFlushSize {
				flush()
			}
		case <-tick.C:
			flush()
		case <-d.spillDone:
			for {
				select {
				case e := <-d.spillCh:
					batch = append(batch, spillEntry(e))
					if len(batch) >= spillFlushSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// spillEntry converts one ring record to the store domain. TS falls back to
// now when the ring timestamp is unparsable; level is lowercased to the
// store's filter domain; req_id is recovered from the flattened fields when
// the handler logged one.
func spillEntry(e logring.Entry) store.LogEntry {
	ts := store.Millis(time.Now())
	if t, err := time.Parse(time.RFC3339, e.Time); err == nil {
		ts = store.Millis(t)
	}
	return store.LogEntry{
		TS:     ts,
		Level:  strings.ToLower(e.Level),
		Msg:    e.Message,
		Fields: strings.Join(e.Fields, "\n"),
		ReqID:  spillReqID(e.Fields),
	}
}

func spillReqID(fields []string) string {
	for _, f := range fields {
		if v, ok := strings.CutPrefix(f, "req_id="); ok {
			return v
		}
	}
	return ""
}

// spillStats reports consumer health for tests and the dashboard itself.
func (d *Dashboard) spillStats() (dropped int64) {
	return d.spillDropped.Load()
}

// Close stops the history spill consumer (flushing what is buffered) and
// closes the history store. Safe to call without WithHistory.
func (d *Dashboard) Close() error {
	if d.spillCh != nil {
		close(d.spillDone)
		done := make(chan struct{})
		go func() { d.spillWg.Wait(); done <- struct{}{} }()
		select {
		case <-done:
		case <-time.After(spillShutdownWait):
		}
		if d.logs != nil {
			d.logs.SetSpill(nil)
		}
	}
	if d.hist != nil {
		return d.hist.Close()
	}
	return nil
}

// quotaKey identifies one sampled quota row; quotaPoint is its persisted
// state. The full-view sampler (tokensData) records a row only when the
// point differs from the last persisted one, so history holds change points
// rather than per-poll duplicates. The hot poll never samples.
type quotaKey struct {
	idx   int
	model string
}

type quotaPoint struct {
	recent  float64
	limit   float64
	resetAt int64
}

// sampleQuota persists one quota row when its state changed since the last
// sample. No-op without a history store. Synchronous single-row insert on
// the full-view path (once per mount + 5min cadence + mutations), never on
// the 10s hot poll.
func (d *Dashboard) sampleQuota(idx int, model string, limit, recent float64, resetAt time.Time, entitlements string) {
	if d.hist == nil {
		return
	}
	pt := quotaPoint{recent: recent, limit: limit, resetAt: store.Millis(resetAt)}
	key := quotaKey{idx: idx, model: model}
	d.quotaSeenMu.Lock()
	if old, ok := d.quotaSeen[key]; ok && old == pt {
		d.quotaSeenMu.Unlock()
		return
	}
	if d.quotaSeen == nil {
		d.quotaSeen = make(map[quotaKey]quotaPoint)
	}
	d.quotaSeen[key] = pt
	d.quotaSeenMu.Unlock()
	if err := d.hist.RecordQuota(store.QuotaSnapshot{
		TS: store.Millis(time.Now()), TokenIdx: idx, Model: model,
		Limit: limit, Recent: recent, ResetAt: pt.resetAt, Entitlements: entitlements,
	}); err != nil {
		d.logger.Warn("quota sample failed", "err", err)
	}
}

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
