package runs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

const agentA = "agent-alpha"
const agentB = "agent-beta"

// newTestManager wires the mock upstream through a real client and session
// manager.
func newTestManager(t *testing.T, mock *testutil.MockUpstream, rotationInterval time.Duration) (*RunManager, *session.Manager) {
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
	sess := session.NewManager(client)
	return NewRunManager(client, sess, rotationInterval), sess
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func finishedRun(mock *testutil.MockUpstream, runID string) (testutil.FinishedRun, bool) {
	// Snapshot under the mock's lock: FINISH arrives from a background
	// goroutine while the test polls (eventually), so raw field reads race.
	for _, f := range mock.FinishedRunsSnapshot() {
		if f.RunID == runID {
			return f, true
		}
	}
	return testutil.FinishedRun{}, false
}

func TestRotationAtInterval(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, 40*time.Millisecond)

	first, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if first.RunID != "run-0001" {
		t.Fatalf("first run id = %q, want run-0001", first.RunID)
	}
	if first.StartedAt.IsZero() {
		t.Error("StartedAt not set")
	}
	mgr.Release(first)

	// Let the run age past the rotation interval, then acquire again: the
	// old run must be rotated away and FINISHed asynchronously.
	time.Sleep(80 * time.Millisecond)
	second, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if second.RunID != "run-0002" {
		t.Fatalf("second run id = %q, want run-0002 (rotated)", second.RunID)
	}

	eventually(t, "FINISH of rotated run", func() bool {
		f, ok := finishedRun(mock, "run-0001")
		return ok && f.Status == "completed" && f.TotalSteps == 1
	})
}

func TestFinishRunDropsFromActive(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID != "run-0001" {
		t.Fatalf("run id = %q", run.RunID)
	}
	mgr.Release(run)

	mgr.FinishRun(context.Background(), run, 3)

	eventually(t, "FINISH payload", func() bool {
		f, ok := finishedRun(mock, "run-0001")
		return ok && f.Status == "completed" && f.TotalSteps == 3
	})

	// Dropped from active: the next acquire must START afresh.
	next, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if next.RunID != "run-0002" {
		t.Fatalf("run after FinishRun = %q, want run-0002 (re-START)", next.RunID)
	}
}

func TestInvalidateRestarts(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID != "run-0001" {
		t.Fatalf("run id = %q", run.RunID)
	}
	mgr.Release(run)

	// Upstream said "runId not found"; drop it without FINISH.
	mgr.Invalidate(agentA)

	next, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if next.RunID != "run-0002" {
		t.Fatalf("run after Invalidate = %q, want run-0002 (re-START)", next.RunID)
	}
	if len(mock.StartedRuns) != 2 {
		t.Errorf("STARTs = %d, want 2", len(mock.StartedRuns))
	}
	if len(mock.FinishedRuns) != 0 {
		t.Errorf("invalidated run must not be FINISHed, got %v", mock.FinishedRuns)
	}
}

func TestCooldownBlocksAcquire(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	if _, err := mgr.Acquire(context.Background(), agentA); err != nil {
		t.Fatal(err)
	}
	mgr.Cooldown(DefaultCooldown)

	until := mgr.CooldownUntil()
	if until.Before(time.Now().Add(DefaultCooldown - time.Second)) {
		t.Errorf("CooldownUntil = %v, want ~now+%s", until, DefaultCooldown)
	}
	if snap := mgr.Snapshot(); snap.CooldownUntil != until {
		t.Errorf("snapshot CooldownUntil = %v, want %v", snap.CooldownUntil, until)
	}

	_, err := mgr.Acquire(context.Background(), agentA)
	if err == nil {
		t.Fatal("Acquire succeeded while cooling down")
	}
	if !strings.Contains(err.Error(), "cooling down until") {
		t.Errorf("error = %v, want cooldown error", err)
	}
	if errors.Is(err, upstream.ErrAuthRejected) {
		t.Error("cooldown error must not alias ErrAuthRejected")
	}
}

func TestShutdownFinishesAllAndEndsSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, sess := newTestManager(t, mock, time.Hour)

	// Prime an active upstream session so EndSession actually fires.
	if _, err := sess.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	a, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := mgr.Acquire(context.Background(), agentB)
	if err != nil {
		t.Fatal(err)
	}
	// Leases deliberately left un-released: shutdown must force-finish.
	_ = a
	_ = b

	mgr.Shutdown(context.Background())

	for _, want := range []struct {
		runID string
		steps int
	}{{"run-0001", 1}, {"run-0002", 1}} {
		f, ok := finishedRun(mock, want.runID)
		if !ok {
			t.Errorf("run %s not FINISHed on shutdown", want.runID)
			continue
		}
		if f.Status != "completed" || f.TotalSteps != want.steps {
			t.Errorf("run %s finished as %+v, want completed/%d", want.runID, f, want.steps)
		}
	}
	if mock.SessionEnds != 1 {
		t.Errorf("session ends = %d, want 1", mock.SessionEnds)
	}
	if snap := mgr.Snapshot(); snap.ActiveRuns != 0 {
		t.Errorf("active runs after shutdown = %d, want 0", snap.ActiveRuns)
	}

	// Idempotent: a second shutdown must not duplicate FINISHes.
	mgr.Shutdown(context.Background())
	if len(mock.FinishedRuns) != 2 {
		t.Errorf("finished runs after double shutdown = %d, want 2", len(mock.FinishedRuns))
	}
}

func TestMaintainDrainsOnlyWhenIdle(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, 40*time.Millisecond)

	lease, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(80 * time.Millisecond) // age the run
	mgr.Maintain(context.Background())

	// The old run is draining but has an outstanding lease: give the async
	// finish goroutine time to run, then assert it was NOT finished.
	time.Sleep(100 * time.Millisecond)
	if _, ok := finishedRun(mock, "run-0001"); ok {
		t.Fatal("draining run FINISHed while inflight > 0")
	}

	mgr.Release(lease)
	mgr.Maintain(context.Background())

	eventually(t, "FINISH of released draining run", func() bool {
		f, ok := finishedRun(mock, "run-0001")
		return ok && f.Status == "completed" && f.TotalSteps == 1
	})
}

func TestPrewarmStartsAllAgentsOnce(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	mgr.Prewarm(context.Background(), []string{agentA, agentB})
	if len(mock.StartedRuns) != 2 {
		t.Fatalf("STARTs after prewarm = %d, want 2", len(mock.StartedRuns))
	}

	// A second prewarm must not restart existing runs.
	mgr.Prewarm(context.Background(), []string{agentA, agentB})
	if len(mock.StartedRuns) != 2 {
		t.Errorf("STARTs after second prewarm = %d, want still 2", len(mock.StartedRuns))
	}

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID != "run-0001" {
		t.Errorf("prewarmed run id = %q, want run-0001 (agent-alpha)", run.RunID)
	}
}

func TestConcurrentAcquireRelease(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Concurrent cold starts can each trigger a START; make the id queue
	// deep enough that the hammer never exhausts it.
	ids := make([]string, 200)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%04d", i)
	}
	mock.RunIDs = ids
	mgr, _ := newTestManager(t, mock, time.Hour)

	const goroutines = 8
	const perGoroutine = 40
	var wg sync.WaitGroup
	var failures atomicError
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			agent := agentA
			if g%2 == 1 {
				agent = agentB
			}
			for i := 0; i < perGoroutine; i++ {
				run, err := mgr.Acquire(context.Background(), agent)
				if err != nil {
					failures.set(err)
					continue
				}
				mgr.Release(run)
			}
		}(g)
	}
	wg.Wait()

	if err := failures.get(); err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	if snap := mgr.Snapshot(); snap.Requests != goroutines*perGoroutine {
		t.Errorf("snapshot requests = %d, want %d", snap.Requests, goroutines*perGoroutine)
	}
}

// atomicError is a tiny thread-safe first-error holder for the hammer.
type atomicError struct {
	mu  sync.Mutex
	err error
}

func (e *atomicError) set(err error) {
	e.mu.Lock()
	if e.err == nil {
		e.err = err
	}
	e.mu.Unlock()
}

func (e *atomicError) get() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}
func TestFinishAllRuns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	lease, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(lease)

	mgr.FinishAllRuns(context.Background())
	snap := mgr.Snapshot()
	if snap.ActiveRuns != 0 {
		t.Errorf("ActiveRuns = %d, want 0 after FinishAllRuns", snap.ActiveRuns)
	}
}

func TestRateLimitAndBanCooldowns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	rle := &upstream.RateLimitError{Body: "rate limit", RetryAfter: 5 * time.Minute}
	mgr.CooldownRateLimit(rle)

	if mgr.RateLimitError() == nil {
		t.Errorf("RateLimitError() = nil, want rate limit error")
	}

	be := &upstream.BanError{Body: "account banned", ResumesAt: time.Now().Add(10 * time.Minute)}
	mgr.CooldownBan(be)

	if mgr.BanError() == nil {
		t.Errorf("BanError() = nil, want ban error")
	}

	snap := mgr.Snapshot()
	if snap.BanError == nil {
		t.Errorf("Snapshot.BanError = nil, want non-nil")
	}
}

func TestInvalidateRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	lease, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(lease)

	mgr.Invalidate(agentA)
	snap := mgr.Snapshot()
	if snap.ActiveRuns != 0 {
		t.Errorf("ActiveRuns = %d after Invalidate, want 0", snap.ActiveRuns)
	}
}
