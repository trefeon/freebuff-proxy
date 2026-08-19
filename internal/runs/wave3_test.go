package runs

// Wave-3 tests: the bounded deferred-FINISH queue (#90), the draining-list
// cap/TTL eviction (#55), abandoned-lease finish on client disconnect
// (#53/#114), context-pruner child runs (#91) + step recording (#114: steps
// are batched and sent WITH FINISH), and run persistence across restarts
// (#40).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// newTestManagerOpts is newTestManager with full Options (queue bounds etc.).
func newTestManagerOpts(t *testing.T, mock *testutil.MockUpstream, opts Options) (*RunManager, *session.Manager) {
	t.Helper()
	client, sess := newTestClient(t, mock)
	if opts.RotationInterval == 0 {
		opts.RotationInterval = time.Hour
	}
	return NewRunManagerOpts(client, sess, opts), sess
}

// newTestClient wires the mock upstream through a real client and session
// manager (shared by newTestManager and newTestManagerOpts).
func newTestClient(t *testing.T, mock *testutil.MockUpstream) (*upstream.Client, *session.Manager) {
	t.Helper()
	client, err := upstream.New("tok", &config.Config{
		UpstreamBaseURL:    mock.URL(),
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RotationInterval:   6 * time.Hour,
		RegistryRefresh:    6 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, session.NewManager(client)
}

func TestFinishWorkerStopsOnShutdown(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, 40*time.Millisecond)

	// Rotate once so the worker starts and processes a FINISH.
	first, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(first)
	time.Sleep(80 * time.Millisecond)
	second, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(second)
	eventually(t, "rotated FINISH", func() bool {
		_, ok := finishedRun(mock, "run-0001")
		return ok
	})

	// Shutdown must stop the worker: the finishWg.Wait inside Shutdown
	// blocks forever if the worker leaked, so a bounded ctx plus the
	// finishExited hook pins the exit.
	mgr.Shutdown(context.Background())
	select {
	case <-mgr.finishExited:
	case <-time.After(2 * time.Second):
		t.Fatal("finish worker did not exit after Shutdown")
	}
}

func TestFinishQueueBoundsInlineFallback(t *testing.T) {
	// FinishQueueSize=1, a slow FINISH on the worker, and a short inline
	// timeout: the fourth rotated run must hit the inline fallback (its
	// FINISH aborts at the deadline and the run stays draining) while the
	// worker keeps processing the queued ones.
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SetFinishDelay(300 * time.Millisecond)
	mock.RunIDs = []string{"run-0001", "run-0002", "run-0003", "run-0004", "run-0005"}
	mgr, _ := newTestManagerOpts(t, mock, Options{
		RotationInterval:    5 * time.Millisecond,
		FinishQueueSize:     1,
		InlineFinishTimeout: 50 * time.Millisecond,
	})

	acquire := func() *Run {
		t.Helper()
		r, err := mgr.Acquire(context.Background(), agentA)
		if err != nil {
			t.Fatal(err)
		}
		mgr.Release(r) // release immediately: FINISHes may proceed anytime
		return r
	}
	r1 := acquire()
	time.Sleep(15 * time.Millisecond) // let r1 age past the rotation interval
	r2 := acquire()                   // rotates r1 → worker FINISHes r1 (slow)
	time.Sleep(5 * time.Millisecond)
	r3 := acquire() // rotates r2 → queued (queue cap 1, worker busy)
	time.Sleep(5 * time.Millisecond)
	r4 := acquire() // rotates r3 → queue full → inline fallback (aborts)

	// r3's inline FINISH was aborted by the 50ms deadline: it must still be
	// draining (the retry semantics keep it for a Maintain pass).
	mgr.mu.Lock()
	inDraining3 := false
	for _, d := range mgr.draining {
		if d.RunID == r3.RunID {
			inDraining3 = true
		}
	}
	mgr.mu.Unlock()
	if !inDraining3 {
		t.Errorf("r3 (%s) not draining after aborted inline FINISH", r3.RunID)
	}

	// The worker eventually finishes r1 (the queued job) at its own pace;
	// r2's FINISH raced into the inline fallback and was aborted, so it
	// stays draining for a Maintain retry.
	eventually(t, "r1 FINISHed by worker", func() bool {
		_, ok := finishedRun(mock, "run-0001")
		return ok
	})
	mgr.mu.Lock()
	inDraining2 := false
	for _, d := range mgr.draining {
		if d.RunID == r2.RunID {
			inDraining2 = true
		}
	}
	mgr.mu.Unlock()
	if !inDraining2 {
		t.Error("r2 expected draining (inline FINISH aborted under the slow worker)")
	}
	_ = r1
	_ = r2
	_ = r4
}

func TestDrainQueueCapEviction(t *testing.T) {
	// DrainQueueCap=2 with a slow FINISH: rotations pile up in the draining
	// list, and the cap must force-drop the oldest entries.
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SetFinishDelay(500 * time.Millisecond)
	mock.RunIDs = []string{"run-0001", "run-0002", "run-0003", "run-0004", "run-0005", "run-0006", "run-0007"}
	mgr, _ := newTestManagerOpts(t, mock, Options{
		RotationInterval:    5 * time.Millisecond,
		FinishQueueSize:     1,
		InlineFinishTimeout: 5 * time.Millisecond,
		DrainQueueCap:       2,
	})

	acquire := func() {
		t.Helper()
		r, err := mgr.Acquire(context.Background(), agentA)
		if err != nil {
			t.Fatal(err)
		}
		mgr.Release(r)
	}
	for i := 0; i < 6; i++ {
		acquire()
		time.Sleep(8 * time.Millisecond)
	}

	mgr.mu.Lock()
	n := len(mgr.draining)
	mgr.mu.Unlock()
	if n > 2 {
		t.Errorf("draining list length = %d, want <= DrainQueueCap 2", n)
	}
}

func TestDrainTTLEviction(t *testing.T) {
	// DrainTTL=50ms with a failing FINISH: the run stays draining past the
	// TTL and the next draining-list pass force-drops it.
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SetFinishFailures(1) // first FINISH fails → run stays draining
	mgr, _ := newTestManagerOpts(t, mock, Options{
		RotationInterval: 5 * time.Millisecond,
		DrainTTL:         50 * time.Millisecond,
	})

	r1, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(r1)
	time.Sleep(15 * time.Millisecond)
	r2, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(r2)
	// r1's FINISH fails (FinishFailures=1) → r1 stays draining.

	mgr.mu.Lock()
	stillDraining := false
	for _, d := range mgr.draining {
		if d.RunID == r1.RunID {
			stillDraining = true
		}
	}
	mgr.mu.Unlock()
	if !stillDraining {
		t.Fatalf("r1 not draining after failed FINISH; cannot test TTL")
	}

	time.Sleep(80 * time.Millisecond) // age past DrainTTL
	mgr.Maintain(context.Background())

	mgr.mu.Lock()
	after := len(mgr.draining)
	for _, d := range mgr.draining {
		if d.RunID == r1.RunID {
			t.Error("r1 still draining after TTL eviction")
		}
	}
	mgr.mu.Unlock()
	_ = after
}

func TestReleaseAbandonedFinishesLastLease(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	// Abandon the only lease: the run must be dropped from active and
	// FINISHed (upstream must not keep an abandoned run alive).
	mgr.ReleaseAbandoned(run)

	// Issue #114: an abandoned run must FINISH as cancelled, not completed —
	// a gateway with zero cancelled runs looks synthetic.
	eventually(t, "abandoned run FINISH", func() bool {
		f, ok := finishedRun(mock, run.RunID)
		return ok && f.Status == "cancelled"
	})
	mgr.mu.Lock()
	_, active := mgr.runs[agentA]
	mgr.mu.Unlock()
	if active {
		t.Error("abandoned run still in active set")
	}
	// The next acquire must START a fresh run.
	next, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if next.RunID == run.RunID {
		t.Error("acquire reused the abandoned run")
	}
	mgr.Release(next)
	mgr.Shutdown(context.Background())
}

func TestReleaseAbandonedKeepsConcurrentRequests(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	r1, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 {
		t.Fatalf("concurrent acquires on different runs (%s vs %s)", r1.RunID, r2.RunID)
	}

	// Abandoning one of two in-flight requests must keep the run alive.
	mgr.ReleaseAbandoned(r1)
	select {
	case <-time.After(150 * time.Millisecond):
	case <-mockFinishDone(mock, r1.RunID):
		t.Fatal("run FINISHed while a concurrent request was in flight")
	}
	mgr.mu.Lock()
	_, active := mgr.runs[agentA]
	mgr.mu.Unlock()
	if !active {
		t.Error("run dropped from active while a concurrent request holds it")
	}

	// The second request completes normally: still no FINISH (normal
	// release never finishes a run).
	mgr.Release(r2)
	select {
	case <-time.After(100 * time.Millisecond):
	case <-mockFinishDone(mock, r1.RunID):
		t.Fatal("run FINISHed after a normal release")
	}
	mgr.Shutdown(context.Background())
}

func TestRecordStepBatchesWithFinish(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	// Issue #114: steps are recorded in memory and batched with FINISH —
	// recording must never hit the network (the CLI has no /steps
	// endpoint), so the mock sees them only inside the FINISH payload.
	mgr.RecordStep(run, "chatcmpl-123")
	mgr.RecordStep(run, "chatcmpl-124")
	mgr.FinishRun(context.Background(), run)

	eventually(t, "steps ride in FINISH", func() bool {
		f, ok := finishedRun(mock, run.RunID)
		if !ok || f.Status != "completed" || f.TotalSteps != 2 || len(f.Steps) != 2 {
			return false
		}
		return f.Steps[0].StepNumber == 1 && f.Steps[0].MessageID != nil && *f.Steps[0].MessageID == "chatcmpl-123" &&
			f.Steps[1].StepNumber == 2 && f.Steps[1].MessageID != nil && *f.Steps[1].MessageID == "chatcmpl-124" &&
			f.Steps[1].Status == "completed" && f.Steps[0].StartTime != ""
	})
	mgr.Shutdown(context.Background())
}

func TestChildRunCreatedAfterStart(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}

	// The context-pruner child must be STARTed with ancestorRunIds=[parent]
	// and then FINISHed (best-effort, through the queue).
	eventually(t, "context-pruner child started", func() bool {
		for _, parent := range mock.ChildRunsStartedSnapshot() {
			if parent == run.RunID {
				return true
			}
		}
		return false
	})
	eventually(t, "context-pruner child FINISHed", func() bool {
		for _, f := range mock.FinishedRunsSnapshot() {
			if len(f.RunID) > 0 && f.RunID != run.RunID {
				return true
			}
		}
		return false
	})
	// The child START must carry the ancestor link.
	for _, sr := range mock.StartRequestsSnapshot() {
		if sr.AgentID == "context-pruner" {
			if len(sr.AncestorRunIDs) != 1 || sr.AncestorRunIDs[0] != run.RunID {
				t.Errorf("context-pruner ancestorRunIds = %v, want [%s]", sr.AncestorRunIDs, run.RunID)
			}
		}
	}
	mgr.Shutdown(context.Background())
}

func TestRunPersistenceAdoptAndFinish(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := session.NewStore(t.TempDir() + "/state.json")

	// First process: START a run (persisted).
	mgr1, _ := newTestManagerOpts(t, mock, Options{RotationInterval: time.Hour, Store: store})
	run, err := mgr1.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	pr := store.LoadRun(mgr1.key, agentA)
	if pr == nil || pr.RunID != run.RunID {
		t.Fatalf("run not persisted after START (got %+v)", pr)
	}
	mgr1.Release(run)
	mgr1.Shutdown(context.Background())
	// Second process (restart) on the same store: Acquire must ADOPT the
	// persisted run without a new START.
	mgr2, _ := newTestManagerOpts(t, mock, Options{RotationInterval: time.Hour, Store: store})
	adopted, err := mgr2.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.RunID != run.RunID {
		t.Errorf("restart acquired run %s, want persisted %s (re-STARTed instead of resumed)", adopted.RunID, run.RunID)
	}
	started := mock.StartedRunsSnapshot()
	if len(started) != 1 {
		t.Errorf("STARTs after restart = %d, want 1 (no re-START on adopt)", len(started))
	}
	if adopted.TraceSessionID != run.TraceSessionID {
		t.Errorf("adopted trace session id = %q, want persisted %q", adopted.TraceSessionID, run.TraceSessionID)
	}
	mgr2.Release(adopted)

	// FINISHing the run must remove it from the store so a third restart
	// does not resurrect it.
	mgr2.FinishRun(context.Background(), adopted)
	eventually(t, "FINISH lands", func() bool {
		_, ok := finishedRun(mock, adopted.RunID)
		return ok
	})
	if pr := store.LoadRun(mgr2.key, agentA); pr != nil {
		t.Errorf("run still persisted after FINISH: %+v", pr)
	}
	mgr2.Shutdown(context.Background())
}

func TestRunPersistenceShutdownKeepsRuns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := session.NewStore(t.TempDir() + "/state.json")

	mgr, _ := newTestManagerOpts(t, mock, Options{RotationInterval: time.Hour, Store: store})
	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(run)

	// With a store, Shutdown must NOT FINISH the runs (they stay alive for
	// the restart) — mirroring the session keep-alive.
	mgr.Shutdown(context.Background())
	select {
	case <-time.After(150 * time.Millisecond):
	case <-mockFinishDone(mock, run.RunID):
		t.Fatal("run FINISHed during persistence-enabled Shutdown")
	}
	if pr := store.LoadRun(mgr.key, agentA); pr == nil {
		t.Error("run not persisted after persistence-enabled Shutdown")
	}
}

// mockFinishDone returns a channel closed when the mock records a FINISH
// for runID (or never, when the test deadline passes).
func mockFinishDone(mock *testutil.MockUpstream, runID string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for {
			for _, f := range mock.FinishedRunsSnapshot() {
				if f.RunID == runID {
					close(done)
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return done
}

// nonChildFinished filters the mock's FINISH records to parent (non
// context-pruner) runs: the deferred child-run creation (issue #91)
// FINISHes child runs that pre-#91 tests did not expect.
func nonChildFinished(mock *testutil.MockUpstream) []testutil.FinishedRun {
	var out []testutil.FinishedRun
	for _, f := range mock.FinishedRunsSnapshot() {
		if strings.HasPrefix(f.RunID, "child-run-") {
			continue
		}
		out = append(out, f)
	}
	return out
}

// TestReleaseAbandonedFinishFailureRedrains pins the abandoned-run re-drain
// fix: when the abandoned run's FINISH fails transiently, the run must stay
// on the draining list so Maintain retries it — without membership it would
// be in no set and its cancelled FINISH would be lost forever, leaking the
// upstream agent run.
func TestReleaseAbandonedFinishFailureRedrains(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SetFinishFailures(1)
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.ReleaseAbandoned(run)

	// The first FINISH fails (500): the run must be re-drained, not dropped.
	eventually(t, "run re-drained after failed FINISH", func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		for _, d := range mgr.draining {
			if d == run {
				return true
			}
		}
		return false
	})

	// Maintain retries the FINISH and it lands.
	mgr.Maintain(context.Background())
	eventually(t, "abandoned run FINISH after retry", func() bool {
		f, ok := finishedRun(mock, run.RunID)
		return ok && f.Status == "cancelled"
	})
	mgr.Shutdown(context.Background())
}

// TestRecordStepCap pins the FINISH payload bound: a run recording more than
// maxRecordedSteps keeps only the newest steps, while totalSteps stays
// honest via the monotonic stepTotal.
func TestRecordStepCap(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)
	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Release(run)
	const total = maxRecordedSteps + 50
	for i := 0; i < total; i++ {
		mgr.RecordStep(run, fmt.Sprintf("msg-%d", i))
	}
	_, steps, totalSteps := mgr.finishPayload(run)
	if len(steps) != maxRecordedSteps {
		t.Errorf("steps kept = %d, want %d", len(steps), maxRecordedSteps)
	}
	if totalSteps != total {
		t.Errorf("totalSteps = %d, want %d (monotonic, not capped)", totalSteps, total)
	}
	if last := steps[len(steps)-1].MessageID; last == nil || *last != fmt.Sprintf("msg-%d", total-1) {
		t.Errorf("newest step = %v, want msg-%d", steps[len(steps)-1].MessageID, total-1)
	}
	mgr.Shutdown(context.Background())
}
