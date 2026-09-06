package dashboard

import (
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
