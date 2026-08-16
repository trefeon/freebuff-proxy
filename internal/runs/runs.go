// Package runs implements the per-agent FreeBuff agent-run lifecycle for a
// single token: lazy START on first use, 6h rotation, FINISH drain, 30-min
// auth cooldown, and a shutdown drain. Port of
// reference/proxy-freebuff/lib/runs.js and freebuff2api-quorinex
// run_manager.go (tokenPool half), adapted to this project's layout: the
// session manager is owned by the caller (pool) and only used here for the
// shutdown EndSession, and the pool — not this package — decides which token
// serves a request.
//
// Concurrency: all run bookkeeping is guarded by the manager mutex; no lock
// is held across upstream calls. Rotation swaps the current run under the
// lock and hands the old one to an async finishIfReady, so concurrent
// acquires are race-safe.
package runs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// DefaultCooldown is the token cooldown applied on upstream auth rejection
// (PRD §5.3: "401 triggers 30-min token cooldown").
const DefaultCooldown = 30 * time.Minute

// countryBlockCooldown is the token cooldown applied when upstream reports a
// region block (country_blocked): long enough to stop the request hammer
// from re-hitting the blocked admission, short enough to re-probe after the
// client switches egress/VPN.
const countryBlockCooldown = 15 * time.Minute

// shutdownTimeout bounds Shutdown when the caller passes a context without a
// deadline (PRD §5: "10s force deadline").
const shutdownTimeout = 10 * time.Second

// Run is one agent run leased to a caller. Requests counts acquires served
// by this run; it is sent as totalSteps when the run is FINISHed.
type Run struct {
	AgentID   string
	RunID     string
	StartedAt time.Time
	Requests  int

	inflight  int  // leases outstanding; guarded by the manager mutex
	finishing bool // FINISH in flight; guarded by the manager mutex
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
	runs          map[string]*Run // agentID → current run
	draining      []*Run          // rotated runs awaiting FINISH
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
	// totalRequests is the cumulative count of Acquire leases handed out.
	// It is kept separate from the per-run counters because rotated runs
	// that get FINISHed leave the active+draining sets and would otherwise
	// take their request counts out of Snapshot.
	totalRequests int
}

// NewRunManager builds the manager for one token. rotationInterval is how
// long a run lives before it is rotated (config ROTATION_INTERVAL, default
// 6h). The session manager is used only for Shutdown's EndSession.
func NewRunManager(client *upstream.Client, session *session.Manager, rotationInterval time.Duration) *RunManager {
	return &RunManager{
		client:           client,
		session:          session,
		rotationInterval: rotationInterval,
		runs:             make(map[string]*Run),
	}
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
// Draining finishes happen on the maintain tick or the next rotation.
func (m *RunManager) Release(run *Run) {
	if run == nil {
		return
	}
	m.mu.Lock()
	if run.inflight > 0 {
		run.inflight--
	}
	m.mu.Unlock()
}

// InflightCount returns the number of outstanding leases across all runs
// (active and draining). The pool uses it to skip evicting bridge entries
// whose run is still serving a request: FINISHing such a run would kill the
// in-flight chat, so those entries are left for the idle sweep instead.
func (m *RunManager) InflightCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, run := range m.runs {
		n += run.inflight
	}
	for _, run := range m.draining {
		n += run.inflight
	}
	return n
}

// FinishRun FINISHes the run upstream with the given step accounting and
// drops it from the active set. On upstream failure the run is put back on
// the draining list so Maintain retries it. It does not touch inflight —
// callers should have already Released the lease.
func (m *RunManager) FinishRun(ctx context.Context, run *Run, totalSteps int) {
	if run == nil {
		return
	}
	m.drop(run)
	if err := m.client.FinishRun(ctx, run.RunID, totalSteps); err != nil {
		// Keep the run around for a Maintain retry; the id is not
		// necessarily dead upstream (network errors, 5xx).
		m.mu.Lock()
		m.draining = append(m.draining, run)
		m.mu.Unlock()
		slog.Debug("runs: FINISH failed, will retry on maintain", "run_id", run.RunID, "err", err)
	}
}

// Maintain rotates aged runs and FINISHes the draining list. Runs with
// outstanding inflight leases or an in-flight FINISH are skipped. Best
// effort: failures are logged, never returned (background job). While the
// token is cooling down (auth rejection, rate limit, ban) the pass returns
// immediately: no rotate attempts, no draining FINISH, no log — retrying
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
	draining := append([]*Run(nil), m.draining...)
	m.mu.Unlock()

	for _, agentID := range toRotate {
		if err := m.rotate(ctx, agentID); err != nil {
			slog.Debug("runs: maintain rotate failed", "agent_id", agentID, "err", err)
		}
	}
	for _, run := range draining {
		m.finishIfReady(run)
	}
}

// Shutdown FINISHes every run (active and draining) and ends the upstream
// session. When ctx carries no deadline a 10s force deadline is applied
// (PRD §5.5 shutdown sequence).
func (m *RunManager) Shutdown(ctx context.Context) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
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
		if err := m.client.FinishRun(ctx, run.RunID, run.Requests); err != nil {
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

// FinishAllRuns FINISHes every active run and drops it from the active set,
// leaving the session untouched (unlike Shutdown). Used by the pool's idle
// rotation: once a token has been idle past IDLE_ROTATION_TIMEOUT, its runs
// are finished so no rotation/refresh activity continues upstream; the next
// Acquire starts a fresh run on demand.
func (m *RunManager) FinishAllRuns(ctx context.Context) {
	m.mu.Lock()
	all := make([]*Run, 0, len(m.runs))
	for _, run := range m.runs {
		all = append(all, run)
	}
	m.runs = make(map[string]*Run)
	m.mu.Unlock()

	var errs []string
	for _, run := range all {
		if err := m.client.FinishRun(ctx, run.RunID, run.Requests); err != nil {
			errs = append(errs, fmt.Sprintf("finish run %s: %v", run.RunID, err))
		}
	}
	if len(errs) > 0 {
		slog.Warn("runs: idle finish with errors", "errors", strings.Join(errs, "; "))
	}
}

// Invalidate drops the current run for agentID so the next Acquire starts a
// fresh one. Used when an upstream chat reports the run id as unknown
// (ErrRunInvalid); the dead run is not FINISHed (upstream already forgot it)
// and not drained.
func (m *RunManager) Invalidate(agentID string) {
	m.mu.Lock()
	delete(m.runs, agentID)
	m.mu.Unlock()
}

// Cooldown puts the token in a cooldown window of duration d (e.g.
// DefaultCooldown after an auth rejection). Durations <= 0 are ignored.
func (m *RunManager) Cooldown(d time.Duration) {
	if d <= 0 {
		return
	}
	m.mu.Lock()
	m.cooldownUntil = time.Now().Add(d)
	m.rateLimit = nil
	m.ban = nil
	m.countryBlock = nil
	m.mu.Unlock()
}

// ClearCooldowns removes any cooldown, rate-limit lock, and ban window so
// the token is immediately acquirable again (dashboard unlock action).
func (m *RunManager) ClearCooldowns() {
	m.mu.Lock()
	m.cooldownUntil = time.Time{}
	m.rateLimit = nil
	m.ban = nil
	m.banUntil = time.Time{}
	m.countryBlock = nil
	m.countryUntil = time.Time{}
	m.mu.Unlock()
}

// CooldownUntil returns the cooldown deadline (zero when not cooling down).
func (m *RunManager) CooldownUntil() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cooldownUntil
}

// CooldownRateLimit applies a rate-limit cooldown and remembers the error
// so subsequent Acquires surface 429 + Retry-After instead of a generic
// 502. Errors with RetryAfter <= 0 are ignored.
func (m *RunManager) CooldownRateLimit(rle *upstream.RateLimitError) {
	if rle == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rle.RetryAfter > 0 {
		m.cooldownUntil = time.Now().Add(rle.RetryAfter)
	} else if !rle.ResetAt.IsZero() && rle.ResetAt.After(time.Now()) {
		m.cooldownUntil = rle.ResetAt
	} else {
		m.cooldownUntil = upstream.NextPacificMidnight()
	}
	m.rateLimit = rle
	m.ban = nil
	m.countryBlock = nil
}

// RateLimitError returns the remembered rate-limit error while its
// cooldown is still active, nil otherwise.
func (m *RunManager) RateLimitError() *upstream.RateLimitError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.cooldownUntil) && m.rateLimit != nil {
		return m.rateLimit
	}
	return nil
}

// CooldownBan applies a ban cooldown and remembers the error so Acquires
// keep surfacing 403 banned + resumes-at until the unban time.
func (m *RunManager) CooldownBan(be *upstream.BanError) {
	if be == nil {
		return
	}
	m.mu.Lock()
	m.ban = be
	if be.ResumesAt.After(time.Now()) {
		m.banUntil = be.ResumesAt
	} else {
		m.banUntil = time.Now().Add(24 * time.Hour) // no timestamp: safe default
	}
	// The ban also fills the shared cooldown deadline so Acquire skips the
	// token entirely during the window (the remembered error is surfaced by
	// the cooldown-skip branch instead of re-hitting upstream).
	m.cooldownUntil = m.banUntil
	m.rateLimit = nil // a ban supersedes any rate-limit cooldown
	m.countryBlock = nil
	m.mu.Unlock()
}

// BanError returns the remembered ban error while the ban window is
// active, nil otherwise.
func (m *RunManager) BanError() *upstream.BanError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.banUntil) && m.ban != nil {
		return m.ban
	}
	return nil
}

// CooldownCountryBlocked applies a country-block cooldown and remembers the
// error so Acquires keep surfacing the region-block instead of re-hitting
// upstream during the window (mirrors CooldownRateLimit/CooldownBan).
func (m *RunManager) CooldownCountryBlocked(cbe *upstream.CountryBlockedError) {
	if cbe == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// A ban outranks a country block (pool precedence ban > country): keep
	// the ban window and its remembered error instead of downgrading to the
	// shorter country cooldown.
	if time.Now().Before(m.banUntil) && m.ban != nil {
		return
	}
	m.countryBlock = cbe
	m.countryUntil = time.Now().Add(countryBlockCooldown)
	// The block also fills the shared cooldown deadline so Acquire skips
	// the token entirely during the window (the remembered error is
	// surfaced by the cooldown-skip branch instead of re-hitting upstream).
	m.cooldownUntil = m.countryUntil
	m.rateLimit = nil
	m.ban = nil
	m.banUntil = time.Time{}
}

// CountryBlockedError returns the remembered country-block error while its
// cooldown window is active, nil otherwise.
func (m *RunManager) CountryBlockedError() *upstream.CountryBlockedError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.countryUntil) && m.countryBlock != nil {
		return m.countryBlock
	}
	return nil
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

// --- internals ---

// rotate starts a fresh run for agentID, pushing the previous current run
// (if any) onto the draining list and finishing it asynchronously. The
// double checks under the lock make concurrent rotators converge on one
// START when possible.
func (m *RunManager) rotate(ctx context.Context, agentID string) error {
	m.mu.Lock()
	if now := time.Now(); now.Before(m.cooldownUntil) {
		until := m.cooldownUntil
		m.mu.Unlock()
		return fmt.Errorf("token cooling down until %s", until.Format(time.RFC3339))
	}
	if run := m.runs[agentID]; run != nil && time.Since(run.StartedAt) < m.rotationInterval {
		m.mu.Unlock()
		return nil // a concurrent rotator already refreshed it
	}
	m.mu.Unlock()

	runID, err := m.client.StartRun(ctx, agentID)
	if err != nil {
		return err
	}
	slog.Debug("runs: run started", "agent_id", agentID, "run_id", runID)

	m.mu.Lock()
	oldRun := m.runs[agentID]
	m.runs[agentID] = &Run{AgentID: agentID, RunID: runID, StartedAt: time.Now()}
	if oldRun != nil {
		m.draining = append(m.draining, oldRun)
	}
	m.mu.Unlock()

	if oldRun != nil {
		go m.finishIfReady(oldRun)
	}
	return nil
}

// finishIfReady FINISHes a rotated run once it has no outstanding leases and
// is no longer the current run for its agent. Concurrent callers are
// serialized by the finishing flag.
func (m *RunManager) finishIfReady(run *Run) {
	m.mu.Lock()
	if run == nil || run.inflight > 0 || run.finishing {
		m.mu.Unlock()
		return
	}
	if current, ok := m.runs[run.AgentID]; ok && current == run {
		m.mu.Unlock()
		return
	}
	run.finishing = true
	m.mu.Unlock()

	// Client-side call timeout bounds this (sessionCallTimeout); background
	// context is fine for a drain goroutine.
	if err := m.client.FinishRun(context.Background(), run.RunID, run.Requests); err != nil {
		m.mu.Lock()
		run.finishing = false
		m.mu.Unlock()
		slog.Warn("runs: finish draining run failed", "run_id", run.RunID, "requests", run.Requests, "err", err)
		return
	}

	m.mu.Lock()
	filtered := m.draining[:0]
	for _, d := range m.draining {
		if d != run {
			filtered = append(filtered, d)
		}
	}
	m.draining = filtered
	m.mu.Unlock()
	slog.Debug("runs: run finished", "run_id", run.RunID, "requests", run.Requests)
}

// drop removes run from the active set (if it is still current) and the
// draining list.
func (m *RunManager) drop(run *Run) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.runs[run.AgentID]; ok && current == run {
		delete(m.runs, run.AgentID)
	}
	filtered := m.draining[:0]
	for _, d := range m.draining {
		if d != run {
			filtered = append(filtered, d)
		}
	}
	m.draining = filtered
}
