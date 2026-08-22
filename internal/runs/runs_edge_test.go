package runs

// Edge-case tests for the run manager: FINISH-failure re-drain, shutdown of
// plain draining runs, inflight accounting across active+draining, prewarm
// and release safety, ctx-cancelled rotation, and the Cooldown ban/country
// window regression.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// TestFinishRunFailureRetriesOnMaintain covers the FINISH-failure path of
// FinishRun (previously untested): an upstream FINISH failure re-appends the
// run to the draining list for a Maintain retry instead of dropping it.
func TestFinishRunFailureRetriesOnMaintain(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID != "run-0001" {
		t.Fatalf("run id = %q, want run-0001", run.RunID)
	}
	mgr.Release(run)

	// With the #91 context-pruner child traffic gone, no async job precedes
	// the FINISH: the synchronous FinishRun below consumes the injected
	// failure deterministically (the released ACTIVE run queues nothing on
	// Release).
	mock.SetFinishFailures(1)
	mgr.FinishRun(context.Background(), run)

	if got := mock.FinishesStartedSnapshot(); got != 1 {
		t.Fatalf("FINISH attempts = %d, want 1 (the failed run)", got)
	}
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
		t.Fatalf("run recorded finished despite FINISH failure: %v", got)
	}
	mgr.mu.Lock()
	stillDraining := false
	for _, d := range mgr.draining {
		if d == run {
			stillDraining = true
		}
	}
	mgr.mu.Unlock()
	if !stillDraining {
		t.Fatal("failed FINISH dropped the run instead of re-draining it")
	}

	// The next Maintain pass retries and succeeds (the retry finishes with
	// the run's own request count, not the original totalSteps).
	mgr.Maintain(context.Background())
	eventually(t, "Maintain retry FINISH", func() bool {
		f, ok := finishedRun(mock, "run-0001")
		return ok && f.Status == "completed"
	})
	if snap := mgr.Snapshot(); snap.ActiveRuns != 0 {
		t.Errorf("active runs after drain = %d, want 0", snap.ActiveRuns)
	}
}

// TestFinishIfReadyFailureKeepsDraining covers the async drain-failure path:
// when a rotated run's FINISH fails, finishIfReady resets the finishing flag
// and leaves the run draining for the next Maintain pass to retry.
func TestFinishIfReadyFailureKeepsDraining(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, 40*time.Millisecond)

	lease, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(lease)

	// Age the run so Maintain rotates it (deterministic; no sleep).
	mgr.mu.Lock()
	mgr.runs[agentA].StartedAt = time.Now().Add(-time.Hour)
	mgr.mu.Unlock()

	// With the #91 context-pruner child traffic gone, no other FINISH can
	// precede the rotated run's attempt: pre-set the failure so the async
	// drain FINISH of run-0001 consumes it.
	mock.SetFinishFailures(1)
	mgr.Maintain(context.Background())

	// The rotated run's async FINISH failed: wait for the attempt to hit
	// the mock, then assert it is not recorded and still draining.
	eventually(t, "run-0001 FINISH attempt", func() bool {
		return mock.FinishesStartedSnapshot() >= 1
	})
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
		t.Fatalf("run recorded finished despite FINISH failure: %v", got)
	}
	mgr.mu.Lock()
	stillDraining := false
	for _, d := range mgr.draining {
		if d.RunID == "run-0001" {
			stillDraining = true
		}
	}
	mgr.mu.Unlock()
	if !stillDraining {
		t.Fatal("failed async FINISH dropped the run instead of keeping it draining")
	}

	// The next Maintain pass retries the drain and succeeds.
	mgr.Maintain(context.Background())
	eventually(t, "Maintain retry FINISH", func() bool {
		f, ok := finishedRun(mock, "run-0001")
		return ok && f.Status == "completed"
	})
}

// TestShutdownDrainsPlainDrainingRun covers Shutdown with a draining run
// that has no FINISH in flight (finishing=false) — the existing tests cover
// only the finishing-flag skip and the active-run path. A plain draining run
// must be FINISHed exactly once.
func TestShutdownDrainsPlainDrainingRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	lease, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(lease)

	// Move the run to the draining list without a finishing flag (a rotated
	// run whose async finishIfReady has not run yet).
	mgr.mu.Lock()
	run := mgr.runs[agentA]
	delete(mgr.runs, agentA)
	mgr.draining = append(mgr.draining, run)
	mgr.mu.Unlock()

	mgr.Shutdown(context.Background())

	finished := mock.FinishedRunsSnapshot()
	if len(finished) != 1 || finished[0].RunID != "run-0001" {
		t.Fatalf("finished runs = %v, want exactly [run-0001]", finished)
	}
	if finished[0].Status != "completed" {
		t.Errorf("run finished with status %q, want completed", finished[0].Status)
	}
	if snap := mgr.Snapshot(); snap.ActiveRuns != 0 {
		t.Errorf("active runs after shutdown = %d, want 0", snap.ActiveRuns)
	}
}

// TestInflightCountActiveAndDraining pins InflightCount across both sets:
// active runs AND rotated runs still draining count (the pool skips busy
// bridge entries on it, so an undercount would evict a live chat).
func TestInflightCountActiveAndDraining(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, 40*time.Millisecond)

	runA, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	// Age the run so the next acquire rotates it into draining while the
	// lease is still held.
	mgr.mu.Lock()
	mgr.runs[agentA].StartedAt = time.Now().Add(-time.Hour)
	mgr.mu.Unlock()
	runB, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if runB.RunID == runA.RunID {
		t.Fatalf("expected a rotation, got the same run %s", runA.RunID)
	}

	// runA is draining with its lease still held; runB is active with a
	// fresh lease.
	if got := mgr.InflightCount(); got != 2 {
		t.Errorf("InflightCount = %d, want 2 (draining lease + active lease)", got)
	}
	mgr.Release(runA)
	if got := mgr.InflightCount(); got != 1 {
		t.Errorf("InflightCount after releasing the draining run = %d, want 1", got)
	}
	mgr.Release(runB)
	if got := mgr.InflightCount(); got != 0 {
		t.Errorf("InflightCount after releasing the active run = %d, want 0", got)
	}
}

// TestPrewarmDuringCooldown pins the cooldown guard in the prewarm path:
// Prewarm during a cooldown must not START any run (and must not panic).
func TestPrewarmDuringCooldown(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	mgr.Cooldown(time.Hour)
	mgr.Prewarm(context.Background(), []string{agentA, agentB})

	if got := len(mock.StartedRunsSnapshot()); got != 0 {
		t.Errorf("STARTs during cooldown = %d, want 0", got)
	}
	if snap := mgr.Snapshot(); snap.ActiveRuns != 0 {
		t.Errorf("active runs = %d, want 0", snap.ActiveRuns)
	}
}

// TestReleaseAfterShutdownAndDoubleRelease pins Release's no-op safety: a
// double release and a release after Shutdown (the run is no longer tracked)
// must not panic or corrupt the inflight accounting.
func TestReleaseAfterShutdownAndDoubleRelease(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(run)
	mgr.Release(run) // double release: inflight already 0
	if got := mgr.InflightCount(); got != 0 {
		t.Fatalf("InflightCount after double release = %d, want 0", got)
	}

	mgr.Shutdown(context.Background())
	mgr.Release(run) // release after shutdown: run no longer tracked
	mgr.Release(nil)
	if got := mgr.InflightCount(); got != 0 {
		t.Errorf("InflightCount after post-shutdown release = %d, want 0", got)
	}
}

// blockingStartRT holds the first agent-runs request (START) in flight until
// its context is canceled, exposing the rotation's upstream call to
// cancellation. Everything else delegates to base.
type blockingStartRT struct {
	base    http.RoundTripper
	started chan struct{}
	once    sync.Once
}

func (b *blockingStartRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/agent-runs") {
		b.once.Do(func() { close(b.started) })
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	return b.base.RoundTrip(req)
}

// TestAcquireCancelledCtxMidRotation pins ctx cancellation during a run
// START (mid-rotation): Acquire must abort with ctx.Err() and leave no run
// registered.
func TestAcquireCancelledCtxMidRotation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
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
	mgr := NewRunManager(client, sess, time.Hour)

	rt := &blockingStartRT{base: http.DefaultTransport, started: make(chan struct{})}
	client.SetTransport(rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := mgr.Acquire(ctx, agentA)
		done <- err
	}()

	// Wait until the START is in flight, then cancel it.
	select {
	case <-rt.started:
	case <-time.After(2 * time.Second):
		t.Fatal("START request never reached the transport")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Acquire after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return after ctx cancel")
	}
	if got := len(mock.StartedRunsSnapshot()); got != 0 {
		t.Errorf("START completed despite cancel: %v", mock.StartedRunsSnapshot())
	}
}

// TestCooldownClearsBanAndCountryWindows is the regression guard for the P2
// Cooldown bug: Cooldown cleared the remembered ban/country errors but left
// their Until deadlines set, so Snapshot().BannedUntil reported a stale
// future deadline with no ban attached (healthz risk gating). Fails before
// the fix (BannedUntil stays set after Cooldown).
func TestCooldownClearsBanAndCountryWindows(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	until := time.Now().Add(10 * time.Minute)
	mgr.CooldownBan(&upstream.BanError{Body: "banned", ResumesAt: until})
	if mgr.BanError() == nil {
		t.Fatal("expected the ban to be live before Cooldown")
	}
	if snap := mgr.Snapshot(); snap.BannedUntil.IsZero() {
		t.Fatal("BannedUntil not set during the ban window")
	}

	mgr.Cooldown(DefaultCooldown)
	if mgr.BanError() != nil {
		t.Error("BanError not cleared by Cooldown")
	}
	if snap := mgr.Snapshot(); !snap.BannedUntil.IsZero() {
		t.Errorf("BannedUntil = %v after Cooldown, want zero (stale ban deadline)", snap.BannedUntil)
	}

	// Same for the country window (deadline cleared with the remembered
	// error; the block stops surfacing).
	mgr.CooldownCountryBlocked(&upstream.CountryBlockedError{CountryCode: "CN", CountryBlockReason: "region_restricted"})
	if mgr.CountryBlockedError() == nil {
		t.Fatal("expected the country block to be live before Cooldown")
	}
	mgr.Cooldown(DefaultCooldown)
	if mgr.CountryBlockedError() != nil {
		t.Error("country block not cleared by Cooldown")
	}
}
