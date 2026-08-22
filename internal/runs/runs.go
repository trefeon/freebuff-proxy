// Package runs implements the per-agent FreeBuff agent-run lifecycle for a
// single token: lazy START on first use, 6h rotation, FINISH drain, 30-min
// auth cooldown, and a shutdown drain. Port of
// reference/proxy-freebuff/lib/runs.js and freebuff2api-quorinex
// run_manager.go (tokenPool half), adapted to this project's layout: the
// session manager is owned by the caller (pool) and only used here for the
// shutdown EndSession, and the pool â€” not this package â€” decides which token
// serves a request.
//
// Concurrency: all run bookkeeping is guarded by the manager mutex; no lock
// is held across upstream calls. Rotation swaps the current run under the
// lock and hands the old one to an async finishIfReady, so concurrent
// acquires are race-safe.
package runs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// shutdownTimeout bounds Shutdown when the caller passes a context without a
// deadline (PRD §5: "10s force deadline").
const shutdownTimeout = 10 * time.Second

// ErrShuttingDown is returned by Acquire/Precreate/Prewarm (via rotate)
// once Shutdown has begun: the manager has been (or is being) drained and
// the deferred-finish worker is stopped, so a run STARTed now would never
// be FINISHed (P3). rotate re-checks the flag after its upstream StartRun
// returns and discards/finishes the freshly started run inline instead of
// tracking it.
var ErrShuttingDown = errors.New("runs: manager shutting down; new run starts refused")

// Defaults for the bounded deferred-FINISH queue (issue #90) and the
// draining-runs list bounds (issue #55). Overridable via
// runs.Options (pool wires config knobs RUN_FINISH_QUEUE_SIZE /
// RUN_FINISH_INLINE_TIMEOUT / RUNS_DRAIN_QUEUE_CAP / RUNS_DRAIN_TTL).
const (
	defaultFinishQueueSize     = 64
	defaultInlineFinishTimeout = 250 * time.Millisecond
	defaultDrainQueueCap       = 64
	defaultDrainTTL            = 10 * time.Minute
)

// Options configures a RunManager: the rotation interval, the bounded
// deferred-FINISH worker queue bounds (#90/#55), and the optional
// session-state store for run persistence across restarts (#40). Zero
// values fall back to the package defaults.
type Options struct {
	RotationInterval    time.Duration
	FinishQueueSize     int
	InlineFinishTimeout time.Duration
	DrainQueueCap       int
	DrainTTL            time.Duration
	Store               *session.Store
}

// runFlight coordinates single-flight coalescing for concurrent StartRun calls.
type runFlight struct {
	done chan struct{}
	err  error
}

// RunSnapshot is a best-effort view of the manager state for healthz.
type RunSnapshot struct {
	ActiveRuns    int
	CooldownUntil time.Time
	Requests      int
	BanError      *upstream.BanError
	// BannedUntil is the ban window deadline; BanError is only "live"
	// while now < BannedUntil (mirrors BanError()'s time check). The pool
	// gates its ban risk label on it so an expired ban is not sticky.
	BannedUntil time.Time
}

// RunManager owns the current runs (one per agent) plus the draining list
// for a single token.
type RunManager struct {
	client           *upstream.Client
	session          *session.Manager
	rotationInterval time.Duration

	mu            sync.Mutex
	runs          map[string]*Run       // agentID â†’ current run
	starting      map[string]*runFlight // agentID â†’ in-flight start
	draining      []*Run                // rotated runs awaiting FINISH
	cooldownUntil time.Time
	// rateLimit is the last 429 rate-limit error applied to this token's
	// cooldown. It is surfaced by RateLimitError() so exhausted tokens
	// keep returning 429 + Retry-After instead of a generic 502 while the
	// cooldown window is active.
	rateLimit *upstream.RateLimitError
	// banUntil is set when the account is banned; Acquire rejects with the
	// remembered ban error until the unban time.
	banUntil time.Time
	ban      *upstream.BanError
	// countryBlock is the last country-block error applied to this token's
	// cooldown. It is surfaced by CountryBlockedError() so a region-blocked
	// token keeps returning the block error instead of re-hitting upstream
	// during the window (mirrors the rate-limit/ban memory).
	countryBlock *upstream.CountryBlockedError
	countryUntil time.Time
	// ipCapped is the last ip_capped admission refusal applied to this
	// token's cooldown. Surfaced by IpCappedError() during its short window
	// (mirrors rateLimit) so an IP-capped token keeps returning 429
	// ip_capped + Retry-After instead of a generic cooldown 502.
	ipCapped      *upstream.IpCappedError
	ipCappedUntil time.Time
	// ipCappedReAdmits counts how many times this token has been refused
	// ip_capped during the current Pacific day (issue #118): after
	// maxIpCappedReAdmitsPerDay refusals the token is locked until the
	// next Pacific midnight instead of re-admitting in a pacing loop.
	// Guarded by mu.
	ipCappedReAdmits int
	// ipCappedDayReset is the Pacific midnight that ends the day
	// ipCappedReAdmits was counted in (upstream.NextPacificMidnight at the
	// first refusal that day); a new midnight resets the budget. Guarded
	// by mu.
	ipCappedDayReset time.Time
	// totalRequests is the cumulative count of Acquire leases handed out.
	// It is kept separate from the per-run counters because rotated runs
	// that get FINISHed leave the active+draining sets and would otherwise
	// take their request counts out of Snapshot.
	totalRequests int

	// Deferred-FINISH queue (issue #90): rotated/drained runs, chat steps,
	// and child-run creation are processed by one background worker per
	// finishQueue is bounded (Options.FinishQueueSize); when it is
	// full the caller runs the job inline bounded by inlineFinishTimeout.
	// finishStop is closed once (finishOnce) by Shutdown; the worker drains
	// the queue and exits, tracked by finishWg. finishStartOnce starts the
	// worker on first use. finishExited is closed by the worker on exit
	// (test hook for goroutine-leak assertions).
	finishQueue         chan asyncJob
	finishStop          chan struct{}
	finishDrainCtx      context.Context // bounded by Shutdown's deadline; the drain loop abandons queued jobs when it expires (review P2 â€” unbounded queue drain could stall shutdown for minutes)
	finishOnce          sync.Once
	finishStartOnce     sync.Once
	finishWg            sync.WaitGroup
	finishExited        chan struct{}
	inlineFinishTimeout time.Duration

	// keptForPersistence is set by Shutdown when run persistence kept the
	// active runs alive across restart (issue #40). Pool.Shutdown reads it
	// to avoid a spurious "runs left after shutdown" warning on a clean
	// shutdown of a persisted deployment (review P3).
	keptForPersistence bool
	// shuttingDown is set at the START of Shutdown, before the drain: no
	// new run may be STARTed from that point on (P3). An in-flight request
	// still in its acquire phase when the drain begins would otherwise
	// rotate a fresh run into the cleared manager after the finish worker
	// stopped — that run would never be FINISHed. rotate consults it both
	// before the upstream StartRun and after it returns. Guarded by mu.
	shuttingDown bool
	// drainQueueCap / drainTTL bound the draining list (issue #55).
	drainQueueCap int
	drainTTL      time.Duration

	// store persists active runs across restarts (SESSION_PERSIST, issue
	// #40); nil disables. key is the stable token hash
	// (upstream.Client.TokenKey) mirroring the session store's key space.
	store *session.Store
	key   string
}

// NewRunManager builds the manager for one token. rotationInterval is how
// long a run lives before it is rotated (config ROTATION_INTERVAL, default
// 6h). The session manager is used only for Shutdown's EndSession.
func NewRunManager(client *upstream.Client, session *session.Manager, rotationInterval time.Duration) *RunManager {
	return NewRunManagerOpts(client, session, Options{RotationInterval: rotationInterval})
}

// NewRunManagerOpts builds the manager with full Options (rotation
// interval plus the bounded finish queue and draining-list bounds from
// issues #90/#55 and optional run persistence from #40). Zero option
// values fall back to the package defaults.
func NewRunManagerOpts(client *upstream.Client, session *session.Manager, opts Options) *RunManager {
	queueSize := opts.FinishQueueSize
	if queueSize < 1 {
		queueSize = defaultFinishQueueSize
	}
	inlineTimeout := opts.InlineFinishTimeout
	if inlineTimeout <= 0 {
		inlineTimeout = defaultInlineFinishTimeout
	}
	drainCap := opts.DrainQueueCap
	if drainCap < 1 {
		drainCap = defaultDrainQueueCap
	}
	drainTTL := opts.DrainTTL
	if drainTTL <= 0 {
		drainTTL = defaultDrainTTL
	}
	m := &RunManager{
		client:              client,
		session:             session,
		rotationInterval:    opts.RotationInterval,
		runs:                make(map[string]*Run),
		starting:            make(map[string]*runFlight),
		finishQueue:         make(chan asyncJob, queueSize),
		finishStop:          make(chan struct{}),
		finishExited:        make(chan struct{}),
		inlineFinishTimeout: inlineTimeout,
		drainQueueCap:       drainCap,
		drainTTL:            drainTTL,
	}
	if client != nil {
		m.key = client.TokenKey()
	}
	if opts.Store != nil {
		m.store = opts.Store
	}
	return m
}

// SetStore injects the shared session-state store used for run persistence
// (SESSION_PERSIST, issue #40). The pool calls this on SetSessionStore for
// the fixed-token managers (built before the store exists) and passes the
// store through Options for runtime-added tokens. A nil store disables run
// persistence. Runs already tracked keep their state; persistence applies
// to subsequent START/FINISH transitions.
func (m *RunManager) SetStore(store *session.Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
}

// KeptForPersistence reports whether the last Shutdown preserved the active
// runs for restart-resume (issue #40) instead of FINISHing them.
func (m *RunManager) KeptForPersistence() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keptForPersistence
}

// Acquire returns the current run for agentID, starting one on first use or
// rotating when the current run has reached the rotation interval. The
// rotated run is pushed to the draining list and FINISHed asynchronously.
// The returned run has its inflight and Requests counters incremented;
// callers must Release it when the request completes or fails.
func (m *RunManager) Acquire(ctx context.Context, agentID string) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// The re-validation loop converges: an idle FinishAllRuns (or a
	// concurrent Shutdown) may clear the run map between the initial read
	// and the re-read below, which would otherwise surface a phantom
	// "run missing after rotation" failure to the caller. Each pass either
	// returns a lease or re-creates the current run under the manager
	// mutex, so a cleared map is re-populated on the next iteration.
	// FinishAllRuns clears at most once per idle stretch, so production
	// converges in one retry; ctx cancellation bounds the loop.
	for {
		m.mu.Lock()
		if now := time.Now(); now.Before(m.cooldownUntil) {
			until := m.cooldownUntil
			m.mu.Unlock()
			return nil, fmt.Errorf("token cooling down until %s", until.Format(time.RFC3339))
		}
		run := m.runs[agentID]
		needsRotate := run == nil || time.Since(run.StartedAt) >= m.rotationInterval
		m.mu.Unlock()

		if needsRotate {
			if err := m.rotate(ctx, agentID); err != nil {
				return nil, err
			}
		}

		m.mu.Lock()
		// A concurrent acquire may have rotated again while we were
		// starting; the lease must always point at the current run.
		run = m.runs[agentID]
		if run != nil {
			run.inflight++
			run.Requests++
			m.totalRequests++
			m.mu.Unlock()
			return run, nil
		}
		m.mu.Unlock()

		// The current run vanished mid-acquire (concurrent FinishAllRuns);
		// loop and re-validate instead of failing the request.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

// Release decrements the inflight counter of a leased run. Safe on nil.
// Draining finishes happen on the maintain tick or the next rotation;
// when the LAST lease of a draining run releases (a FinishAllRuns
// deferral, a rotation, or an abandonment), the FINISH is re-queued
// immediately — otherwise a run drained with an outstanding lease would
// never reach finishIfReadyCtx after its final release (P1).
func (m *RunManager) Release(run *Run) {
	if run == nil {
		return
	}
	m.mu.Lock()
	if run.inflight > 0 {
		run.inflight--
	}
	if run.inflight > 0 || !m.runDrainingLocked(run) {
		m.mu.Unlock()
		return
	}
	// Last lease on a draining run: drop it from the active set (a
	// drain-marked run may still be current) so finishIfReadyCtx does not
	// skip it as still-current, then re-queue the deferred FINISH.
	if current, ok := m.runs[run.AgentID]; ok && current == run {
		delete(m.runs, run.AgentID)
	}
	m.mu.Unlock()
	m.enqueueFinish(run)
}

// InflightCount returns the total in-flight request count across all runs,
// or for a specific agentID if provided.
func (m *RunManager) InflightCount(agentID ...string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(agentID) > 0 && agentID[0] != "" {
		if r := m.runs[agentID[0]]; r != nil {
			return r.inflight
		}
		return 0
	}
	count := 0
	for _, r := range m.runs {
		count += r.inflight
	}
	for _, r := range m.draining {
		count += r.inflight
	}
	return count
}

// Invalidate drops the active run for agentID without finishing it (e.g. on runId not found).
func (m *RunManager) Invalidate(agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.runs, agentID)
	if m.store != nil && m.key != "" {
		m.store.RemoveRun(m.key, agentID)
	}
}

// Maintain rotates aged runs and FINISHes the draining list. Runs with
// outstanding inflight leases or an in-flight FINISH are skipped. Best
// effort: failures are logged, never returned (background job). While the
// token is cooling down (auth rejection, rate limit, ban) the pass returns
// immediately: no rotate attempts, no draining FINISH, no log â€” retrying
// upstream work during a cooldown looks like abuse and would log the
// "token cooling down" rotate failure once per maintain tick (observed in
// production). The pool logs the skip.
func (m *RunManager) Maintain(ctx context.Context) {
	if time.Now().Before(m.CooldownUntil()) {
		return
	}
	m.mu.Lock()
	var toRotate []string
	for agentID, run := range m.runs {
		if time.Since(run.StartedAt) >= m.rotationInterval {
			toRotate = append(toRotate, agentID)
		}
	}
	// Bound the draining list before re-enqueuing its FINISHes: entries
	// past the TTL or cap are force-dropped (issue #55) so a persistently
	// failing FINISH cannot grow the list without bound.
	m.pruneDrainingLocked()
	draining := append([]*Run(nil), m.draining...)
	m.mu.Unlock()

	for _, agentID := range toRotate {
		if err := m.rotate(ctx, agentID); err != nil {
			slog.Debug("runs: maintain rotate failed", "agent_id", agentID, "err", err)
		}
	}
	// Deferred-FINISH through the bounded queue (issue #90): the maintain
	// tick never blocks on upstream FINISH calls; the worker (or the inline
	// fallback) owns them. finishIfReady skips busy/finishing runs, so a
	// run with an outstanding lease stays draining for the next pass.
	for _, run := range draining {
		m.enqueueFinish(run)
	}
}

// Shutdown FINISHes every run (active and draining) and ends the upstream
// session. When ctx carries no deadline a 10s force deadline is applied
// (PRD Â§5.5 shutdown sequence).
func (m *RunManager) Shutdown(ctx context.Context) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
	}

	// P3: refuse new run STARTs from this instant: an in-flight request
	// still in its acquire phase when the drain begins must not start a
	// fresh run after the manager is cleared — the finish worker is
	// stopped, so that run would never be FINISHed. rotate re-checks the
	// flag after its upstream StartRun returns and discards (inline-
	// FINISHing) the fresh run instead of tracking it.
	m.mu.Lock()
	m.shuttingDown = true
	m.mu.Unlock()

	// Stop the deferred-job worker first (issue #90): it drains whatever is
	// queued, so its FINISHes land before our own claim below, and no job
	// can start after we snapshot the runs. The worker's finishing-flag
	// claims are respected by the claim loop (finishing runs are skipped).
	// The drain is bounded by the same deadline as the rest of Shutdown: a
	// saturated queue (up to FinishQueueSize jobs x session-call timeout)
	// must not stall shutdown for minutes (review P2).
	m.finishDrainCtx = ctx
	m.finishOnce.Do(func() { close(m.finishStop) })
	// Ensure the worker exists BEFORE waiting: its finishWg.Add(1) must be
	// ordered before the Wait below â€” a lazy first-start racing Shutdown
	// would be a WaitGroup Add/Wait race and Shutdown could proceed without
	// the late worker. If it was never started, it exits immediately on
	// the closed stop channel and balances the count.
	m.startFinishWorker()
	// Wait for the drain, but never past the caller's deadline: if the
	// worker is still draining after ctx expires, the remaining jobs are
	// abandoned (best-effort FINISHes; the upstream connection dies with
	// the process anyway).
	waitDone := make(chan struct{})
	go func() {
		m.finishWg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-ctx.Done():
		// Report the queue and run counts, never the manager struct itself
		// (a *RunManager dump would leak internal state to the log).
		m.mu.Lock()
		runs := len(m.runs)
		m.mu.Unlock()
		slog.Warn("runs: finish-queue drain exceeded shutdown deadline; abandoning remaining jobs",
			"pending_jobs", len(m.finishQueue),
			"runs", runs,
			"key", m.key)
	}
	_ = ctx.Err() // lint: ctx is still used by the caller below

	// Run persistence (issue #40): with a store, keep the active runs alive
	// across the restart like the session keep-alive â€” FINISHing them here
	// would force the next process to re-START and burn upstream calls. The
	// runs are already persisted on START; re-save so the latest Requests
	// counter survives.
	if m.store != nil && m.key != "" {
		m.mu.Lock()
		snapshot := make([]*Run, 0, len(m.runs))
		for _, run := range m.runs {
			// cloneRun, never *run: the Run carries an atomic.Int64 step
			// counter that must not be copied after first use.
			snapshot = append(snapshot, m.cloneRun(run))
		}
		// Drained (rotated) runs are finished, never resumed: best-effort
		// FINISH them NOW â€” the worker is stopped, so this is their last
		// chance, and a stale store entry must not resurrect a finished
		// run on the next boot.
		draining := make([]*Run, 0, len(m.draining))
		for _, run := range m.draining {
			if run.finishing {
				continue
			}
			run.finishing = true
			draining = append(draining, run)
		}
		m.mu.Unlock()
		for _, run := range snapshot {
			m.persistRun(run)
		}
		var errs []string
		for _, run := range draining {
			status, steps, totalSteps := m.finishPayload(run)
			if err := m.client.FinishRun(ctx, run.RunID, status, totalSteps, steps, ""); err != nil {
				errs = append(errs, fmt.Sprintf("finish run %s: %v", run.RunID, err))
			} else {
				m.removeRun(run)
			}
		}
		m.keptForPersistence = true
		if err := m.session.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Sprintf("shutdown session: %v", err))
		}
		if len(errs) > 0 {
			slog.Warn("runs: shutdown with errors", "errors", strings.Join(errs, "; "))
		}
		return
	}

	m.mu.Lock()
	// Skip runs with a FINISH already in flight (an async rotate drain
	// owns them): re-FINISHing the same run id upstream is a duplicate
	// call the drain goroutine is already completing. Claim the rest by
	// setting finishing so a concurrently starting finishIfReady cannot
	// double-FINISH a run we are about to finish here.
	all := make([]*Run, 0, len(m.runs)+len(m.draining))
	for _, run := range m.runs {
		if run.finishing {
			continue
		}
		run.finishing = true
		all = append(all, run)
	}
	for _, run := range m.draining {
		if run.finishing {
			continue
		}
		run.finishing = true
		all = append(all, run)
	}
	m.runs = make(map[string]*Run)
	m.draining = nil
	m.mu.Unlock()

	var errs []string
	for _, run := range all {
		status, steps, totalSteps := m.finishPayload(run)
		if err := m.client.FinishRun(ctx, run.RunID, status, totalSteps, steps, ""); err != nil {
			errs = append(errs, fmt.Sprintf("finish run %s: %v", run.RunID, err))
		}
	}
	if err := m.session.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("shutdown session: %v", err))
	}
	if len(errs) > 0 {
		slog.Warn("runs: shutdown with errors", "errors", strings.Join(errs, "; "))
	}
}

// Snapshot returns a best-effort view of the manager state.
func (m *RunManager) Snapshot() RunSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := RunSnapshot{ActiveRuns: len(m.runs), CooldownUntil: m.cooldownUntil, Requests: m.totalRequests, BanError: m.ban, BannedUntil: m.banUntil}
	return s
}

// Prewarm starts a run for every agent that does not already have a fresh
// one, best-effort (per-agent errors are logged, never returned). Used at
// pool boot so the first request does not pay the START latency.
func (m *RunManager) Prewarm(ctx context.Context, agentIDs []string) {
	for _, agentID := range agentIDs {
		m.mu.Lock()
		needs := m.runs[agentID] == nil
		m.mu.Unlock()
		if !needs {
			continue
		}
		if err := m.rotate(ctx, agentID); err != nil {
			slog.Debug("runs: prewarm failed", "agent_id", agentID, "err", err)
		}
	}
}

// rotate starts a fresh run for agentID, pushing the previous current run
// (if any) onto the draining list and finishing it asynchronously. Single-flight
// coalescing ensures concurrent callers for the same agent wait on a single
// upstream StartRun call rather than launching duplicate requests.
func (m *RunManager) rotate(ctx context.Context, agentID string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		m.mu.Lock()
		if m.shuttingDown {
			m.mu.Unlock()
			return ErrShuttingDown
		}
		if now := time.Now(); now.Before(m.cooldownUntil) {
			until := m.cooldownUntil
			m.mu.Unlock()
			return fmt.Errorf("token cooling down until %s", until.Format(time.RFC3339))
		}
		if run := m.runs[agentID]; run != nil && time.Since(run.StartedAt) < m.rotationInterval {
			m.mu.Unlock()
			return nil // a concurrent rotator already refreshed it
		}
		if flight, ok := m.starting[agentID]; ok {
			ch := flight.done
			m.mu.Unlock()
			select {
			case <-ch:
				if flight.err != nil {
					if (errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded)) && ctx.Err() == nil {
						// Leader goroutine canceled/timed out, but this waiter's context is still active.
						// Loop back to try becoming leader.
						continue
					}
					return flight.err
				}
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		// We are the leader for starting this agent's run.
		flight := &runFlight{done: make(chan struct{})}
		if m.starting == nil {
			m.starting = make(map[string]*runFlight)
		}
		m.starting[agentID] = flight
		m.mu.Unlock()

		// Issue #40: resume a persisted run instead of STARTing a fresh one
		// when a restart left an active run behind. Only runs started within
		// the rotation interval are adopted â€” a stale entry is dropped so
		// the upstream's own rotation wins. Best-effort: the store read
		// never fails the rotate.
		if m.store != nil && m.key != "" {
			if pr := m.store.LoadRun(m.key, agentID); pr != nil {
				if pr.RunID != "" && time.Since(pr.StartedAt) < m.rotationInterval {
					m.mu.Lock()
					if m.shuttingDown {
						m.mu.Unlock()
						return ErrShuttingDown
					}
					oldRun := m.runs[agentID]
					m.runs[agentID] = &Run{
						AgentID:        agentID,
						RunID:          pr.RunID,
						StartedAt:      pr.StartedAt,
						TraceSessionID: pr.TraceSessionID,
						Requests:       pr.Requests,
					}
					flight.err = nil
					close(flight.done)
					delete(m.starting, agentID)
					if oldRun != nil {
						m.appendDrainingLocked(oldRun)
					}
					m.mu.Unlock()
					if oldRun != nil {
						m.enqueueFinish(oldRun)
					}
					slog.Debug("runs: run resumed from store", "agent_id", agentID, "run_id", pr.RunID)
					return nil
				}
				m.store.RemoveRun(m.key, agentID)
			}
		}

		runID, err := m.client.StartRun(ctx, agentID)

		m.mu.Lock()
		flight.err = err
		close(flight.done)
		delete(m.starting, agentID)

		if err != nil {
			m.mu.Unlock()
			return err
		}
		if m.shuttingDown {
			// Shutdown began while the upstream START was in flight: the
			// manager was (or is being) drained and the finish worker is
			// stopped, so tracking this fresh run would leave it never
			// FINISHed (P3). Discard it — best-effort FINISH it inline
			// (bounded by the shutdown deadline) so the upstream agent run
			// does not leak until its own rotation expiry.
			m.mu.Unlock()
			m.finishInline(runID, agentID)
			return ErrShuttingDown
		}
		// Mint the trace session id before logging so the run-started line
		// and every chat trace of this run share it (T3, D2).
		traceSessionID := newTraceSessionID()
		slog.Debug("runs: run started", "agent_id", agentID, "run_id", runID, "trace_session_id", traceSessionID)

		newRun := &Run{AgentID: agentID, RunID: runID, StartedAt: time.Now(), TraceSessionID: traceSessionID}
		oldRun := m.runs[agentID]
		m.runs[agentID] = newRun
		if oldRun != nil {
			m.appendDrainingLocked(oldRun)
		}
		m.mu.Unlock()

		m.persistRun(newRun)
		if oldRun != nil {
			m.enqueueFinish(oldRun)
		}
		return nil
	}
}

// Precreate starts the run for agentID if none is fresh, without leasing it
// (issue #90a): the pool calls it right after a session admission so the
// first chat on a newly-admitted session does not pay the START latency.
// Best-effort: the caller's Acquire surfaces any real failure through the
// normal path.
func (m *RunManager) Precreate(ctx context.Context, agentID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	run := m.runs[agentID]
	needs := run == nil || time.Since(run.StartedAt) >= m.rotationInterval
	m.mu.Unlock()
	if !needs {
		return nil
	}
	return m.rotate(ctx, agentID)
}
