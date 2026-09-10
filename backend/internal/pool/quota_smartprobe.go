// quota_smartprobe.go — activity-aware adaptive quota prober.
//
// Probe often when busy, once-and-sleep when idle, self-healing on boot.
// The scheduler rides the maintain tick (no new goroutine) and picks its
// cadence from pool traffic (lastActive, written by every successful
// Acquire/AcquireBridge):
//
//	ACTIVE: traffic <2m ago → probe every QUOTA_PROBE_ACTIVE_INTERVAL (60s).
//	WARM:   traffic <15m ago → probe every quotaProbeWarmInterval (5m).
//	IDLE:   no traffic for 15m+ (or never) → probe once, then sleep until
//	        traffic resumes or QUOTA_PROBE_IDLE_HEARTBEAT (30m) elapses.
//
// Event triggers bypass the timer: the first tick after Start runs one
// round, token add/remove kicks the next tick, a manual Probe-all counts as
// a round (the scheduler timer restarts from it), and the first request
// after an idle sleep wakes the next tick.
//
// Per-round guards: locked, quarantined, banned, cooling and
// country-blocked tokens are skipped (same health gates as the maturity
// tick); token probes stagger 1–3s apart with jitter; the first upstream
// 429 aborts the round and doubles the effective interval (cap 30m, reset
// to the base cadence on the next 429-free round).
//
// Probe traffic is internal: the session-less ProbeToken path never touches
// the usage ledgers, lastActive, or requestsServed, so probes neither feed
// the tier classifier nor count as client usage. All scheduler state is
// in-memory only (a restart re-probes on the next due tick); the
// quota_snapshots wallet persistence path is untouched.
package pool

import (
	"context"
	"errors"
	"sync"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/upstream"
)

// Activity windows: how recent pool traffic must be for each tier.
const (
	quotaProbeActiveWindow = 2 * time.Minute
	quotaProbeWarmWindow   = 15 * time.Minute
)

// quotaProbeWarmInterval is the derived WARM cadence (not knobbed): five
// times the default active interval, one twelfth of the idle heartbeat.
const quotaProbeWarmInterval = 5 * time.Minute

// quotaProbeMaxInterval caps the effective interval: 429 backoff doubling
// never sleeps longer than the idle heartbeat itself.
const quotaProbeMaxInterval = 30 * time.Minute

// Scheduler knob defaults (mirrored in config defaults + catalog).
const (
	defaultQuotaProbeActiveInterval = time.Minute
	defaultQuotaProbeIdleHeartbeat  = 30 * time.Minute
)

// quotaProbeFireTimeout bounds one token's session-less probe.
const quotaProbeFireTimeout = 10 * time.Second

// maxSmartProbeBackoff bounds the doubling multiplier (the effective
// interval is capped independently, so this only guards the shift).
const maxSmartProbeBackoff = 32

// quotaProbeTier is one activity cadence of the scheduler.
type quotaProbeTier int

const (
	quotaTierActive quotaProbeTier = iota
	quotaTierWarm
	quotaTierIdle
)

// classifyQuotaProbeTier maps pool traffic recency to a tier. A pool that
// never served (zero lastActive) is IDLE — the boot round still fires via
// the Start-anchored force, not via the tier. Future timestamps (clock
// skew) clamp to ACTIVE.
func classifyQuotaProbeTier(lastActive, now time.Time) quotaProbeTier {
	if lastActive.IsZero() {
		return quotaTierIdle
	}
	idle := now.Sub(lastActive)
	if idle < 0 {
		idle = 0
	}
	switch {
	case idle < quotaProbeActiveWindow:
		return quotaTierActive
	case idle < quotaProbeWarmWindow:
		return quotaTierWarm
	default:
		return quotaTierIdle
	}
}

// baseInterval returns the tier's configured cadence (before backoff).
// Non-positive configured values fall back to the documented defaults, so
// a zero-value Config (unit tests) behaves like production.
func (t quotaProbeTier) baseInterval(cfg *config.Config) time.Duration {
	switch t {
	case quotaTierActive:
		if cfg != nil && cfg.QuotaProbeActiveInterval > 0 {
			return cfg.QuotaProbeActiveInterval
		}
		return defaultQuotaProbeActiveInterval
	case quotaTierWarm:
		return quotaProbeWarmInterval
	default:
		if cfg != nil && cfg.QuotaProbeIdleHeartbeat > 0 {
			return cfg.QuotaProbeIdleHeartbeat
		}
		return defaultQuotaProbeIdleHeartbeat
	}
}

// effectiveQuotaProbeInterval doubles the base cadence per backoff step
// (1 = no backoff), saturating at quotaProbeMaxInterval.
func effectiveQuotaProbeInterval(base time.Duration, backoff int) time.Duration {
	if base <= 0 {
		base = defaultQuotaProbeActiveInterval
	}
	if backoff < 1 {
		backoff = 1
	}
	eff := base
	for i := 1; i < backoff; i++ {
		if eff >= quotaProbeMaxInterval || eff <= 0 || eff > (1<<62)/2 {
			return quotaProbeMaxInterval
		}
		eff *= 2
		if eff >= quotaProbeMaxInterval || eff <= 0 {
			return quotaProbeMaxInterval
		}
	}
	return eff
}

// smartProbeState is the scheduler's in-memory-only state. Guarded by mu;
// AddToken/Remove/ProbeAll (request goroutines) mutate kick/lastProbe while
// the maintain goroutine owns the tick, so every access takes mu and the
// probes themselves always run outside it.
type smartProbeState struct {
	mu sync.Mutex
	// lastProbe is when the last round ran (forced or due, any outcome).
	lastProbe time.Time
	// backoff is the 429 doubling multiplier (0/1 = base cadence).
	backoff int
	// bootDone marks the Start-anchored first round as run.
	bootDone bool
	// kick forces the next tick to run (token add/remove).
	kick bool
	// idleSlept marks the IDLE single-probe as run for the current quiet
	// stretch: further ticks sleep until traffic resumes (wake) or the
	// heartbeat elapses.
	idleSlept bool
}

func (s *smartProbeState) mult() int {
	if s.backoff < 1 {
		return 1
	}
	return s.backoff
}

// quotaProbeStagger sleeps between token probes inside one round (1–3s with
// jitter from the pool's crypto-rand source), so a fleet restart never
// fires as a burst. A var so tests stub it to a no-op.
var quotaProbeStagger = func(ctx context.Context) {
	d := time.Second + time.Duration(sessionRand()%uint64(2*time.Second))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// smartProbeKick forces the next maintain tick to run a probe round
// (token add/remove event trigger).
func (p *Pool) smartProbeKick() {
	p.smartProbe.mu.Lock()
	p.smartProbe.kick = true
	p.smartProbe.mu.Unlock()
}

// smartProbeNoteManual stamps a manual/visit bulk probe as a scheduler
// round: the pass just refreshed every token, so the timer restarts from it
// instead of re-probing on the next tick.
func (p *Pool) smartProbeNoteManual(now time.Time) {
	p.smartProbe.mu.Lock()
	p.smartProbe.lastProbe = now
	p.smartProbe.kick = false
	p.smartProbe.idleSlept = true
	p.smartProbe.mu.Unlock()
}

// smartProbeTick runs one scheduler pass on the maintain clock.
func (p *Pool) smartProbeTick(ctx context.Context) {
	p.smartProbeTickAt(ctx, time.Now())
}

// smartProbeTickAt is smartProbeTick with the clock injected (tests).
func (p *Pool) smartProbeTickAt(ctx context.Context, now time.Time) {
	cfg := p.cfg.Load()
	if cfg == nil || !cfg.QuotaAutoProbe {
		return
	}
	toks := p.roster.Load()
	if toks == nil || len(*toks) == 0 {
		return
	}
	lastActive := p.lastActiveAt()
	tier := classifyQuotaProbeTier(lastActive, now)

	p.smartProbe.mu.Lock()
	forced := p.smartProbe.kick || (!p.smartProbe.bootDone && !p.quotaBootAt.IsZero())
	wake := !lastActive.IsZero() && !p.smartProbe.lastProbe.IsZero() &&
		lastActive.After(p.smartProbe.lastProbe) && p.smartProbe.idleSlept
	due := forced || wake
	interval := effectiveQuotaProbeInterval(tier.baseInterval(cfg), p.smartProbe.mult())
	if !due {
		switch {
		case p.smartProbe.lastProbe.IsZero():
			due = true
		case tier == quotaTierIdle:
			due = !p.smartProbe.idleSlept || !now.Before(p.smartProbe.lastProbe.Add(interval))
		default:
			due = !now.Before(p.smartProbe.lastProbe.Add(interval))
		}
	}
	if !due {
		p.smartProbe.mu.Unlock()
		return
	}
	p.smartProbe.kick = false
	p.smartProbe.bootDone = true
	p.smartProbe.mu.Unlock()

	fired, limited, canceled := p.smartProbeRound(ctx, *toks, now)

	p.smartProbe.mu.Lock()
	defer p.smartProbe.mu.Unlock()
	p.smartProbe.lastProbe = now
	if canceled {
		return
	}
	if limited {
		if p.smartProbe.backoff < 1 {
			p.smartProbe.backoff = 2
		} else if p.smartProbe.backoff < maxSmartProbeBackoff {
			p.smartProbe.backoff *= 2
		}
	} else if fired > 0 {
		p.smartProbe.backoff = 1
	}
	if tier == quotaTierIdle {
		p.smartProbe.idleSlept = true
	} else {
		p.smartProbe.idleSlept = false
	}
}

// smartProbeRound probes every eligible token once, staggering between
// probes. The first upstream 429 aborts the round (the remaining tokens
// wait for the doubled interval). A 429 arrives two ways: an unparseable
// body classifies to ErrRateLimited, while a structured quota body parses
// into a SessionState with Status "rate_limited" (the quota it carries is
// still cached by ProbeToken — the abort only spares the other tokens).
// Returns the success count plus whether the round hit a 429 or the
// context died mid-round.
func (p *Pool) smartProbeRound(ctx context.Context, toks []*tokenEntry, now time.Time) (fired int, limited, canceled bool) {
	staggered := false
	for i, tok := range toks {
		if ctx.Err() != nil {
			return fired, false, true
		}
		if smartProbeSkipToken(tok, now) {
			continue
		}
		if staggered {
			quotaProbeStagger(ctx)
			if ctx.Err() != nil {
				return fired, false, true
			}
		}
		staggered = true
		st, err := p.smartProbeOne(ctx, i, tok)
		if err != nil {
			if errors.Is(err, upstream.ErrRateLimited) {
				return fired, true, false
			}
			continue
		}
		if st != nil && st.Status == "rate_limited" {
			return fired, true, false
		}
		fired++
	}
	return fired, false, false
}

// smartProbeOne issues one session-less ProbeToken with a bounded context,
// warn-only on failure. It never records usage: probes stay out of the
// ledgers, lastActive, and requestsServed by construction. The live state
// rides along so the round can spot a 429 that parsed as quota data.
func (p *Pool) smartProbeOne(ctx context.Context, i int, tok *tokenEntry) (*upstream.SessionState, error) {
	label := tokenEntryLabel(tok)
	fire, cancel := context.WithTimeout(ctx, quotaProbeFireTimeout)
	st, err := p.ProbeToken(fire, i)
	cancel()
	if err != nil {
		if errors.Is(err, upstream.ErrRateLimited) {
			p.logger.Warn("pool: smart probe rate-limited, aborting round", "token", i+1, "token_label", label, "err", err)
		} else {
			p.logger.Warn("pool: smart probe failed", "token", i+1, "token_label", label, "err", err)
		}
		return nil, err
	}
	if st != nil && st.Status == "rate_limited" {
		p.logger.Warn("pool: smart probe rate-limited, aborting round", "token", i+1, "token_label", label)
	} else {
		p.logger.Debug("pool: smart probe refreshed", "token", i+1, "token_label", label)
	}
	return st, nil
}

// smartProbeSkipToken reports whether the token sits out this round: the
// administrative lock plus the same health gates as the maturity tick
// (quarantined, banned, cooling, country-blocked accounts are never poked).
// Health state is set by the request path; a probe never sets it (probe
// failures stay warn-only so scheduler work cannot change serving).
func smartProbeSkipToken(tok *tokenEntry, now time.Time) bool {
	if tok == nil {
		return true
	}
	if tok.locked.Load() {
		return true
	}
	if tok.quarantine.Load() != nil {
		return true
	}
	rs := tok.runs.Snapshot()
	if rs.BanError != nil && (rs.BannedUntil.IsZero() || now.Before(rs.BannedUntil)) {
		return true
	}
	if !rs.CooldownUntil.IsZero() && now.Before(rs.CooldownUntil) {
		return true
	}
	if tok.runs.CountryBlockedError() != nil {
		return true
	}
	return false
}
