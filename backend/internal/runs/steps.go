package runs

// Run/step recording for a leased agent run (issue #114): the Run struct
// with its per-chat step counter and in-memory step list, the terminal
// status recorders (MarkFailed, ReleaseAbandoned), and the helpers that
// snapshot or persist a run (cloneRun, the session-state store write/remove).
// Step recording is local-only — steps are batched and sent WITH FINISH, so
// nothing here touches the network.

import (
	cryptoRand "crypto/rand"
	"fmt"
	"sync/atomic"
	"time"

	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
)

// newTraceSessionID mints a UUIDv4 trace session id from crypto/rand,
// mirroring the CLI's randomUUID per run (run.ts: previousRun?.traceSessionId
// ?? randomUUID). A crypto/rand failure is unrecoverable in practice; fall
// back to a time-seeded hex id rather than panicking mid-run.
func newTraceSessionID() string {
	var b [16]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Run is one agent run leased to a caller. Requests counts acquires served
// by this run; it is used as the totalSteps fallback when no steps were
// recorded (issue #114 — a step is one chat call, recorded in memory and
// batched with FINISH).
type Run struct {
	AgentID   string
	RunID     string
	StartedAt time.Time
	Requests  int
	// TraceSessionID is minted once per run (crypto/rand UUID) and reused
	// across the run's requests as codebuff_metadata["trace_session_id"],
	// exactly like the CLI (run.ts: previousRun?.traceSessionId ??
	// randomUUID; reference/proxy-freebuff lib/runs.js:43-46).
	TraceSessionID string

	// ClientID is codebuff_metadata["client_id"], minted once per run and
	// repeated by every chat call of that run. The CLI mints it once per
	// prompt (run.ts:722 `promptId`, handed to the agent loop as
	// `clientSessionId` at run.ts:822 and reused by every LLM step), so a
	// FRESH id per chat call made one run_id fan out across N client ids —
	// the shape upstream refuses as free_mode_run_fanout. Persisted with the
	// run so a restart resumes the id instead of splitting the run.
	ClientID string

	// StepCount is the run's 1-based per-chat-call step counter, stamped
	// as codebuff_metadata["llm_step_number"] (issue #113, CLI parity:
	// run-agent-step.ts increments per step). Atomic: chatAttempt
	// increments it while the manager goroutines may rotate/finish the
	// run, so the server reaches it without the manager mutex.
	StepCount atomic.Int64

	// Steps accumulates the run's completed chat steps in memory (issue
	// #114): they are batched and sent WITH FINISH — the CLI has no /steps
	// endpoint, so step recording is local-only. Guarded by the manager
	// mutex; snapshot under the lock at FINISH time. Bounded to the newest
	// maxRecordedSteps (the FINISH payload must not grow without bound).
	Steps []upstream.RunStep

	// stepTotal is the monotonic count of recorded steps (Steps may have
	// dropped the oldest beyond maxRecordedSteps; FINISH's totalSteps stays
	// honest). Guarded by the manager mutex.
	stepTotal int

	// Status is the run's terminal disposition reported in FINISH
	// (completed/cancelled/failed, CLI parity run-agent-step.ts
	// 1237-1341). Empty means completed. Set by ReleaseAbandoned
	// (cancelled) and MarkFailed (failed); guarded by the manager mutex.
	Status string

	inflight  int  // leases outstanding; guarded by the manager mutex
	finishing bool // FINISH in flight; guarded by the manager mutex
	// queued marks a deferred FINISH job already in the bounded queue
	// (issue #90): rotate and Maintain both enqueue draining runs, and the
	// dedupe prevents a failed attempt from being FINISHed twice upstream.
	// Guarded by the manager mutex.
	queued bool
	// drainedAt is when the run was pushed onto the draining list (issue
	// #55): the TTL eviction drops entries stuck draining past DrainTTL.
	// Guarded by the manager mutex.
	drainedAt time.Time
}

// NextStepNumber increments the run's 1-based per-chat step counter and
// returns the new value. The server calls it once per chat call to stamp
// codebuff_metadata["llm_step_number"] (issue #113).
func (r *Run) NextStepNumber() int64 {
	return r.StepCount.Add(1)
}

// maxRecordedSteps bounds a run's in-memory step list and FINISH payload:
// a busy 6h run can otherwise accumulate thousands of steps (each with a
// fresh UUID + timestamps) and emit a multi-MB FINISH. The oldest steps are
// dropped; the monotonic stepTotal keeps FINISH's totalSteps honest.
const maxRecordedSteps = 512

// RecordStep appends a completed-chat step to run's in-memory step list
// (issue #114): steps are batched and sent WITH FINISH — the CLI has no
// /steps endpoint, so recording is local-only and never touches the
// network. The server fires it after a successful chat; messageID is the
// completed chat response id ("" â†’ null on the wire, allowed by the CLI
// step schema). Step numbers come from the run's per-attempt counter — the
// SAME counter stamped as llm_step_number on the wire — so FINISH's steps
// agree with the stamps already sent even when earlier attempts failed
// (#113/#114; the CLI numbers both from one per-run counter).
func (m *RunManager) RecordStep(run *Run, messageID string) {
	if run == nil || run.RunID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var msgID *string
	if messageID != "" {
		msgID = &messageID
	}
	run.stepTotal++
	stepNumber := run.StepCount.Load()
	if stepNumber == 0 {
		// No attempt ever stamped llm_step_number (recording fired without
		// a server chat attempt, e.g. pool-level direct use): fall back to
		// sequential numbering in completion order.
		stepNumber = int64(len(run.Steps) + 1)
	}
	run.Steps = append(run.Steps, upstream.RunStep{
		ID:         newTraceSessionID(),
		StepNumber: int(stepNumber),
		MessageID:  msgID,
		Status:     "completed",
		StartTime:  time.Now().UTC().Format(time.RFC3339Nano),
	})
	if len(run.Steps) > maxRecordedSteps {
		run.Steps = run.Steps[len(run.Steps)-maxRecordedSteps:]
	}
}

// MarkFailed records that run's chat died on a terminal upstream error so
// its eventual FINISH reports status "failed" instead of "completed"
// (issue #114: a gateway with zero failed runs looks synthetic). The server
// calls it from the chat error path; the run stays active — only its
// terminal status is recorded.
func (m *RunManager) MarkFailed(run *Run) {
	if run == nil {
		return
	}
	m.mu.Lock()
	run.Status = "failed"
	m.mu.Unlock()
}

// cloneRun copies run's snapshot-able fields without copying the atomic
// step counter by value (atomic.Int64 must never be copied after first
// use). Used by Shutdown's persistence snapshot.
func (m *RunManager) cloneRun(run *Run) *Run {
	c := &Run{
		AgentID:        run.AgentID,
		RunID:          run.RunID,
		StartedAt:      run.StartedAt,
		Requests:       run.Requests,
		TraceSessionID: run.TraceSessionID,
		ClientID:       run.ClientID,
		Status:         run.Status,
		Steps:          append([]upstream.RunStep(nil), run.Steps...),
		stepTotal:      run.stepTotal,
	}
	c.StepCount.Store(run.StepCount.Load())
	return c
}

// persistRun writes the run into the session-state store (issue #40) so a
// restart can resume it without re-START. Best-effort; the store write
// never fails the caller.
func (m *RunManager) persistRun(run *Run) {
	if m.store == nil || m.key == "" || run == nil {
		return
	}
	m.mu.Lock()
	requests := run.Requests
	startedAt := run.StartedAt
	m.mu.Unlock()
	m.store.SaveRun(m.key, run.AgentID, session.PersistedRun{
		RunID:          run.RunID,
		AgentID:        run.AgentID,
		TraceSessionID: run.TraceSessionID,
		ClientID:       run.ClientID,
		StartedAt:      startedAt,
		Requests:       requests,
	})
}

// removeRun drops the run from the session-state store (issue #40): the
// run was FINISHed upstream, so a restart must not resurrect it.
func (m *RunManager) removeRun(run *Run) {
	if m.store == nil || m.key == "" || run == nil {
		return
	}
	m.store.RemoveRun(m.key, run.AgentID)
}

// ReleaseAbandoned releases run after the downstream client's context was
// cancelled mid-chat (issue #53, CLI DELETE-on-exit parity): when this was
// the LAST in-flight request on the run, the run is dropped from the active
// set and FINISHed through the bounded queue so upstream does not keep an
// abandoned agent run alive until rotation. Concurrent requests on the same
// run keep it alive (inflight stays > 0). The decrement and the finish
// decision happen under the manager mutex, so a racing Acquire can never
// lease a run that is about to be finished. The abandoned run FINISHes as
// "cancelled" (issue #114): a run killed by a client disconnect must not
// report completed — a gateway with zero cancelled runs looks synthetic.
func (m *RunManager) ReleaseAbandoned(run *Run) {
	if run == nil {
		return
	}
	m.mu.Lock()
	if run.inflight > 0 {
		run.inflight--
	}
	if run.inflight > 0 {
		// Other requests are still in flight on this run: keep it alive.
		m.mu.Unlock()
		return
	}
	// Last lease abandoned: the run must FINISH as cancelled, whichever
	// finish path owns it (active drop or the draining queue). A run that
	// already died on a terminal upstream error keeps its "failed" status
	// (issue #114) — cancelled only fills an unset status.
	if run.Status == "" {
		run.Status = "cancelled"
	}
	// If it is still the current run, drop it from the active set so no
	// new acquire reuses it, then FINISH it. Join the draining list BEFORE
	// enqueueing (mirrors rotate): if the FINISH fails transiently,
	// Maintain re-drains it — without draining membership the run would be
	// in no set and its cancelled FINISH would be lost forever, leaking
	// the upstream agent run. A run that already rotated away is owned by
	// the draining queue.
	if current, ok := m.runs[run.AgentID]; ok && current == run {
		delete(m.runs, run.AgentID)
		m.appendDrainingLocked(run)
		m.mu.Unlock()
		m.enqueueFinish(run)
		return
	}
	m.mu.Unlock()
	// Rotated already (or drained by FinishAllRuns): re-queue the FINISH
	// the draining queue skipped while inflight > 0 — Maintain is the only
	// other re-enqueuer, and after a drain there may be no next tick.
	// enqueueFinish dedupes against a job already in the queue.
	m.enqueueFinish(run)
}

// resumedClientID keeps a resumed run's persisted client id, minting a fresh
// one only for a run persisted before the field existed. Returning "" instead
// would send no client_id at all, which is a shape the CLI never produces.
func resumedClientID(persisted string) string {
	if persisted != "" {
		return persisted
	}
	return upstream.NewClientID()
}
