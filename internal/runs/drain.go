package runs

// FINISH machinery (issues #90/#55): the bounded deferred-job queue
// (enqueue/startFinishWorker/finishLoop), the synchronous finish entry
// points (FinishAllRuns, FinishRun), the draining-list pruner, and
// finishIfReadyCtx — the shared "FINISH a run when it is not leased and not
// already finishing" path that both the queue worker and the inline
// fallback use.

import (
	"context"
	"log/slog"
	"time"

	"freebuff-proxy/internal/upstream"
)

// asyncJobKind discriminates the deferred-side-effect jobs carried by the
// bounded finish queue (issue #90): FINISH a rotated/drained run. Chat-step
// recording used to be a second kind (#91) but is now a synchronous
// in-memory append (issue #114: steps are batched and sent WITH FINISH —
// the CLI has no /steps endpoint). The context-pruner child-run job (#91)
// was removed with G4: the newest CLI UNTRACKS pruner runs entirely
// (reference/freebuff packages/agent-runtime/src/run-agent-step.ts:785-795
// mints a local untracked run id, zero ledger POSTs).
type asyncJobKind uint8

const jobFinish asyncJobKind = iota

// asyncJob is one unit of deferred upstream work. jobs are processed by a
// single background worker per RunManager; when the bounded queue is full
// the caller runs the job inline bounded by the inline timeout.
type asyncJob struct {
	kind asyncJobKind
	run  *Run
}

// FinishAllRuns finishes all idle active and draining runs synchronously.
// Runs with an outstanding lease are NOT dropped: they are marked draining
// (drainedAt) and kept in the manager so the LAST lease release re-queues
// their FINISH. Previously every run was deleted from both sets up front,
// so a run with inflight > 0 was skipped by finishIfReadyCtx AND became
// unreachable — Release only decremented, ReleaseAbandoned only re-queued
// runs still current in m.runs, and Maintain iterates the current sets —
// so its upstream FINISH never ran (P1, anti-ban contract): the terminal
// status was lost and the run leaked upstream until its own rotation
// expiry.
func (m *RunManager) FinishAllRuns(ctx context.Context) {
	m.mu.Lock()
	runsToFinish := make([]*Run, 0, len(m.runs)+len(m.draining))
	for agentID, run := range m.runs {
		if run.inflight > 0 {
			continue
		}
		delete(m.runs, agentID)
		runsToFinish = append(runsToFinish, run)
	}
	// Draining runs: finish the idle ones; busy ones stay draining (the
	// lease release re-queues them once inflight drains).
	kept := m.draining[:0]
	for _, run := range m.draining {
		if run.inflight > 0 {
			kept = append(kept, run)
			continue
		}
		runsToFinish = append(runsToFinish, run)
	}
	m.draining = kept
	// In-flight active runs stay in the manager so the last lease release
	// re-queues the FINISH; mark them draining so the release path (and
	// Maintain) sees them, with pruneDrainingLocked still bounding the
	// list.
	for _, run := range m.runs {
		if run.inflight > 0 {
			m.appendDrainingLocked(run)
		}
	}
	m.pruneDrainingLocked()
	m.mu.Unlock()
	for _, r := range runsToFinish {
		m.finishIfReadyCtx(ctx, r)
	}
}

// FinishRun FINISHes the run upstream with its recorded terminal status,
// completed steps, and totalSteps (issue #114), then drops it from the
// active set. On upstream failure the run is put back on the draining list
// so Maintain retries it. It does not touch inflight â€” callers should have
// already Released the lease.
func (m *RunManager) FinishRun(ctx context.Context, run *Run) {
	if run == nil {
		return
	}
	m.drop(run)
	status, steps, totalSteps := m.finishPayload(run)
	if err := m.client.FinishRun(ctx, run.RunID, status, totalSteps, steps, ""); err != nil {
		// Keep the run around for a Maintain retry; the id is not
		// necessarily dead upstream (network errors, 5xx).
		m.mu.Lock()
		m.appendDrainingLocked(run)
		m.mu.Unlock()
		slog.Debug("runs: FINISH failed, will retry on maintain", "run_id", run.RunID, "err", err)
		return
	}
	// FINISHed cleanly: a restart must not resurrect the run.
	m.removeRun(run)
}

// enqueueFinish submits a deferred FINISH for run through the bounded queue
// (issue #90). Runs already queued are skipped (rotate and Maintain both
// enqueue draining runs; without the dedupe a failed attempt would be
// FINISHed twice upstream once the finishing flag resets).
func (m *RunManager) enqueueFinish(run *Run) {
	if run == nil {
		return
	}
	m.mu.Lock()
	if run.queued {
		m.mu.Unlock()
		return
	}
	run.queued = true
	m.mu.Unlock()
	m.enqueue(asyncJob{kind: jobFinish, run: run})
}

// enqueue submits a deferred upstream job to the bounded finish queue
// (issue #90). When the queue is full the job runs inline, bounded by the
// inline finish timeout — the caller never blocks on the worker.
func (m *RunManager) enqueue(job asyncJob) {
	if job.run == nil {
		return
	}
	if m.finishQueue == nil {
		return
	}
	m.startFinishWorker()
	select {
	case m.finishQueue <- job:
	default:
		// Queue full: synchronous inline fallback bounded by the short
		// inline deadline (mirrors the reference async finalizer's
		// finalizeInlineTimeout). A run whose FINISH exceeds the deadline is
		// left draining for the next Maintain retry.
		ctx, cancel := context.WithTimeout(context.Background(), m.inlineFinishTimeout)
		defer cancel()
		m.runJob(ctx, job)
	}
}

// startFinishWorker launches the single deferred-job worker on first use.
// The worker exits when Shutdown closes finishStop after draining the
// queue, so no goroutine outlives the manager.
func (m *RunManager) startFinishWorker() {
	m.finishStartOnce.Do(func() {
		m.finishWg.Add(1)
		go m.finishLoop()
	})
}

// finishLoop is the deferred-job worker: FINISH rotated/drained runs,
// best-effort.
func (m *RunManager) finishLoop() {
	defer m.finishWg.Done()
	defer close(m.finishExited)
	for {
		select {
		case <-m.finishStop:
			// Shutdown: drain whatever is queued (bounded by the shutdown
			// deadline â€” a saturated queue must not stall the process),
			// then exit. Jobs run on a background ctx so they COMPLETE;
			// the deadline only abandons the wait (review P2).
			for {
				select {
				case <-m.finishDrainCtx.Done():
					return
				case job := <-m.finishQueue:
					m.runJob(context.Background(), job)
				default:
					return
				}
			}
		case job := <-m.finishQueue:
			m.runJob(context.Background(), job)
		}
	}
}

// runJob executes one deferred job (FINISH). Best-effort: failures are
// logged, never surfaced to a caller.
func (m *RunManager) runJob(ctx context.Context, job asyncJob) {
	switch job.kind {
	case jobFinish:
		m.finishIfReadyCtx(ctx, job.run)
	}
}

// finishPayload snapshots the run's recorded terminal status, steps, and
// totalSteps for its FINISH (issue #114): status is honest
// (completed/cancelled/failed), steps are batched and sent WITH FINISH, and
// totalSteps prefers the recorded step count, falling back to the request
// count when no steps were recorded (a prewarmed run that served no
// successful chats still reports its activity). Caller does not hold m.mu.
func (m *RunManager) finishPayload(run *Run) (status string, steps []upstream.RunStep, totalSteps int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status = run.Status
	if status == "" {
		status = "completed"
	}
	steps = append([]upstream.RunStep(nil), run.Steps...)
	totalSteps = run.stepTotal
	if totalSteps == 0 {
		totalSteps = len(steps)
	}
	if totalSteps == 0 {
		totalSteps = run.Requests
	}
	return status, steps, totalSteps
}

// logRunFinished emits a run's terminal lifecycle record (W3-A): every way
// a run leaves the manager â€” FINISHed through the deferred queue or
// force-dropped from the draining list without FINISH â€” logs the same
// run-finished event with the run's lifetime (duration_ms, now-StartedAt),
// its in-memory recorded step count (steps), and the termination path
// ("finish" via the FINISH queue, "drop" without FINISH). steps is the
// run's recorded-step snapshot taken under m.mu by the caller: the queue
// path reuses finishPayload's copy, the drop path reads run.Steps while
// holding the manager mutex. Takes no lock itself.
func logRunFinished(run *Run, steps int, termination string) {
	slog.Debug("runs: run finished",
		"run_id", run.RunID,
		"requests", run.Requests,
		"trace_session_id", run.TraceSessionID,
		"duration_ms", int(time.Since(run.StartedAt).Milliseconds()),
		"steps", steps,
		"termination", termination)
}

// appendDrainingLocked adds run to the draining list unless it is already
// present, stamping drainedAt (the TTL start, issue #55) and bounding the
// list. Caller holds m.mu. Shared by rotate, ReleaseAbandoned,
// FinishAllRuns, and FinishRun's failure path so a run is never tracked
// twice — a duplicate draining entry would be FINISHed twice upstream once
// the finishing flag resets.
func (m *RunManager) appendDrainingLocked(run *Run) {
	if run == nil {
		return
	}
	for _, d := range m.draining {
		if d == run {
			return
		}
	}
	run.drainedAt = time.Now()
	m.draining = append(m.draining, run)
	m.pruneDrainingLocked()
}

// runDrainingLocked reports whether run is on the draining list. Caller
// holds m.mu. Release consults it to re-queue a deferred FINISH when the
// last lease of a draining run releases (P1).
func (m *RunManager) runDrainingLocked(run *Run) bool {
	for _, d := range m.draining {
		if d == run {
			return true
		}
	}
	return false
}

// pruneDrainingLocked bounds the draining list (issue #55): entries stuck
// past DrainTTL or beyond DrainQueueCap are force-dropped with a warn log â€”
// their upstream FINISH is best-effort anyway, and the list must never grow
// unbounded when FINISH keeps failing. Caller holds m.mu.
func (m *RunManager) pruneDrainingLocked() {
	now := time.Now()
	kept := m.draining[:0]
	for _, run := range m.draining {
		if !run.drainedAt.IsZero() && now.Sub(run.drainedAt) > m.drainTTL {
			slog.Warn("runs: dropping draining run (TTL expired)", "run_id", run.RunID, "agent_id", run.AgentID, "age", now.Sub(run.drainedAt).Round(time.Second))
			logRunFinished(run, len(run.Steps), "drop")
			continue
		}
		kept = append(kept, run)
	}
	m.draining = kept
	if len(m.draining) > m.drainQueueCap {
		overflow := m.draining[m.drainQueueCap:]
		m.draining = append([]*Run(nil), m.draining[:m.drainQueueCap]...)
		for _, run := range overflow {
			slog.Warn("runs: dropping draining run (queue cap)", "run_id", run.RunID, "agent_id", run.AgentID)
			logRunFinished(run, len(run.Steps), "drop")
		}
	}
}

// finishIfReadyCtx is finishIfReady with an explicit context: the deferred
// queue worker uses a background context (the client-side session-call
// timeout bounds it), while the inline fallback passes its short deadline
// so a saturated queue cannot stall the caller.
func (m *RunManager) finishIfReadyCtx(ctx context.Context, run *Run) {
	m.mu.Lock()
	// The worker picked up the job: clear the queued marker so a later
	// Maintain pass may retry a run that was not finishable right now.
	if run != nil {
		run.queued = false
	}
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

	status, steps, totalSteps := m.finishPayload(run)
	if err := m.client.FinishRun(ctx, run.RunID, status, totalSteps, steps, ""); err != nil {
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
	m.removeRun(run)
	logRunFinished(run, len(steps), "finish")
}

// finishInline best-effort FINISHes a run that was never tracked by the
// manager (P3): rotate discards a fresh run whose upstream START completed
// after Shutdown began. The finish worker is stopped by then, so the
// FINISH runs here, bounded by the shutdown deadline — never the caller's
// request ctx, which may already be cancelled. The payload defaults mirror
// a zero-request run (status completed, no steps). No manager state is
// touched: the run was never inserted, and it must not disturb a persisted
// entry for the same agent.
func (m *RunManager) finishInline(runID, agentID string) {
	fctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := m.client.FinishRun(fctx, runID, "completed", 0, nil, ""); err != nil {
		slog.Warn("runs: inline FINISH of run started during shutdown failed", "run_id", runID, "agent_id", agentID, "err", err)
		return
	}
	slog.Debug("runs: run started during shutdown FINISHed inline", "run_id", runID, "agent_id", agentID)
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
