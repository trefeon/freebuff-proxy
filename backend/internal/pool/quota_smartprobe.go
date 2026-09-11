// quota_smartprobe.go — activity-aware adaptive quota prober.
//
// Probe often when busy, once-and-sleep when idle, self-healing on boot.
// The scheduler decides on the maintain tick (cheap timestamp checks only)
// and dispatches each round to a detached worker-pool goroutine (issue
// #484): a ~10min fleet round must never stall the maintain goroutine's
// session-liveness poll grid and rotation. Dispatch is single-flight — a
// round already in flight suppresses the next tick — and every round is
// wg-tracked under a pool-rooted context Shutdown cancels and waits for.
//
// Cadence tiers from pool traffic (lastActive, written by every successful
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
// tick), as are tokens whose cached quota is still fresh (refreshed within
// quotaProbeFreshWindow by any probe/admission/seed write — a fully-fresh
// roster yields a cheap no-op round). The round probes with a small fixed
// worker pool under a round-level deadline instead of a sequential
// stagger. The first upstream 429 aborts the round and doubles the
// effective interval (cap 30m, reset to the base cadence on the next
// 429-free full round); a fleet-wide 503 overload (WaitingRoom) aborts the
// same way and additionally damps the next round to a sparse canary sample
// for the first minute, so a still-saturated upstream sees a trickle, not
// an immediate full-roster re-burst.
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

// quotaProbeFireTimeout bounds one token's session-less probe (25s: raised
// from 10s for slow upstream tails on large fleets, still under the 30s
// SESSION_CALL_TIMEOUT envelope). A named const, not a knob: the config
// catalog spans eight files per key, which is not trivial for a tuning
// constant with a safe static value.
const quotaProbeFireTimeout = 25 * time.Second

// smartProbeWorkers is the round's fixed probe concurrency (issue #484): a
// small bounded pool replaces the strictly-sequential 1–3s stagger, so
// round latency scales sub-linearly with fleet size without bursting
// upstream like an unbounded fan-out would.
const smartProbeWorkers = 4

// quotaProbeRoundTimeout bounds a whole round: a wedged tail (every probe
// timing out at quotaProbeFireTimeout on a large fleet) aborts instead of
// holding the single-flight slot — and Shutdown — open indefinitely. Sized
// above the worst case for a large pooled fleet (80 tokens / 4 workers ×
// 25s ≈ 8m would need every probe to time out; the timeout only fires
// past that).
const quotaProbeRoundTimeout = 10 * time.Minute

// quotaProbeFreshWindow is how long a token's cached quota counts as fresh:
// a token probed (or admitted, or boot-seeded) within this window sits out
// the next round. Matches the default active cadence, so a steady-state
// fleet still probes every due round (quota ages exactly one interval),
// while out-of-band refreshes (manual Probe-all, visit auto-probe) spare
// their tokens and a fully-fresh roster yields a no-op round.
const quotaProbeFreshWindow = 60 * time.Second

// smartProbeOverloadSparseWindow is how long after a fleet-wide overload
// abort (503/WaitingRoom) rounds stay sparse: the next round inside this
// window probes at most smartProbeOverloadSparseCap tokens instead of the
// full roster, so a still-saturated upstream sees a canary trickle.
const smartProbeOverloadSparseWindow = time.Minute

// smartProbeOverloadSparseCap caps the probes of one sparse round.
const smartProbeOverloadSparseCap = 2

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
	// inflight marks a dispatched round still running: ticks while set
	// suppress the next round (single-flight) without consuming the
	// kick, so a membership change mid-round still fires after it.
	inflight bool
	// overloadedAt is when the last round aborted on a fleet-wide
	// overload (503/WaitingRoom): rounds inside
	// smartProbeOverloadSparseWindow after it probe at most
	// smartProbeOverloadSparseCap tokens (sparse canary sample).
	overloadedAt time.Time
}

func (s *smartProbeState) mult() int {
	if s.backoff < 1 {
		return 1
	}
	return s.backoff
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
//
// The tick itself never probes: it runs only the cheap scheduler decision
// on the maintain goroutine and dispatches a due round to a detached
// goroutine (single-flight, wg-tracked, Shutdown-cancelable), so a slow
// fleet round never blocks maintainTick's rotation and liveness work.
func (p *Pool) smartProbeTickAt(ctx context.Context, now time.Time) {
	cfg := p.cfg.Load()
	if cfg == nil || !cfg.QuotaAutoProbe {
		return
	}
	// Shutdown is terminal: never dispatch past it. Shutdown wg-waits the
	// in-flight rounds, so a wg.Add after its Wait observed zero would be
	// a WaitGroup-misuse panic; draining is set before Shutdown waits.
	if p.draining.Load() {
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
	// Single-flight: a round already in flight suppresses this tick. The
	// kick/wake/lastProbe state is deliberately left untouched, so a
	// membership kick that landed mid-round still forces the tick after
	// this round lands (the new member is not in this round's snapshot).
	if p.smartProbe.inflight {
		p.smartProbe.mu.Unlock()
		p.logger.Debug("pool: smart probe round already in flight, tick suppressed")
		return
	}
	p.smartProbe.kick = false
	p.smartProbe.bootDone = true
	// A fleet-wide overload abort damps the next round: inside the sparse
	// window the round samples at most a couple of tokens instead of
	// re-bursting the whole roster at a still-saturated upstream.
	sparse := !p.smartProbe.overloadedAt.IsZero() &&
		now.Before(p.smartProbe.overloadedAt.Add(smartProbeOverloadSparseWindow))
	p.smartProbe.inflight = true
	p.wg.Add(1)
	roundCtx, roundCancel := context.WithTimeout(p.probeCtx, quotaProbeRoundTimeout)
	p.smartProbe.mu.Unlock()

	snapshot := *toks
	go func() {
		defer p.wg.Done()
		defer roundCancel()
		fired, limited, overloaded, canceled := p.smartProbeRound(roundCtx, snapshot, now, sparse)

		p.smartProbe.mu.Lock()
		defer p.smartProbe.mu.Unlock()
		p.smartProbe.inflight = false
		if canceled {
			return
		}
		p.smartProbe.lastProbe = time.Now()
		if limited || overloaded {
			if p.smartProbe.backoff < 1 {
				p.smartProbe.backoff = 2
			} else if p.smartProbe.backoff < maxSmartProbeBackoff {
				p.smartProbe.backoff *= 2
			}
		} else if fired > 0 && !sparse {
			// Only a full clean round resets the backoff: a sparse
			// canary sample is not evidence the fleet is healthy.
			p.smartProbe.backoff = 1
		}
		if overloaded {
			p.smartProbe.overloadedAt = time.Now()
		}
		if tier == quotaTierIdle {
			p.smartProbe.idleSlept = true
		} else {
			p.smartProbe.idleSlept = false
		}
	}()
}

// smartProbeInflight reports whether a probe round is currently running.
// Production only logs it; tests poll it to await a dispatched round.
func (p *Pool) smartProbeInflight() bool {
	p.smartProbe.mu.Lock()
	defer p.smartProbe.mu.Unlock()
	return p.smartProbe.inflight
}

// smartProbeRound probes each eligible token once with a small fixed worker
// pool under the round context's deadline. Tokens are fed in roster order,
// so an abort spares the highest indexes first. The first upstream 429
// aborts the round (the remaining tokens wait for the doubled interval); a
// fleet-wide 503 overload (WaitingRoom) aborts the same way and is
// reported separately so the scheduler can damp the next round. A 429
// arrives two ways: an unparseable body classifies to ErrRateLimited, while
// a structured quota body parses into a SessionState with Status
// "rate_limited" (the quota it carries is still cached by ProbeToken — the
// abort only spares the other tokens). Returns the success count plus
// whether the round hit a 429, a fleet overload, or the context died
// mid-round (Shutdown/round deadline: no backoff change, the next tick
// retries on the stale timer).
func (p *Pool) smartProbeRound(ctx context.Context, toks []*tokenEntry, now time.Time, sparse bool) (fired int, limited, overloaded, canceled bool) {
	eligible := make([]int, 0, len(toks))
	for i, tok := range toks {
		if smartProbeSkipToken(tok, now) {
			continue
		}
		if !smartProbeQuotaStale(tok, now) {
			continue
		}
		eligible = append(eligible, i)
	}
	if len(eligible) == 0 {
		p.logger.Debug("pool: smart probe round skipped, roster quota fresh")
		return 0, false, false, false
	}
	if sparse && len(eligible) > smartProbeOverloadSparseCap {
		p.logger.Info("pool: smart probe sparse round after overload abort, sampling roster",
			"probes", smartProbeOverloadSparseCap, "eligible", len(eligible))
		eligible = eligible[:smartProbeOverloadSparseCap]
	}

	roundCtx, abort := context.WithCancel(ctx)
	defer abort()
	jobs := make(chan int)
	var mu sync.Mutex
	aborted := false
	var wg sync.WaitGroup
	for w := 0; w < smartProbeWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if roundCtx.Err() != nil {
					return
				}
				st, err := p.smartProbeOne(roundCtx, i, toks[i])
				mu.Lock()
				switch {
				case roundCtx.Err() != nil || aborted:
					// The round outcome is already decided (abort or
					// parent cancel): record nothing more.
				case err != nil && errors.Is(err, upstream.ErrRateLimited):
					limited = true
					aborted = true
					abort()
				case err != nil && errors.Is(err, upstream.ErrWaitingRoom):
					overloaded = true
					aborted = true
					abort()
				case err != nil:
					// Warn-only failure (logged in smartProbeOne):
					// the round continues with the next token.
				case st != nil && st.Status == "rate_limited":
					limited = true
					aborted = true
					abort()
				default:
					fired++
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, i := range eligible {
		select {
		case <-roundCtx.Done():
			break feed
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil && !aborted {
		return fired, false, false, true
	}
	return fired, limited, overloaded, false
}

// smartProbeQuotaStale reports whether the token's cached quota is too old
// to trust. A token with no quota data (never probed, or a probe that
// carried none) is always stale. Freshness is judged against the session's
// own QuotaSavedAt — the existing last-probe timestamp stamped by every
// probe/admission/seed write — so out-of-band refreshes (manual Probe-all,
// visit auto-probe, boot seed) spare the token in the next scheduled round.
func smartProbeQuotaStale(tok *tokenEntry, now time.Time) bool {
	snap := tok.session.Snapshot()
	if len(snap.QuotaByModel) == 0 || snap.QuotaSavedAt.IsZero() {
		return true
	}
	age := now.Sub(snap.QuotaSavedAt)
	if age < 0 {
		age = 0
	}
	return age >= quotaProbeFreshWindow
}

// smartProbeOne issues one session-less ProbeToken with a bounded context,
// warn-only on failure. It never records usage: probes stay out of the
// ledgers, lastActive, and requestsServed by construction. The live state
// rides along so the round can spot a 429 that parsed as quota data. The
// probe runs against the snapshot entry directly (not a roster index), so
// a membership change mid-round can never mis-target another account.
func (p *Pool) smartProbeOne(ctx context.Context, i int, tok *tokenEntry) (*upstream.SessionState, error) {
	label := tokenEntryLabel(tok)
	fire, cancel := context.WithTimeout(ctx, quotaProbeFireTimeout)
	st, err := tok.client.ProbeAccount(fire)
	if err == nil && st != nil {
		tok.session.UpdateQuotaFromProbe(st)
	}
	cancel()
	if err != nil {
		switch {
		case errors.Is(err, upstream.ErrRateLimited):
			p.logger.Warn("pool: smart probe rate-limited, aborting round", "token", i+1, "token_label", label, "err", err)
		case errors.Is(err, upstream.ErrWaitingRoom):
			p.logger.Warn("pool: smart probe overloaded, aborting round", "token", i+1, "token_label", label, "err", err)
		default:
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
