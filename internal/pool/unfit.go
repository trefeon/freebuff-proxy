package pool

import (
	"time"

	"freebuff-proxy/internal/upstream"
)

// modelUnfitTTL is how long a (egress, model) pair stays marked unfit after
// an upstream limited_ip refusal. A var (not const) so tests can shrink it.
var modelUnfitTTL = 5 * time.Minute

// unfitEgress is the pool's only egress: the direct connection (SOCKS5/HTTP
// proxy support was removed — the pool has exactly one egress). The registry
// still keys (egress, model) so a proxy re-introduction can key per egress
// without changing the registry shape.
const unfitEgress = "direct"

// unfitKey is one (egress, model) pair in the model-unfit registry.
type unfitKey struct {
	egress string
	model  string
}

// unfitEntry is one registry entry: when the (egress, model) pair is unfit
// until, and the refusal that marked it (for surfacing to the client).
type unfitEntry struct {
	until time.Time
	err   *upstream.LimitedIpError
}

// MarkModelUnfit records that the pool's egress cannot serve model for the
// next modelUnfitTTL (upstream limited_ip refusal: the session row is fine,
// but this egress IP cannot serve this model). nil lie is tolerated; when
// non-nil its Model is set to the marked model so the surfaced error is
// self-describing.
func (p *Pool) MarkModelUnfit(model string, lie *upstream.LimitedIpError) {
	if lie != nil {
		lie.Model = model
	}
	until := time.Now().Add(modelUnfitTTL)
	p.unfitMu.Lock()
	p.unfit[unfitKey{egress: unfitEgress, model: model}] = unfitEntry{until: until, err: lie}
	p.unfitMu.Unlock()
	p.logger.Debug("pool: model marked unfit on egress", "egress", unfitEgress, "model", model, "until", until.Format(time.RFC3339))
}

// ClearModelUnfit removes the unfit mark for model (called after a
// successful chat on the model: the refusal window is over).
func (p *Pool) ClearModelUnfit(model string) {
	p.unfitMu.Lock()
	delete(p.unfit, unfitKey{egress: unfitEgress, model: model})
	p.unfitMu.Unlock()
	p.logger.Debug("pool: model unfit mark cleared", "egress", unfitEgress, "model", model)
}

// ModelUnfit reports whether model is currently marked unfit on the pool's
// egress. The zero time.Time means "not unfit"; a nil error means the entry
// was marked with no refusal detail. Expired entries are purged lazily.
func (p *Pool) ModelUnfit(model string) (time.Time, *upstream.LimitedIpError) {
	key := unfitKey{egress: unfitEgress, model: model}
	now := time.Now()
	p.unfitMu.Lock()
	defer p.unfitMu.Unlock()
	e, ok := p.unfit[key]
	if !ok {
		return time.Time{}, nil
	}
	if !now.Before(e.until) {
		delete(p.unfit, key)
		return time.Time{}, nil
	}
	return e.until, e.err
}
