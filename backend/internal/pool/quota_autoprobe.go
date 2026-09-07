// quota_autoprobe.go — quota auto-probe scheduler (ADR-0022).
//
// The Tokens header Probe-all button was removed: quota data must stay fresh
// without a manual bulk probe. This scheduler rides the maintain tick (no new
// goroutine) and probes each pooled token once per Pacific day at a
// deterministic jittered slot inside the 2h window before its known reset:
//
//	Slot = hash(token-index + Pacific date) spread over [reset-2h, reset).
//
// The day map is in-memory (tokenEntry.quotaProbeDay): a restart may
// double-probe once, and the jitter spreads restarts so the herd never moves
// together. The probe path is the session-less ProbeToken (warn-only
// failures, results flow through the existing UpdateQuotaFromProbe
// snapshots). Tokens with unknown reset that the ADR-0024 boot seed never
// filled (truly never probed) get one staggered boot probe instead of
// waiting for a manual one (see quota_bootseed.go); seeded tokens follow
// the slots below once their reset is known. QUOTA_AUTO_PROBE=false
// restores exact pre-scheduler behavior (no auto-probe, no boot probe).
package pool

import (
	"context"
	"hash/fnv"
	"strconv"
	"time"

	"freebuff-proxy/backend/internal/session"
)

// quotaAutoProbeWindow is the pre-reset window the daily probe slot is drawn
// from (ADR-0022): the last 2h before the token's known quota reset.
const quotaAutoProbeWindow = 2 * time.Hour

// quotaAutoProbeSlot returns the deterministic probe slot for token idx on
// pacificDay (YYYY-MM-DD): reset-2h plus an FNV-1a hash spread over the 2h
// window. Pure (no clock, no pool state) so tests pin determinism and spread
// directly. FNV-1a is stdlib (no new deps); cryptographic strength is not
// needed — the hash only staggers probes.
func quotaAutoProbeSlot(idx int, pacificDay string, reset time.Time) time.Time {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strconv.Itoa(idx) + "|" + pacificDay))
	offset := time.Duration(h.Sum64() % uint64(quotaAutoProbeWindow))
	return reset.Add(-quotaAutoProbeWindow).Add(offset)
}

// quotaAutoProbeReset returns the earliest future quota reset across the
// token's cached quota map. ok=false means unknown reset (never probed):
// the scheduler skips the token until a manual probe seeds its quota.
func quotaAutoProbeReset(snap session.SessionSnapshot, now time.Time) (time.Time, bool) {
	var best time.Time
	for _, q := range snap.QuotaByModel {
		if q.ResetAt.IsZero() || !q.ResetAt.After(now) {
			continue
		}
		if best.IsZero() || q.ResetAt.Before(best) {
			best = q.ResetAt
		}
	}
	return best, !best.IsZero()
}

// quotaAutoProbeDue is the pure firing predicate: enabled kill-switch, known
// (non-zero) reset, not already probed this Pacific day, and now at/past the
// deterministic slot. A stale reset (already past) yields a past slot, so a
// token whose window rolled unprobed fires once as catch-up.
func quotaAutoProbeDue(enabled bool, idx int, lastProbeDay string, now time.Time, reset time.Time) bool {
	if !enabled || reset.IsZero() {
		return false
	}
	day := now.In(maturityLocation("")).Format("2006-01-02")
	if lastProbeDay == day {
		return false
	}
	return !now.Before(quotaAutoProbeSlot(idx, day, reset))
}

// quotaAutoProbeTick runs one auto-probe pass over the fixed tokens. Called
// from maintainTick on every pass (active and idle-stretch, like
// maturityTick): idle accounts are exactly the ones whose quota must be
// fresh when traffic resumes.
func (p *Pool) quotaAutoProbeTick(ctx context.Context) {
	p.quotaAutoProbeTickAt(ctx, time.Now())
}

// quotaAutoProbeTickAt is quotaAutoProbeTick with the clock injected
// (fake-clock tests).
func (p *Pool) quotaAutoProbeTickAt(ctx context.Context, now time.Time) {
	cfg := p.cfg.Load()
	if cfg == nil || !cfg.QuotaAutoProbe {
		return
	}
	toks := p.roster.Load()
	if toks == nil {
		return
	}
	today := now.In(maturityLocation("")).Format("2006-01-02")
	for i, tok := range *toks {
		reset, ok := quotaAutoProbeReset(tok.session.Snapshot(), now)
		if !ok {
			// ADR-0024 first-probe recovery: unknown reset AND never
			// seeded means truly never probed — one staggered boot probe
			// (session-less, warn-only, once per process). Seeded tokens
			// follow the normal slots below once their reset is known.
			if tok.quotaSeeded || !quotaBootProbeDue(i, tok.quotaBootProbed, p.quotaBootAt, now) {
				continue
			}
			// Mark both the boot flag and the day on attempt, so the
			// recovery fires once per process and never doubles with a
			// same-day scheduler slot when the probe teaches a reset.
			tok.quotaBootProbed = true
			tok.quotaProbeDay = today
			p.quotaFireProbe(ctx, i, tok, "boot probe")
			continue
		}
		if !quotaAutoProbeDue(true, i, tok.quotaProbeDay, now, reset) {
			continue
		}
		// Mark the day on attempt — success or warn-only failure alike:
		// exactly one probe per token per Pacific day bounds the ban
		// surface to one session-less GET (the bulk button this replaces
		// fired them all at once).
		tok.quotaProbeDay = today
		p.quotaFireProbe(ctx, i, tok, "auto-probe")
	}
}
