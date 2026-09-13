package pool

import (
	"context"
	"fmt"
)

// refund_refresh.go - per-account pending-refund refresh trigger for the
// dashboard (refund parity follow-up).
//
// The nightly streak touch replays a parked refund at most 3x inline and the
// CLI re-fires refreshRefund whenever its landing view shows a pending
// refund. This trigger is the proxy's on-view equivalent: a manual Refresh
// affordance on the refund line plus one automatic attempt when the pending
// line first renders. It calls the session manager's same-instance
// RefreshRefund path (re-DELETE with the parked instance id) and applies the
// CLI's guards: the parked entry must still be current and the account at
// the slot must still match, otherwise the result is dropped. A zero receipt
// is a real receipt (settled, clears pending).

// RefundRefreshResult is the outcome of one RefreshTokenRefund call.
type RefundRefreshResult struct {
	// Dropped reports the account at the slot changed mid-flight (token
	// swap, removal, or AUTH_TOKENS reload rebuild): the replay ran against
	// the retired entry, so nothing was applied to the current account.
	Dropped bool
	// Settled reports the replay returned an ended receipt: pending is
	// cleared and Amount is recorded as last_refund (zero included).
	Settled bool
	// Amount is the settled refund, valid only when Settled.
	Amount float64
	// Pending reports the refund is still awaiting final usage: either the
	// replay returned another pending receipt or nothing was parked.
	Pending bool
}

// refundFlight is one in-flight per-slot replay; concurrent RefreshTokenRefund
// calls for the same slot join it instead of re-DELETEing.
type refundFlight struct {
	done chan struct{}
	res  RefundRefreshResult
	err  error
}

// RefreshTokenRefund replays the parked pending-refund DELETE for one pool
// slot through the session manager's same-instance RefreshRefund path.
// Concurrent calls for the same slot join the in-flight replay
// (single-flight per account). A replay failure keeps the parked instance
// for a later same-instance retry and returns the error with Pending set.
func (p *Pool) RefreshTokenRefund(ctx context.Context, token int) (RefundRefreshResult, error) {
	toks := p.roster.Load()
	if token < 0 || token >= len(*toks) {
		return RefundRefreshResult{}, fmt.Errorf("pool: token %d out of range", token)
	}
	p.refundMu.Lock()
	if f, ok := p.refundInflight[token]; ok {
		p.refundMu.Unlock()
		select {
		case <-f.done:
			return f.res, f.err
		case <-ctx.Done():
			return RefundRefreshResult{Pending: true}, ctx.Err()
		}
	}
	f := &refundFlight{done: make(chan struct{})}
	if p.refundInflight == nil {
		p.refundInflight = make(map[int]*refundFlight)
	}
	p.refundInflight[token] = f
	p.refundMu.Unlock()

	f.res, f.err = p.refreshTokenRefund(ctx, token)
	close(f.done)
	p.refundMu.Lock()
	delete(p.refundInflight, token)
	p.refundMu.Unlock()
	return f.res, f.err
}

// refreshTokenRefund runs one replay outside the single-flight lock.
func (p *Pool) refreshTokenRefund(ctx context.Context, token int) (RefundRefreshResult, error) {
	toks := p.roster.Load()
	if token < 0 || token >= len(*toks) {
		return RefundRefreshResult{}, fmt.Errorf("pool: token %d out of range", token)
	}
	entry := (*toks)[token]
	entryToken := entry.token
	if entry.session.Snapshot().PendingRefund == "" {
		return RefundRefreshResult{}, nil
	}
	if err := entry.session.RefreshRefund(ctx); err != nil {
		return RefundRefreshResult{Pending: true}, err
	}
	// Current-account match (CLI refreshRefund: drop the result if the
	// account changed mid-flight). A slot rebuild swaps the entry pointer;
	// a reorder moves entries across slots — either way the slot no longer
	// resolves to the entry the replay ran against.
	cur := p.roster.Load()
	if token >= len(*cur) || (*cur)[token] != entry || (*cur)[token].token != entryToken {
		return RefundRefreshResult{Dropped: true}, nil
	}
	snap := entry.session.Snapshot()
	switch {
	case snap.PendingRefund != "":
		return RefundRefreshResult{Pending: true}, nil
	case snap.LastRefund != nil:
		return RefundRefreshResult{Settled: true, Amount: *snap.LastRefund}, nil
	default:
		return RefundRefreshResult{}, nil
	}
}
