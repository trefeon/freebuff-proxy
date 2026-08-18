package runs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	// Issue #114: record 3 completed steps — totalSteps must come from the
	// recorded steps (preferred over the request-count fallback) and the
	// steps must ride IN the FINISH payload.
	for i := 0; i < 3; i++ {
		mgr.RecordStep(run, "")
	}
	mgr.FinishRun(context.Background(), run)

	eventually(t, "FINISH payload", func() bool {
		f, ok := finishedRun(mock, "run-0001")
		return ok && f.Status == "completed" && f.TotalSteps == 3 && len(f.Steps) == 3 &&
			f.Steps[0].StepNumber == 1 && f.Steps[2].StepNumber == 3
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
	if started := mock.StartedRunsSnapshot(); len(started) != 2 {
		t.Errorf("STARTs = %d, want 2", len(started))
	}
	// Issue #91: each START also creates+FINISHes a context-pruner child
	// run asynchronously — filter those out; the parent must never be
	// FINISHed by Invalidate.
	if finished := nonChildFinished(mock); len(finished) != 0 {
		t.Errorf("invalidated run must not be FINISHed, got %v", finished)
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

	// Idempotent: a second shutdown must not duplicate FINISHes. Child-run
	// FINISHes (issue #91) are excluded from the count.
	mgr.Shutdown(context.Background())
	if got := len(nonChildFinished(mock)); got != 2 {
		t.Errorf("finished runs after double shutdown = %d, want 2", got)
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

func TestMaintainSkipsDuringCooldown(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, 40*time.Millisecond)

	// A live run aged past rotation would normally make Maintain rotate it
	// (START) and a draining run would be FINISHed; with the token in
	// cooldown neither may touch the upstream, and nothing may be logged
	// (production observed a "maintain rotate failed ... token cooling
	// down" error once per minute).
	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(run)
	time.Sleep(80 * time.Millisecond) // age the run past rotation

	mgr.Cooldown(time.Hour)

	restore, logged := captureSlog()
	defer restore()

	started := len(mock.StartedRunsSnapshot())
	finished := len(mock.FinishedRunsSnapshot())
	mgr.Maintain(context.Background())

	if got := len(mock.StartedRunsSnapshot()); got != started {
		t.Errorf("STARTs during cooldown = %d, want %d (no rotate)", got, started)
	}
	if got := len(mock.FinishedRunsSnapshot()); got != finished {
		t.Errorf("FINISHes during cooldown = %d, want %d (no drain)", got, finished)
	}
	if out := logged(); out != "" {
		t.Errorf("Maintain logged during cooldown:\n%s", out)
	}
}

// captureSlog swaps the default slog handler for one recording Debug+
// messages and returns a restore func plus a snapshot of everything logged
// since capture. Used to assert that quiet paths emit no log lines.
func captureSlog() (restore func(), logged func() string) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(prev) }, func() string { return buf.String() }
}

func TestPrewarmStartsAllAgentsOnce(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	mgr.Prewarm(context.Background(), []string{agentA, agentB})
	if started := mock.StartedRunsSnapshot(); len(started) != 2 {
		t.Fatalf("STARTs after prewarm = %d, want 2", len(started))
	}

	// A second prewarm must not restart existing runs.
	mgr.Prewarm(context.Background(), []string{agentA, agentB})
	if started := mock.StartedRunsSnapshot(); len(started) != 2 {
		t.Errorf("STARTs after second prewarm = %d, want still 2", len(started))
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

	// Cooldown expired in the past should automatically unlock (return nil)
	mgr.mu.Lock()
	mgr.cooldownUntil = time.Now().Add(-1 * time.Second)
	mgr.mu.Unlock()
	if mgr.RateLimitError() != nil {
		t.Errorf("RateLimitError() != nil for expired cooldown, want automatic unlock")
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

func TestCountryBlockCooldown(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)
	cbe := &upstream.CountryBlockedError{CountryCode: "CN", CountryBlockReason: "region_restricted"}

	mgr.CooldownCountryBlocked(cbe)

	if got := mgr.CountryBlockedError(); got == nil || got.CountryCode != "CN" {
		t.Fatalf("CountryBlockedError() = %v, want remembered CN block", got)
	}
	if until := mgr.CooldownUntil(); !time.Now().Before(until) || time.Until(until) > 16*time.Minute {
		t.Errorf("cooldown until = %v, want ~15m country window", until)
	}
	// The country block clears any remembered rate-limit/ban state so the
	// cooldown-skip surfaces the country error, not a stale 429.
	if mgr.RateLimitError() != nil || mgr.BanError() != nil {
		t.Errorf("rate/ban memory not cleared by country block")
	}

	// Acquire skips the token during the window (shared cooldown deadline).
	if _, err := mgr.Acquire(context.Background(), agentA); err == nil {
		t.Error("Acquire during country cooldown succeeded, want skip error")
	}

	// Expired country window unlocks like the rate-limit memory.
	mgr.mu.Lock()
	mgr.countryUntil = time.Now().Add(-1 * time.Second)
	mgr.mu.Unlock()
	if mgr.CountryBlockedError() != nil {
		t.Errorf("CountryBlockedError() != nil for expired window, want automatic unlock")
	}
}

// TestCooldownIpCappedCapsReAdmits pins #118: the CLI treats ip_capped as
// terminal-until-reset (never an automatic re-admission loop), so the
// proxy's CooldownIpCapped honors the FULL retryAfterMs (+jitter) for the
// first refusals of a Pacific day, then locks the token until the next
// Pacific midnight after maxIpCappedReAdmitsPerDay — with the remembered
// error's Retry-After reflecting the remaining window.
func TestCooldownIpCappedCapsReAdmits(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	ice := &upstream.IpCappedError{ActiveUsersForIP: 8, Limit: 6, RetryAfter: 60 * time.Second, Body: `{"status":"ip_capped"}`}

	// Refusals 1..max-1: bounded window = full retryAfterMs + jitter (the
	// jitter only ever extends the window, never shrinks it).
	for i := 1; i < maxIpCappedReAdmitsPerDay; i++ {
		mgr.CooldownIpCapped(ice)
		until := mgr.CooldownUntil()
		if !time.Now().Before(until) {
			t.Fatalf("refusal %d: cooldown already expired, want now+retryAfter(+jitter)", i)
		}
		if time.Until(until) < ice.RetryAfter-time.Second {
			t.Errorf("refusal %d: window %v shorter than retryAfterMs %v (jitter must not shrink it)",
				i, time.Until(until), ice.RetryAfter)
		}
		if got := mgr.IpCappedError(); got == nil || got.RetryAfter != 60*time.Second {
			t.Errorf("refusal %d: IpCappedError = %+v, want remembered original window", i, got)
		}
	}

	// Budget exhausted: terminal until the next Pacific midnight.
	mgr.CooldownIpCapped(ice)
	want := upstream.NextPacificMidnight()
	if until := mgr.CooldownUntil(); until.Sub(want) > time.Second || want.Sub(until) > time.Second {
		t.Errorf("terminal lock until = %v, want ~Pacific midnight %v", until, want)
	}
	got := mgr.IpCappedError()
	if got == nil {
		t.Fatal("IpCappedError() = nil after budget exhausted, want remembered terminal error")
	}
	if got.RetryAfter < time.Minute {
		t.Errorf("terminal RetryAfter = %v, want the remaining window to midnight (>= 1m)", got.RetryAfter)
	}

	// Further refusals the same day must not move the lock (no re-admit loop).
	first := mgr.CooldownUntil()
	mgr.CooldownIpCapped(ice)
	if until := mgr.CooldownUntil(); !until.Equal(first) {
		t.Errorf("terminal lock moved on extra refusal: %v -> %v", first, until)
	}

	// Acquire skips the token during the terminal window (shared cooldown).
	if _, err := mgr.Acquire(context.Background(), agentA); err == nil {
		t.Error("Acquire during terminal ip_capped lock succeeded, want skip error")
	}

	// Pacific day rollover resets the budget: the next refusal gets a
	// bounded window again instead of staying locked.
	mgr.mu.Lock()
	mgr.ipCappedDayReset = time.Time{} // force the "new day" branch
	mgr.mu.Unlock()
	mgr.CooldownIpCapped(ice)
	until := mgr.CooldownUntil()
	if until.Equal(want) {
		t.Fatal("lock did not lift on the new Pacific day")
	}
	if !time.Now().Before(until) || time.Until(until) > ice.RetryAfter+2*time.Minute {
		t.Errorf("post-reset window = %v, want now+retryAfterMs(+jitter)", time.Until(until))
	}

	// ClearCooldowns (dashboard unlock) resets the budget too.
	mgr.CooldownIpCapped(ice)
	mgr.CooldownIpCapped(ice)
	mgr.CooldownIpCapped(ice)
	if !time.Now().Before(mgr.CooldownUntil()) {
		t.Fatal("expected terminal lock before ClearCooldowns")
	}
	mgr.ClearCooldowns()
	mgr.CooldownIpCapped(ice)
	if until := mgr.CooldownUntil(); !time.Now().Before(until) || time.Until(until) > ice.RetryAfter+2*time.Minute {
		t.Errorf("post-ClearCooldowns window = %v, want bounded window (budget reset)", time.Until(until))
	}
}

func TestCountryBlockDoesNotDowngradeBan(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)
	be := &upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(2 * time.Hour)}
	mgr.CooldownBan(be)
	cbe := &upstream.CountryBlockedError{CountryCode: "CN", CountryBlockReason: "region_restricted"}
	mgr.CooldownCountryBlocked(cbe)

	// A ban outranks a country block (pool precedence ban > country): the
	// ban memory and window must survive the country block.
	if mgr.BanError() == nil {
		t.Errorf("BanError() = nil after country block, ban must outrank country")
	}
	if mgr.CountryBlockedError() != nil {
		t.Errorf("CountryBlockedError() != nil, country must not overwrite an active ban")
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

// TestShutdownSkipsMidFinishRun is the regression guard for the P2
// double-FINISH race: rotate spawns an untracked finishIfReady goroutine
// that may be mid-FINISH (finishing=true, run on the draining list) when
// Shutdown gathers. Shutdown must skip those runs instead of calling
// FinishRun again for the same run id upstream.
func TestShutdownSkipsMidFinishRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	lease, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(lease)

	// Simulate the async rotator mid-FINISH for agentA's run (run-0001):
	// draining with the finishing flag set — finishIfReady's upstream
	// FINISH is in flight.
	mgr.mu.Lock()
	runA := mgr.runs[agentA]
	runA.finishing = true
	mgr.draining = append(mgr.draining, runA)
	mgr.mu.Unlock()

	// A second agent with a plain run must still be finished exactly once.
	leaseB, err := mgr.Acquire(context.Background(), agentB)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(leaseB)

	mgr.Shutdown(context.Background())

	finished := nonChildFinished(mock)
	if len(finished) != 1 {
		t.Fatalf("finished runs = %v, want exactly 1 (only agentB's run)", finished)
	}
	if finished[0].RunID != "run-0002" {
		t.Errorf("finished run = %q, want run-0002 (agentB); mid-FINISH run-0001 must not be re-FINISHed", finished[0].RunID)
	}
	if snap := mgr.Snapshot(); snap.ActiveRuns != 0 {
		t.Errorf("active runs after shutdown = %d, want 0", snap.ActiveRuns)
	}
}

// TestAcquireConcurrentFinishAllRuns hammers Acquire against a concurrent
// idle FinishAllRuns, which used to surface a phantom "run missing after
// rotation" failure whenever the run map was cleared mid-acquire. With the
// re-validation loop every acquire either completes or fails with a real
// error — never the cleared-map phantom.
func TestAcquireConcurrentFinishAllRuns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Generous id pool: acquires racing the idle FINISH can re-START the
	// run several times per cleared window.
	ids := make([]string, 2000)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%04d", i)
	}
	mock.RunIDs = ids
	mgr, _ := newTestManager(t, mock, time.Hour)

	const acquireGoroutines = 8
	const perGoroutine = 60
	var wg sync.WaitGroup
	var failures atomicError

	for g := 0; g < acquireGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				run, err := mgr.Acquire(context.Background(), agentA)
				if err != nil {
					failures.set(err)
					continue
				}
				mgr.Release(run)
			}
		}()
	}
	for i := 0; i < 60; i++ {
		mgr.FinishAllRuns(context.Background())
	}
	wg.Wait()

	if err := failures.get(); err != nil {
		t.Fatalf("acquire failed while FinishAllRuns raced: %v", err)
	}
}

// TestSnapshotBannedUntil surfaces the ban window deadline the pool uses to
// gate its ban risk label (fixes the sticky "critical" healthz after an
// expired ban).
func TestSnapshotBannedUntil(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	// InflightCount reflects outstanding leases (bridge eviction skips
	// busy entries on it).
	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if got := mgr.InflightCount(); got != 1 {
		t.Errorf("InflightCount with one lease = %d, want 1", got)
	}
	mgr.Release(run)
	if got := mgr.InflightCount(); got != 0 {
		t.Errorf("InflightCount after release = %d, want 0", got)
	}

	until := time.Now().Add(10 * time.Minute)
	mgr.CooldownBan(&upstream.BanError{Body: "banned", ResumesAt: until})

	snap := mgr.Snapshot()
	if snap.BanError == nil {
		t.Fatal("Snapshot.BanError = nil, want non-nil during the ban window")
	}
	if !snap.BannedUntil.Equal(until) {
		t.Errorf("Snapshot.BannedUntil = %v, want %v", snap.BannedUntil, until)
	}
}

func TestClearCooldowns(t *testing.T) {
	m := NewRunManager(nil, nil, time.Hour)
	now := time.Now()
	m.Cooldown(time.Hour)
	m.CooldownRateLimit(&upstream.RateLimitError{RetryAfter: time.Hour})
	if m.RateLimitError() == nil {
		t.Fatal("expected rate-limit lock to be active")
	}
	// A ban supersedes the rate-limit lock (mutually exclusive by design).
	m.CooldownBan(&upstream.BanError{ResumesAt: now.Add(2 * time.Hour)})
	if m.RateLimitError() != nil || m.BanError() == nil {
		t.Fatal("expected ban to supersede rate-limit lock")
	}
	m.ClearCooldowns()
	if !m.CooldownUntil().IsZero() {
		t.Errorf("cooldown not cleared: %v", m.CooldownUntil())
	}
	if m.RateLimitError() != nil {
		t.Error("rate-limit lock not cleared")
	}
	if m.BanError() != nil {
		t.Error("ban window not cleared")
	}
}

func TestSingleFlightRunAcquisition(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	const concurrency = 20
	var wg sync.WaitGroup
	runs := make([]*Run, concurrency)
	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r, err := mgr.Acquire(context.Background(), agentA)
			runs[idx] = r
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d failed: %v", i, err)
		}
		if runs[i] == nil {
			t.Fatalf("goroutine %d returned nil run", i)
		}
		if runs[i].RunID != "run-0001" {
			t.Errorf("goroutine %d RunID = %q, want run-0001", i, runs[i].RunID)
		}
		mgr.Release(runs[i])
	}

	started := mock.StartedRunsSnapshot()
	if len(started) != 1 {
		t.Fatalf("StartedRuns count = %d, want 1 (single-flight coalesced)", len(started))
	}
}

func TestSingleFlightRunRotation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, 50*time.Millisecond)

	// First acquire
	r1, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if r1.RunID != "run-0001" {
		t.Fatalf("first RunID = %q, want run-0001", r1.RunID)
	}
	mgr.Release(r1)

	// Wait for rotation interval to pass
	time.Sleep(70 * time.Millisecond)

	const concurrency = 20
	var wg sync.WaitGroup
	runs := make([]*Run, concurrency)
	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r, err := mgr.Acquire(context.Background(), agentA)
			runs[idx] = r
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d failed: %v", i, err)
		}
		if runs[i] == nil {
			t.Fatalf("goroutine %d returned nil run", i)
		}
		if runs[i].RunID != "run-0002" {
			t.Errorf("goroutine %d RunID = %q, want run-0002", i, runs[i].RunID)
		}
		mgr.Release(runs[i])
	}

	started := mock.StartedRunsSnapshot()
	if len(started) != 2 {
		t.Fatalf("StartedRuns count = %d, want 2 (initial + 1 coalesced rotation)", len(started))
	}
}

// ── Wave 1 issue tests (#80) ─────────────────────────────────────────────

// TestTraceSessionIDMintedPerRun verifies #80: each run mints a crypto/rand
// trace session id once and reuses it across the run's requests; a rotated
// run gets a fresh one.
func TestTraceSessionIDMintedPerRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if run.TraceSessionID == "" {
		t.Fatal("TraceSessionID empty, want minted UUID per run")
	}
	first := run.TraceSessionID
	mgr.Release(run)

	// A second acquire of the same run (no rotation) reuses the same id.
	run2, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if run2.TraceSessionID != first {
		t.Errorf("TraceSessionID = %q after re-acquire, want %q (stable per run)", run2.TraceSessionID, first)
	}
	mgr.Release(run2)

	// Force a rotation: age the current run past the rotation interval so
	// the next acquire rotates it; a fresh run gets a fresh trace id.
	mgr.mu.Lock()
	mgr.runs[agentA].StartedAt = time.Now().Add(-2 * time.Hour)
	mgr.mu.Unlock()
	run3, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if run3.TraceSessionID == "" {
		t.Fatal("TraceSessionID empty after rotation")
	}
	if run3.TraceSessionID == first {
		t.Errorf("TraceSessionID = %q after rotation, want a fresh id", run3.TraceSessionID)
	}
	mgr.Release(run3)
}
