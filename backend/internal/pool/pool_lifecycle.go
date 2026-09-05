// pool_lifecycle.go — background-job lifecycle: the maintain
// rotation loop (rotate aged runs, advance queued sessions), and the
// session-liveness poll schedule.
package pool

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/runs"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
)

// maintainInterval is how often the background job rotates aged runs and
// advances queued sessions (PRD §3: 60s maintain ticker). Session-liveness
// polls run on their own jittered schedule (see sessionPoll* below), not on
// this coarse grid.
const maintainInterval = time.Minute

// Session-liveness poll cadence (reference/freebuff sdk
// polling-backoff.ts): while active the CLI polls the compact session every
// 30s ±20% (24–36s), capped to remaining+1s near expiry so the poll lands
// just after expires_at; on failure it backs off 20s → 300s (×2 per
// consecutive failure), never scheduling a retry before the server's
// Retry-After floor.
const (
	// sessionPollCheckInterval is the maintain loop's fine-grained wake-up
	// grid for due session polls; rotation/queued-advance stay on
	// maintainInterval.
	sessionPollCheckInterval = 2 * time.Second
	// sessionPollBaseInterval is the CLI's active poll cadence (30s).
	sessionPollBaseInterval = 30 * time.Second
	// sessionPollBackoffBase is the first failure backoff (20s); each
	// consecutive failure doubles it up to sessionPollBackoffMax (300s).
	sessionPollBackoffBase = 20 * time.Second
	sessionPollBackoffMax  = 300 * time.Second
)

// retiredDrainGrace is how long a retired token may sit without a lease
// before maintainTick drops it from the retired map. RemoveLastToken drains
// the entry at removal, so a parked entry's runs are already finished; the
// grace exists to cover an Acquire that loaded the pre-removal snapshot and
// is still mid-admission when the park happens (see RemoveLastToken).
const retiredDrainGrace = 2 * time.Minute

// maintainToken runs one token/entry's per-pass maintenance work (issue
// #264): the cooldown/ban gate, runs.Maintain, the in-flight queued-session
// EnsureSession, and the run Precreate when the queue advances. It is the
// shared body behind maintainTick's per-token loop and bridgeMaintain's
// per-entry loop, so rotation/queued-advance semantics cannot drift between
// the two modes.
func maintainToken(ctx context.Context, sess *session.Manager, runsMgr *runs.RunManager, reg *registry.Registry, cfg *config.Config, label any, logger *slog.Logger) {
	// Same cooldown/ban gate as the poll loop: no queued-session
	// EnsureSession, no rotation while cooling down — and the same live-ban
	// skip so a hard-banned token stops Maintain/rotate traffic (its cooldown
	// deadline is zero until an operator acts).
	if !runsMgr.MaintenanceEligible() {
		return
	}
	mCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	runsMgr.Maintain(mCtx)
	// Same in-flight gate as the poll loop: skip the queued-session GET while
	// a chat is in flight so it cannot kick the active session
	// (reference/freebuff-proxy-hengxin session-manager.js:37-49, 259-260).
	// Active-session liveness polls run on the jittered poll schedule
	// (sessionPollTick / bridgeSessionPollTick) instead.
	if runsMgr.InflightCount() == 0 {
		snap := sess.Snapshot()
		if snap.Status == "queued" {
			if _, err := sess.EnsureSession(mCtx); err != nil {
				logger.Debug("pool: maintain session not ready", "token", label, "err", err)
			} else {
				// Issue #90a: pre-create the run for the session's model
				// agent so the first request on this session does not pay the
				// START latency.
				after := sess.Snapshot()
				if agentID, err := reg.AgentForModel(after.Model); err == nil && agentID != "" {
					_ = runsMgr.Precreate(mCtx, agentID)
				}
			}
		}
	}
	cancel()
}

// pollSession runs one session-liveness poll for a token/entry's session
// manager (issue #264) and computes the next poll schedule: 20s→300s backoff
// on failure (honoring the server's Retry-After floor), the jittered success
// cadence otherwise. failCount is the current consecutive-failure counter; it
// returns the updated counter, the delay to the next poll, and the poll error
// (nil on success) so the caller logs with the same detail as before.
func pollSession(ctx context.Context, sess *session.Manager, cfg *config.Config, failCount int) (int, time.Duration, error) {
	mCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	err := sess.Poll(mCtx)
	cancel()
	if err != nil {
		failCount++
		return failCount, sessionPollBackoffDelay(failCount, sessionPollRetryAfter(err)), err
	}
	return 0, sessionPollSuccessDelay(sess.Snapshot()), nil
}

// Start launches the 60s maintain loop (rotate aged runs + advance queued
// sessions). It stops when ctx is canceled; Pool.Shutdown cancels.
//
// It deliberately does NOT prewarm a run per registry agent per token any
// more. That boot fleet held ~one concurrent agent run per served model on a
// single free account, which is exactly the "proxy fanout" shape upstream
// counts as ban-grade evidence (upstream
// common/src/constants/freebuff-spend-ceilings.ts) and refuses with
// free_mode_run_fanout — a refusal the reference proxies that keep one run
// per (token, agent) never see. Runs START lazily on first use (Acquire),
// so dropping the fleet costs the first request its START latency and
// nothing else.
func (p *Pool) Start(ctx context.Context) {
	p.once.Do(func() {
		runCtx, cancel := context.WithCancel(ctx)
		p.cancel = cancel
		p.wg.Add(1)
		go p.maintainLoop(runCtx)
		p.wg.Add(1)
		go p.backfillLoop(runCtx)
	})
}

// backfillLoop periodically fetches account info (email/id, issue #269) for
// pooled tokens that still have an empty email, and streak position (issue
// #336) for tokens whose streak is missing or older than an hour. It runs
// as a plain background job so neither pool.New nor the Snapshot read path
// ever issues upstream traffic; the first pass fires shortly after Start,
// later passes are throttled to one try per entry per tick via the
// in-flight guards. The dashboard token cards fill in shortly after the
// first poll.
func (p *Pool) backfillLoop(ctx context.Context) {
	defer p.wg.Done()
	first := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		if !first.Stop() {
			select {
			case <-first.C:
			default:
			}
		}
		ticker.Stop()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		case <-ticker.C:
		}
		for _, tok := range *p.roster.Load() {
			if tok.email.Load() == nil {
				p.asyncAccountInfoFetch(tok)
			}
			p.asyncStreakFetch(tok)
		}
	}
}

// --- internals ---

// idleFor is how long the pool has gone without a successful Acquire (0
// when no request ever arrived, so a freshly prewarmed pool is not treated
// as idle). Uses Mutex (not RWMutex): the critical section includes
// time.Since which is cheap but the lock is uncontended outside maintain
// ticks — RWMutex adds complexity with no benefit.
func (p *Pool) idleFor() time.Duration {
	p.lastActiveMu.Lock()
	defer p.lastActiveMu.Unlock()
	if p.lastActive.IsZero() {
		return 0
	}
	return time.Since(p.lastActive)
}

// tryIdleFinish atomically checks whether the pool has been idle past
// cfg.IdleRotationTimeout and, if so, marks idleFinished. Returns true
// only for the first caller past the threshold per idle stretch.
// Atomicity: threshold check and flag set are one lastActiveMu critical
// section so concurrent maintainTick passes cannot both FINISH.
func (p *Pool) tryIdleFinish(cfg *config.Config) bool {
	p.lastActiveMu.Lock()
	defer p.lastActiveMu.Unlock()
	if cfg.IdleRotationTimeout <= 0 {
		return false
	}
	if p.lastActive.IsZero() {
		return false
	}
	if time.Since(p.lastActive) <= cfg.IdleRotationTimeout {
		return false
	}
	if p.idleFinished {
		return false
	}
	p.idleFinished = true
	return true
}

// trySweepIdleSessions atomically checks whether the pool has been idle
// past cfg.SessionIdleEnd and marks sessionsEnded. Returns true only for
// the first caller past the threshold per idle stretch.
// Atomicity: threshold check and flag set are one lastActiveMu critical
// section so concurrent maintainTicks cannot both sweep (TOCTOU).
func (p *Pool) trySweepIdleSessions(cfg *config.Config) bool {
	p.lastActiveMu.Lock()
	defer p.lastActiveMu.Unlock()
	if cfg.SessionIdleEnd <= 0 {
		return false
	}
	if p.lastActive.IsZero() {
		return false
	}
	if time.Since(p.lastActive) <= cfg.SessionIdleEnd {
		return false
	}
	if p.sessionsEnded {
		return false
	}
	p.sessionsEnded = true
	return true
}

// endIdleSessions implements SESSION_IDLE_END: once the pool has been idle
// past cfg.SessionIdleEnd (opt-in, default off), release every fixed
// token's upstream session slot so an overnight-idle proxy stops holding
// daily admission slots. Tradeoff (documented on the config knob): when
// traffic resumes, EnsureSession re-admits each token, consuming a fresh
// daily slot. Best-effort per token — a failed DELETE is logged and the
// session simply persists until the next use (same as leaving the knob
// unset); no retry within the episode.
func (p *Pool) endIdleSessions(ctx context.Context, cfg *config.Config, toks *[]*tokenEntry) {
	for i, tok := range *toks {
		// Upstream calls during a cooldown read as abuse (maintain-pass
		// policy); skip silently and keep the session.
		if !tok.runs.MaintenanceEligible() {
			continue
		}
		// Re-checked immediately before the DELETE: an Acquire can land
		// between the caller's threshold check and this loop.
		if tok.runs.InflightCount() > 0 {
			continue
		}
		mCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		err := tok.session.EndSession(mCtx)
		cancel()
		if err != nil {
			p.logger.Warn("pool: idle session end failed", "err", err, "token_index", i)
		} else {
			p.logger.Info("pool: ended idle session", "token_index", i)
		}
	}
}

// sweepIdleSessions runs the SESSION_IDLE_END sweep when its threshold is
// crossed, exactly once per idle stretch. Called from every maintainTick
// exit path so the knob works with or without IDLE_ROTATION_TIMEOUT and
// regardless of which threshold fires first.
// Atomicity: delegates to trySweepIdleSessions so the threshold check and
// sessionsEnded flag are set in one critical section (TOCTOU fix).
func (p *Pool) sweepIdleSessions(ctx context.Context, cfg *config.Config, toks *[]*tokenEntry) {
	if p.trySweepIdleSessions(cfg) {
		p.endIdleSessions(ctx, cfg, toks)
	}
}

// maintainLoop ticks every maintainInterval: per token, rotate aged runs and
// advance queued sessions. Session-liveness polls run on their own finer
// jittered schedule (sessionPollTick fires when a token's nextPollAt is
// due; see the sessionPoll* constants). When IDLE_ROTATION_TIMEOUT is set,
// the pool pauses this activity after it has been idle past the timeout:
// one pass FINISHes all runs (so no rotation/session-refresh activity
// continues upstream) and every further pass is skipped until the next
// request — Acquire re-creates runs on demand.
func (p *Pool) maintainLoop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(maintainInterval)
	defer ticker.Stop()
	// The poll grid is finer than maintainInterval so the per-token jittered
	// ~30s liveness polls are not quantized onto the 60s rotation
	// grid — a due poll fires on the first grid point at/after nextPollAt.
	pollTicker := time.NewTicker(sessionPollCheckInterval)
	defer pollTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.maintainTick(ctx)
		case <-pollTicker.C:
			p.sessionPollTick(ctx)
		}
	}
}

// maintainTick runs one maintenance pass: the idle handling (see
// maintainLoop), then the per-token rotate/refresh work. Split out of
// maintainLoop so tests can drive a pass without waiting for the
// minute-long ticker.
func (p *Pool) maintainTick(ctx context.Context) {
	toks := p.roster.Load()
	cfg := p.cfg.Load()
	// Drop retired tokens that never saw a slipped lease (their runs were
	// drained at removal). Entries still carrying a lease stay until
	// LeaseRelease drains them; the park grace covers an Acquire that loaded
	// the pre-removal snapshot (see RemoveLastToken).
	p.pruneRetired()
	// Streak-maturity automation rides every pass — including idle
	// stretches, whose quiet accounts are exactly the ones whose streaks
	// need keeping. It never fires on unhealthy accounts (banned, cooling,
	// quarantined, country-blocked) and defaults to dry-run probes.
	p.maturityTick(ctx)
	// Idle handling — tryIdleFinish atomically checks the threshold and
	// marks idleFinished in one lastActiveMu critical section (TOCTOU fix).
	// The first idle pass FINISHes all runs so rotation/refresh stops
	// upstream; sessions are left untouched. Later passes skip per-token
	// work and only sweep.
	if p.tryIdleFinish(cfg) {
		for _, tok := range *toks {
			// Skip tokens with outstanding leases: FINISHing this run
			// would kill an in-flight chat; leave it for rotation once the
			// lease drains (same rule as the bridge idle sweep).
			if tok.runs.InflightCount() > 0 {
				continue
			}
			// Thread the maintain ctx: Pool.Shutdown cancels it first, so a
			// mid-drain FINISH must abort on cancel instead of blocking
			// shutdown for the full upstream call timeout.
			tok.runs.FinishAllRuns(ctx)
		}
		p.sweepIdleSessions(ctx, cfg, toks)
		p.bridgeMaintain(ctx, true)
		return
	}
	if cfg.IdleRotationTimeout > 0 && p.idleFor() > cfg.IdleRotationTimeout {
		// Subsequent idle passes (already FINISHed): still sweep idle
		// bridge entries — without this, entries idle past the idle-eviction TTL
		// are never evicted while the pool stays idle and their sessions
		// stay admitted upstream until expiry.
		p.bridgeMaintain(ctx, true)
		p.sweepIdleSessions(ctx, cfg, toks)
		return
	}
	for i, tok := range *toks {
		// Lift-aware quarantine: a temporary ban's marker may have timed
		// out since the last pass — clear it so the token rejoins the
		// rotation/poll pool instead of staying excluded until an operator
		// unlocks it.
		p.clearLiftedQuarantine(tok)
		maintainToken(ctx, tok.session, tok.runs, p.reg, cfg, i+1, p.logger)
	}
	p.sweepIdleSessions(ctx, cfg, toks)
	// Bridge sweep: drop entries idle past the idle-eviction TTL (runs FINISHed
	// best-effort), maintain the rest like the fixed tokens above.
	p.bridgeMaintain(ctx, false)
}

// sessionPollTick runs the per-token session-liveness polls on their own
// jittered schedule (see the sessionPoll* constants): an active (or
// in-grace ended) session is compact-polled every ~30s ±20% — capped to
// remaining+1s near expiry — with 20s→300s failure backoff honoring the
// server's Retry-After, mirroring the CLI's liveness fingerprint
// (reference/freebuff sdk polling-backoff.ts). Rotation and queued-session
// advance stay on the coarse maintainInterval ticker (maintainTick). The
// poll is skipped while a chat is in flight (the upstream allows one client
// per account at a time; a poll landing mid-chat can kick the active
// session with 428) and while the token cools down, exactly like
// maintainTick.
func (p *Pool) sessionPollTick(ctx context.Context) {
	cfg := p.cfg.Load()
	if cfg.IdleRotationTimeout > 0 && p.idleFor() > cfg.IdleRotationTimeout {
		// Session polls pause with the fixed tokens while idle (the
		// maintain pass already FINISHed every run upstream).
		return
	}
	toks := p.roster.Load()
	for i, tok := range *toks {
		if !tok.runs.MaintenanceEligible() {
			// Cooldown: no session poll (same rule as maintainTick).
			continue
		}
		if tok.runs.InflightCount() > 0 {
			// Mid-chat in-flight gate (same rule as maintainTick): a poll
			// GET can kick the active session (428 waiting_room). Leave the
			// schedule due; the next pass polls once the lease drains.
			continue
		}
		now := time.Now()
		if !tok.nextPollAt.IsZero() && now.Before(tok.nextPollAt) {
			continue
		}
		failures, delay, err := pollSession(ctx, tok.session, cfg, tok.pollFailures)
		if err != nil {
			p.logger.Debug("pool: session poll failed", "token", i+1, "err", err, "retry_in", delay)
		}
		tok.pollFailures = failures
		tok.nextPollAt = now.Add(delay)
	}
	p.bridgeSessionPollTick(ctx, cfg)
}

// sessionPollSuccessDelay returns the delay before the next liveness poll
// after a SUCCESSFUL poll: ~30s ±20% jitter, capped so a poll near expiry
// lands ~1s after expires_at (the CLI observes the status flip then;
// reference/freebuff sdk polling-backoff.ts). Sessions already inside the
// grace drain poll at the plain jittered cadence.
func sessionPollSuccessDelay(snap session.SessionSnapshot) time.Duration {
	d := sessionPollJittered(sessionPollBaseInterval)
	if !snap.ExpiresAt.IsZero() {
		if rem := time.Until(snap.ExpiresAt); rem > 0 && rem+time.Second < d {
			d = rem + time.Second
		}
	}
	return d
}

// sessionPollBackoffDelay returns the delay after a FAILED poll: 20s ×2 per
// consecutive failure (cap 300s) with equal jitter over the lower half of
// the window, and never before the server's Retry-After floor (multiplied
// by 1 ± 0.2 jitter, capped 300s) — polling-backoff.ts semantics.
func sessionPollBackoffDelay(failures int, retryAfter time.Duration) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := sessionPollBackoffBase << min(failures-1, 5)
	if d > sessionPollBackoffMax {
		d = sessionPollBackoffMax
	}
	d = d/2 + time.Duration(sessionRand()%uint64(d/2))
	if retryAfter > 0 {
		// Floor retryAfter to avoid uint64(0) modulo panic when the
		// server's Retry-After is absurdly small (1ns).
		if retryAfter < 5*time.Nanosecond {
			retryAfter = 5 * time.Nanosecond
		}
		ra := retryAfter - retryAfter/5 + time.Duration(sessionRand()%uint64(2*retryAfter/5))
		if ra > d {
			d = ra
		}
		if d > sessionPollBackoffMax {
			d = sessionPollBackoffMax
		}
	}
	return d
}

// sessionPollJittered applies the CLI's symmetric ±20% jitter around d.
func sessionPollJittered(d time.Duration) time.Duration {
	span := d / 5
	return d - span + time.Duration(sessionRand()%uint64(2*span+1))
}

// sessionRand draws one uint64 from crypto/rand (the pool's jitter source,
// matching the upstream client's pattern). A read failure is unrecoverable
// in practice; fall back to the clock rather than panicking in a background
// loop.
func sessionRand() uint64 {
	var b [8]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint64(b[:])
}

// sessionPollRetryAfter extracts the server's Retry-After floor from a
// failed session poll error (0 when the error carries none). The backoff
// never schedules a retry before this floor.
func sessionPollRetryAfter(err error) time.Duration {
	var ue *upstream.UpstreamError
	if errors.As(err, &ue) {
		return ue.RetryAfter
	}
	var rle *upstream.RateLimitError
	if errors.As(err, &rle) {
		return rle.RetryAfter
	}
	var wrr *upstream.WaitingRoomRequiredError
	if errors.As(err, &wrr) {
		return wrr.RetryAfter
	}
	var wr *session.WaitingRoomError
	if errors.As(err, &wr) {
		return wr.RetryAfter
	}
	return 0
}
