package pool

import (
	"time"
)

// MaturityHistoryEvent is one pool lifecycle fact (ADR-0016) for the
// history store. The sink runs on pool goroutines (maintain tick, admin
// handlers), so it must never block, fail open, or call back into the
// pool: the CLI adapter does a single synchronous SQLite insert and drops
// nothing (callers are rare: config changes, daily touches).
//
// Kinds: "config" (enable/disable/target/mode change), "touch" (one fire
// outcome, skips included), "advance" (streak moved), "release" (target
// reached, automation disabled).
type MaturityHistoryEvent struct {
	TS       int64
	TokenIdx int
	Kind     string
	Detail   string
}

// HistorySink persists pool lifecycle facts. Implementations must be
// goroutine-safe and must not call back into the pool.
type HistorySink interface {
	RecordMaturity(MaturityHistoryEvent)
}

// SetHistorySink installs the maturity history consumer (nil clears it).
func (p *Pool) SetHistorySink(s HistorySink) {
	if s == nil {
		p.histSink.Store(nil)
		return
	}
	p.histSink.Store(&s)
}

func (p *Pool) emitMaturity(idx int, kind, detail string) {
	if v := p.histSink.Load(); v != nil && *v != nil {
		(*v).RecordMaturity(MaturityHistoryEvent{
			TS:       time.Now().UnixMilli(),
			TokenIdx: idx,
			Kind:     kind,
			Detail:   detail,
		})
	}
}
