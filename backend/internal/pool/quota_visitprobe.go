// quota_visitprobe.go — quota bulk probe with a pool-scoped throttle
// (ADR-0025).
//
// The Quota Tracker page fires POST /admin/tokens/test-all?auto=1 on mount
// so a cold page shows numbers within seconds of opening, no button press.
// Probes are session-less upstream GETs (the same read the CLI performs on
// its own): no session admission, no chat relay, warn-tolerant like the
// ADR-0022 scheduler. One in-memory timestamp covers all clients/tabs —
// ten open tabs still cause one upstream pass per hour, not ten.
//
// The manual Probe-all button (no auto param) always forces through the
// same pass and refreshes the timestamp. The visit probe ignores the
// QUOTA_AUTO_PROBE kill-switch: like the manual button it is an explicit
// user-visit action, not scheduler work. ADR-0022 daily slots and the
// ADR-0024 boot seed stay as backstops.
package pool

import (
	"context"
	"errors"
	"time"

	"freebuff-proxy/backend/internal/upstream"
)

// QuotaVisitProbeMaxAge is the staleness bound for the visit auto-probe
// (ADR-0025): auto=1 probes only when the last bulk probe is older than
// this (or never ran). A plain const, not an env knob — the manual button
// never had a throttle, so a bounded automatic probe is strictly gentler
// than what the UI already allows.
const QuotaVisitProbeMaxAge = time.Hour

// quotaVisitProbeDue is the pure firing predicate: never probed, or the
// last bulk probe is at least maxAge old. A future timestamp (clock skew)
// reads fresh. Pure (no clock, no pool state) so tests pin the boundary
// directly.
func quotaVisitProbeDue(last, now time.Time, maxAge time.Duration) bool {
	if last.IsZero() {
		return true
	}
	return !now.Before(last.Add(maxAge))
}

// BulkProbeResult is one token's outcome from a bulk probe pass. Err is
// nil on success or carries upstream.ErrNoActiveSession (still a valid
// token — the warn-tolerant ok shape the dashboard renders); any other
// non-nil Err is a real probe failure.
type BulkProbeResult struct {
	Index int
	State *upstream.SessionState
	Err   error
}

// probeAllTokens probes every pooled token via the session-less ProbeToken
// path (8s bound each, warn-tolerant). Failures warn-log (the silent visit
// path reports nothing else) and ride along in the results for the manual
// renderer. No timestamp accounting — callers stamp via markBulkProbed.
func (p *Pool) probeAllTokens(ctx context.Context) []BulkProbeResult {
	var out []BulkProbeResult
	for _, snap := range p.PoolSnapshot().Tokens {
		fire, cancel := context.WithTimeout(ctx, 8*time.Second)
		st, err := p.ProbeToken(fire, snap.Token)
		cancel()
		if err != nil && !errors.Is(err, upstream.ErrNoActiveSession) {
			p.logger.Warn("pool: bulk probe failed", "token", snap.Token+1, "err", err)
		}
		out = append(out, BulkProbeResult{Index: snap.Token, State: st, Err: err})
	}
	return out
}

// markBulkProbed stamps the pool-scoped last-bulk-probe timestamp.
func (p *Pool) markBulkProbed(now time.Time) {
	p.bulkProbeMu.Lock()
	defer p.bulkProbeMu.Unlock()
	p.lastBulkProbe = now
}

// ProbeAll force-probes every pooled token (the manual Probe-all button):
// always probes, then refreshes the pool-scoped timestamp.
func (p *Pool) ProbeAll(ctx context.Context) []BulkProbeResult {
	out := p.probeAllTokens(ctx)
	p.markBulkProbed(time.Now())
	return out
}

// ProbeAllIfStale probes every pooled token only when the pool-scoped
// last-bulk-probe timestamp is older than maxAge (or never): the visit
// auto-probe path. Returns false without touching upstream when fresh.
// The slot is claimed before probing so concurrent visitors share one
// pass instead of each firing their own.
func (p *Pool) ProbeAllIfStale(ctx context.Context, maxAge time.Duration) bool {
	now := time.Now()
	p.bulkProbeMu.Lock()
	if !quotaVisitProbeDue(p.lastBulkProbe, now, maxAge) {
		p.bulkProbeMu.Unlock()
		return false
	}
	p.lastBulkProbe = now
	p.bulkProbeMu.Unlock()
	p.probeAllTokens(ctx)
	return true
}
