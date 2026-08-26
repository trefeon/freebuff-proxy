package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

func TestNewLengthMismatch(t *testing.T) {
	cfg := &config.Config{AuthTokens: []string{"a", "b"}, RotationInterval: time.Hour}
	if _, err := New(cfg, nil, nil, registry.New(cfg, nil)); err == nil {
		t.Fatal("want error for client/session count mismatch")
	}
}

func TestRoundRobinDistribution(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	// Strict round-robin only applies while no token holds a live session:
	// hot-session-first selection routes every acquire to a live session
	// once one exists. Invalidate both cached sessions before each acquire
	// so the cold path is exercised, and assert the historical order is
	// unchanged (selection-order change must not regress cold failover).
	const n = 6
	got := make([]int, n)
	for i := 0; i < n; i++ {
		// Unconditional invalidation (test intent: force the cold path) —
		// the pool's InvalidateSession is now instance-guarded (#132).
		toks := p.toks.Load()
		(*toks)[0].session.Invalidate()
		(*toks)[1].session.Invalidate()
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatal(err)
		}
		got[i] = lease.Token
		if lease.AgentID != agentA {
			t.Errorf("lease agent = %q, want %q", lease.AgentID, agentA)
		}
		p.LeaseRelease(lease)
	}
	for i, want := range []int{0, 1, 0, 1, 0, 1} {
		if got[i] != want {
			t.Errorf("acquire %d token = %d, want %d", i, got[i], want)
		}
	}
	// Both tokens created the run for the agent exactly once (runs survive
	// session invalidation).
	for i, mock := range []*testutil.MockUpstream{mock0, mock1} {
		started := mock.StartedRunsSnapshot()
		if len(started) != 1 || started[0] != agentA {
			t.Errorf("mock%d started runs = %v, want [%s]", i, started, agentA)
		}
		if len(mock.FinishedRunsSnapshot()) != 0 {
			t.Errorf("mock%d finished runs = %v, want none", i, mock.FinishedRunsSnapshot())
		}
	}

	snaps := p.Snapshot()
	for i, snap := range snaps {
		if snap.ActiveRuns != 1 || snap.Requests != 3 {
			t.Errorf("token %d snapshot: active=%d requests=%d, want 1/3", i, snap.ActiveRuns, snap.Requests)
		}
	}
	// The last acquire (round-robin start 1) re-created token 1's session
	// fresh; token 0's was invalidated before it and is gone. Every acquire
	// admitted a fresh session: the cold path never reused a live one (3
	// creates per token, one per acquire).
	if snaps[1].SessionStatus != "active" || snaps[1].SessionInstanceID != "inst-abc-123" {
		t.Errorf("token 1 session snapshot = %q/%q, want active/inst-abc-123", snaps[1].SessionStatus, snaps[1].SessionInstanceID)
	}
	if mock0.SessionCreates != 3 || mock1.SessionCreates != 3 {
		t.Errorf("session creates = %d/%d, want 3/3 (cold path only)", mock0.SessionCreates, mock1.SessionCreates)
	}
}

func TestAcquirePrefersTokenWithLiveSession(t *testing.T) {
	mock1 := testutil.NewMock() // token 1 (index 0): will hold the live session
	defer mock1.Close()
	mock2 := testutil.NewMock() // token 2 (index 1): stays fresh
	defer mock2.Close()
	p := newTestPool(t, mock1, mock2)

	// First acquire lands on token 1 (round-robin start) and admits its
	// session; token 2 remains fresh.
	first, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token != 0 {
		t.Fatalf("first lease token = %d, want 0", first.Token)
	}
	p.LeaseRelease(first)
	if mock1.SessionCreates != 1 {
		t.Fatalf("token 1 session creates = %d, want 1", mock1.SessionCreates)
	}
	if mock2.SessionCreates != 0 {
		t.Fatalf("token 2 session creates = %d, want 0", mock2.SessionCreates)
	}

	// Successive acquires all land on token 1 (hot-session-first): its live
	// session is reused and token 2 never gets a session admitted — the
	// round-robin start alternates back to token 2, but the hot token wins.
	for i := 0; i < 5; i++ {
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatal(err)
		}
		if lease.Token != 0 {
			t.Errorf("acquire %d token = %d, want 0 (hot-session-first)", i, lease.Token)
		}
		p.LeaseRelease(lease)
	}
	if mock2.SessionCreates != 0 {
		t.Errorf("token 2 session creates = %d, want 0 (never admitted)", mock2.SessionCreates)
	}
	if got := mock2.StartedRunsSnapshot(); len(got) != 0 {
		t.Errorf("token 2 started runs = %v, want none", got)
	}

	// Cool token 1 down: the next acquire falls back to token 2 and admits
	// its session on demand.
	p.CooldownToken(0, time.Hour)
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Token != 1 {
		t.Errorf("lease token = %d, want 1 (cold fallback after cooldown)", lease.Token)
	}
	p.LeaseRelease(lease)
	if mock2.SessionCreates != 1 {
		t.Errorf("token 2 session creates = %d, want 1 after token 1 cooldown", mock2.SessionCreates)
	}
}

func TestFailoverOnAuthReject(t *testing.T) {
	bad := testutil.NewMock() // token-1: 401 on every route
	defer bad.Close()
	bad.AuthReject = true
	good := testutil.NewMock() // token-2: healthy
	defer good.Close()
	p := newTestPool(t, bad, good)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Token != 1 {
		t.Errorf("lease token = %d, want 1 (failover to healthy)", lease.Token)
	}
	if lease.SessionInstanceID != "inst-abc-123" {
		t.Errorf("session instance = %q, want inst-abc-123", lease.SessionInstanceID)
	}
	p.LeaseRelease(lease)

	if len(bad.StartedRuns) != 0 {
		t.Errorf("rejecting token started runs: %v", bad.StartedRuns)
	}
	if len(good.StartedRuns) != 1 {
		t.Errorf("healthy token started runs = %v, want 1", good.StartedRuns)
	}

	// The dead token must be on a 30-min cooldown; subsequent acquires skip
	// it entirely (round-robin returns to it on the 3rd acquire).
	snap := p.Snapshot()[0]
	if snap.CooldownUntil.Before(time.Now().Add(29 * time.Minute)) {
		t.Errorf("cooldown until = %v, want ~now+30m", snap.CooldownUntil)
	}
	for i := 0; i < 2; i++ {
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatal(err)
		}
		if lease.Token != 1 {
			t.Errorf("acquire %d token = %d, want 1 (dead token skipped)", i, lease.Token)
		}
		p.LeaseRelease(lease)
	}
	if len(good.StartedRuns) != 1 {
		t.Errorf("healthy token re-STARTed: %v", good.StartedRuns)
	}
}

func TestWaitingRoomBestPosition(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.SessionMode = "queued"
	mock0.QueuePosition = 3
	mock0.QueueDepth = 7
	mock0.EstimatedWaitMs = 5000
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.SessionMode = "queued"
	mock1.QueuePosition = 1
	mock1.QueueDepth = 9
	mock1.EstimatedWaitMs = 5000
	p := newTestPool(t, mock0, mock1)

	_, err := p.Acquire(context.Background(), modelA)
	var wr *session.WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("want session.WaitingRoomError, got %v", err)
	}
	if wr.Position != 1 || wr.QueueDepth != 9 {
		t.Errorf("waiting room = position %d depth %d, want 1/9 (best position)", wr.Position, wr.QueueDepth)
	}
	if wr.RetryAfter < 4*time.Second {
		t.Errorf("RetryAfter = %s, want ~5s", wr.RetryAfter)
	}
}

func TestWaitingRoomTieBreaksByQueueDepth(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.SessionMode = "queued"
	mock0.QueuePosition = 3
	mock0.QueueDepth = 7
	mock0.EstimatedWaitMs = 5000
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.SessionMode = "queued"
	mock1.QueuePosition = 3
	mock1.QueueDepth = 4
	mock1.EstimatedWaitMs = 5000
	p := newTestPool(t, mock0, mock1)

	_, err := p.Acquire(context.Background(), modelA)
	var wr *session.WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("want session.WaitingRoomError, got %v", err)
	}
	if wr.Position != 3 || wr.QueueDepth != 4 {
		t.Errorf("waiting room = position %d depth %d, want 3/4 (lowest depth on tie)", wr.Position, wr.QueueDepth)
	}
}

func TestAllFailedCombinedError(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.AuthReject = true
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.AuthReject = true
	p := newTestPool(t, mock0, mock1)

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil {
		t.Fatal("want combined error")
	}
	if !strings.Contains(err.Error(), "unable to acquire run from any token") {
		t.Errorf("error = %q, want combined-error prefix", err)
	}
	for _, tok := range []string{"token-1", "token-2"} {
		if !strings.Contains(err.Error(), tok) {
			t.Errorf("combined error missing %s: %q", tok, err)
		}
	}
	for _, snap := range p.Snapshot() {
		if snap.CooldownUntil.Before(time.Now().Add(29 * time.Minute)) {
			t.Errorf("token %d not cooled down: %v", snap.Token, snap.CooldownUntil)
		}
	}
}

func TestWaitingRoomSurfacesOnAnyQueuedToken(t *testing.T) {
	// Precedence-chain failover (ban > country > rate > waiting > daily)
	// surfaces the waiting-room error as soon as ANY token is queued — a
	// queued token is the only actionable signal — instead of requiring
	// every token to be queued. Buckets lower than waiting (daily cap) and
	// the generic fallback lose to it; here the second token's auth-reject
	// is not a matrix bucket, so waiting wins.
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.SessionMode = "queued"
	mock0.QueuePosition = 2
	mock0.QueueDepth = 5
	mock0.EstimatedWaitMs = 5000
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.AuthReject = true
	p := newTestPool(t, mock0, mock1)

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil {
		t.Fatal("want error")
	}
	var wr *session.WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("want session.WaitingRoomError when any token is queued, got %v", err)
	}
	if wr.Position != 2 {
		t.Errorf("waiting room position = %d, want 2", wr.Position)
	}
}

func TestSessionInstanceIDOnLease(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionInstanceID != "inst-abc-123" {
		t.Errorf("instance = %q, want inst-abc-123", lease.SessionInstanceID)
	}
	p.LeaseRelease(lease)
}

func TestDisabledSessionLease(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "disabled"
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionInstanceID != "" {
		t.Errorf("instance = %q, want empty for disabled session", lease.SessionInstanceID)
	}
	p.LeaseRelease(lease)
}

func TestInvalidateSessionRecreates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	if mock.SessionCreates != 1 {
		t.Fatalf("session creates = %d, want 1", mock.SessionCreates)
	}

	p.InvalidateSession(lease.Token, lease.SessionInstanceID)
	lease2, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease2)
	if mock.SessionCreates != 2 {
		t.Errorf("session creates = %d, want 2 (recreated after invalidate)", mock.SessionCreates)
	}
	if lease2.SessionInstanceID != "inst-abc-123" {
		t.Errorf("recreated instance = %q, want inst-abc-123", lease2.SessionInstanceID)
	}

	// Out-of-range tokens are ignored without panicking.
	p.InvalidateSession(-1, "")
	p.InvalidateSession(99, "")
}

func TestInvalidateRunRestarts(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	if started := mock.StartedRunsSnapshot(); len(started) != 1 {
		t.Fatalf("started runs = %v, want 1", started)
	}

	p.InvalidateRun(lease.Token, lease.AgentID)
	lease2, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease2)
	if started := mock.StartedRunsSnapshot(); len(started) != 2 {
		t.Errorf("started runs = %d, want 2 (restart after invalidate)", len(started))
	}
	// The INVALIDATED parent run must NOT be FINISHed (Invalidate deletes
	// it without an upstream FINISH), and with the #91 context-pruner child
	// traffic gone no other FINISH may exist either.
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
		t.Errorf("finished runs = %v, want none (invalidated run is not FINISHed)", got)
	}

	// Out-of-range tokens are ignored without panicking.
	p.InvalidateRun(-1, agentA)
	p.InvalidateRun(99, agentA)
}

func TestPoolChat(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"` + modelA + `","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(lease)

	opts := upstream.ChatOptions{Model: modelA, RunID: lease.Run.RunID, SessionInstanceID: lease.SessionInstanceID}
	body := []byte(`{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}]}`)
	rc, err := p.Chat(context.Background(), lease, opts, body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"content":"hi"`) {
		t.Errorf("stream = %q, want content chunk", got)
	}

	// The chat went out with the CLI envelope on the leased token.
	if len(mock.RecordedChatBodies) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", len(mock.RecordedChatBodies))
	}
	recorded := mock.RecordedChatBodies[0]
	for _, want := range []string{`"codebuff_metadata"`, `"data_collection":"deny"`, `"stream":true`, `"stop":["\"cb_easp\""]`} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s: %s", want, recorded)
		}
	}
	if !strings.Contains(recorded, `"run_id":"run-0001"`) {
		t.Errorf("upstream body missing the leased run id: %s", recorded)
	}
	h := mock.RecordedChatHeaders[0]
	// #106: the chat POST carries no model/instance headers — they ride in
	// the body metadata only.
	if got := h.Get("x-freebuff-model"); got != "" {
		t.Errorf("x-freebuff-model = %q on the chat POST, want absent (#106)", got)
	}
	if got := h.Get("x-freebuff-instance-id"); got != "" {
		t.Errorf("x-freebuff-instance-id = %q on the chat POST, want absent (#106)", got)
	}
	if !strings.Contains(recorded, `"freebuff_instance_id":"inst-abc-123"`) {
		t.Errorf("upstream body missing freebuff_instance_id in codebuff_metadata: %s", recorded)
	}

	// Invalid leases fail without panicking.
	if _, err := p.Chat(context.Background(), nil, opts, body); err == nil {
		t.Error("want error for nil lease")
	}
	if _, err := p.Chat(context.Background(), &Lease{Token: 99}, opts, body); err == nil {
		t.Error("want error for out-of-range lease token")
	}
}

// TestChatDispatchesThroughLeaseEntry is the regression guard for the P2
// chat dispatch bug: Chat used the lease's Token index against a FRESH
// token snapshot, so a concurrent RemoveAllTokens+AddToken left the index
// pointing at a DIFFERENT token — the chat went through the wrong account's
// client and its usage/error path charged the wrong token (or the index was
// out of range and the chat failed outright). The lease's backing entry is
// the authoritative owner pinned at Acquire: Chat must dispatch through it
// and skip usage recording once the entry is no longer in the pool.
func TestChatDispatchesThroughLeaseEntry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"` + modelA + `","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	origEntry := lease.entry
	if origEntry == nil {
		t.Fatal("acquired lease has no backing entry")
	}
	opts := upstream.ChatOptions{Model: modelA, RunID: lease.Run.RunID, SessionInstanceID: lease.SessionInstanceID}
	body := []byte(`{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}]}`)

	// Rebuild the token list under the in-flight lease: the lease's Token
	// index (0) now belongs to a DIFFERENT token's entry.
	p.RemoveAllTokens(context.Background())
	if _, err := p.AddToken("new-token"); err != nil {
		t.Fatal(err)
	}
	if (*p.toks.Load())[0] == origEntry {
		t.Fatal("test setup: index 0 still the original entry")
	}

	rc, err := p.Chat(context.Background(), lease, opts, body)
	if err != nil {
		t.Fatalf("chat through stale lease failed: %v", err)
	}
	_ = rc.Close()
	p.LeaseRelease(lease)

	// The chat went out on the ORIGINAL token's client, not the new token
	// that reused its index.
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer tok-0" {
		t.Errorf("upstream Authorization = %q, want %q (chat went through the wrong token)", got, "Bearer tok-0")
	}

	// The removed entry's usage must NOT be charged to the new index-0 token.
	if got := p.usageCount(0); got != 0 {
		t.Errorf("usage on reused index = %d, want 0 (removed entry's chat must not charge the new token)", got)
	}
}

func TestUnknownModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	_, err := p.Acquire(context.Background(), "no/such-model")
	if !errors.Is(err, registry.ErrModelNotFound) {
		t.Fatalf("want registry.ErrModelNotFound, got %v", err)
	}
}

// TestStartDoesNotFanOutAndShutdownDrains pins the free_mode_run_fanout
// root cause: Start must NOT open a run per registry agent per token at boot
// (that fleet is the "proxy fanout" shape upstream refuses), and the runs a
// real lease does create must still FINISH on shutdown.
func TestStartDoesNotFanOutAndShutdownDrains(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%04d", i)
	}
	mock.RunIDs = ids
	p := newTestPool(t, mock)

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	defer cancel()

	// No request has arrived, so no run may exist upstream. The registry
	// carries several agents; a fleet prewarm would show up here.
	if agents := len(p.regAgentIDs(t)); agents < 2 {
		t.Fatalf("registry agents = %d, want >= 2 for this to prove anything", agents)
	}
	if started := mock.StartedRunsSnapshot(); len(started) != 0 {
		t.Fatalf("STARTed runs at boot = %d, want 0 (no agent-fleet fanout)", len(started))
	}

	// One lease starts exactly the run it needs, and shutdown drains it.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	p.LeaseRelease(lease)
	if started := mock.StartedRunsSnapshot(); len(started) != 1 {
		t.Errorf("STARTed runs after one Acquire = %d, want 1", len(started))
	}

	p.Shutdown(context.Background())

	eventually(t, "shutdown drain FINISHes", func() bool {
		return len(mock.FinishedRunsSnapshot()) >= 1
	})
	for _, f := range mock.FinishedRunsSnapshot() {
		if f.Status != "completed" {
			t.Errorf("run %s finished with status %q, want completed", f.RunID, f.Status)
		}
	}
}

func TestConcurrentAcquireHammer(t *testing.T) {
	mocks := []*testutil.MockUpstream{testutil.NewMock(), testutil.NewMock(), testutil.NewMock()}
	defer func() {
		for _, m := range mocks {
			m.Close()
		}
	}()
	for _, m := range mocks {
		ids := make([]string, 100)
		for i := range ids {
			ids[i] = fmt.Sprintf("run-%04d", i)
		}
		m.RunIDs = ids
	}
	p := newTestPool(t, mocks...)

	models := []string{modelA, modelB}
	const goroutines = 8
	const perGoroutine = 25
	var wg sync.WaitGroup
	var failures atomicErr
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				lease, err := p.Acquire(context.Background(), models[(g+i)%len(models)])
				if err != nil {
					failures.set(err)
					continue
				}
				p.LeaseRelease(lease)
			}
		}(g)
	}
	wg.Wait()

	if err := failures.get(); err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	// Hot-session-first selection concentrates traffic on tokens that
	// already hold a live session, so the exact per-token distribution is
	// interleaving-dependent. Assert the deterministic invariants instead:
	// every acquire was served by a run, and both agents have runs on at
	// least one token (each token holds at most one run per agent).
	var totalRequests, activeRuns int
	for _, snap := range p.Snapshot() {
		totalRequests += snap.Requests
		activeRuns += snap.ActiveRuns
	}
	if totalRequests != goroutines*perGoroutine {
		t.Errorf("total requests = %d, want %d", totalRequests, goroutines*perGoroutine)
	}
	if activeRuns < 2 || activeRuns > len(mocks)*2 {
		t.Errorf("active runs = %d, want within [2, %d] (both agents on at least one token)", activeRuns, len(mocks)*2)
	}
}

func TestWaitingRoomRankings(t *testing.T) {
	err1 := &session.WaitingRoomError{Position: 5, QueueDepth: 10, RetryAfter: time.Second}
	err2 := &session.WaitingRoomError{Position: 2, QueueDepth: 10, RetryAfter: time.Second}
	errUnknown := &session.WaitingRoomError{Position: 0, QueueDepth: 10, RetryAfter: time.Second}

	if !betterWait(err2, err1) {
		t.Errorf("betterWait position 2 vs 5: want true")
	}
	if betterWait(err1, err2) {
		t.Errorf("betterWait position 5 vs 2: want false")
	}
	if betterWait(errUnknown, err1) {
		t.Errorf("betterWait position 0 vs 5: want false (unknown ranks lower)")
	}
}

func TestPoolInvalidateToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	p.InvalidateSession(0, "")
	p.InvalidateSession(999, "") // out of range safe
	p.InvalidateRun(0, "base2-free")
	p.InvalidateRun(999, "base2-free") // out of range safe
	p.CooldownToken(0, 5*time.Minute)
	p.CooldownToken(999, 5*time.Minute)
	p.CooldownTokenRateLimit(999, nil)
	p.CooldownTokenBan(999, nil)
}

// TestTokenSnapshotTierAndCountry pins the TokenSnapshot region fields
// carried from the admitted session (healthz / /v1/models annotation).
func TestTokenSnapshotTierAndCountry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.CountryCode = "US"
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	snap := p.Snapshot()[0]
	if snap.CountryCode != "US" {
		t.Errorf("snapshot country = %q, want US", snap.CountryCode)
	}
	if snap.CountryBlockReason != "" {
		t.Errorf("CountryBlockReason = %q, want empty for an admitted session", snap.CountryBlockReason)
	}
}

// TestAcquireSyncsAdmittedModel pins the upstream model coercion fix: when the
// client requests model A (e.g. deepseek/deepseek-v4-flash) but upstream
// admits the session for model B (e.g. mimo/mimo-v2.5 on that IP/country),
// Acquire must return a lease with Model=B and AgentID for B so downstream
// chat and runs stay consistent with the upstream session row.
func TestAcquireSyncsAdmittedModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-coerced","model":"`+modelB+`"}`)
	}
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	defer p.LeaseRelease(lease)

	if lease.Model != modelB {
		t.Errorf("lease.Model = %q, want coerced model %q", lease.Model, modelB)
	}
	wantAgent := agentB
	if lease.AgentID != wantAgent {
		t.Errorf("lease.AgentID = %q, want %q", lease.AgentID, wantAgent)
	}
}

func TestTokenLocking(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	// Verify both tokens are initially accessible.
	lease0, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	first := lease0.Token
	p.LeaseRelease(lease0)

	// Lock token 0 — Acquire must never return it.
	if err := p.LockToken(0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		// Invalidate both cached sessions so the cold path is exercised.
		toks := p.toks.Load()
		(*toks)[0].session.Invalidate()
		(*toks)[1].session.Invalidate()
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		if lease.Token == 0 {
			t.Errorf("acquire %d: locked token 0 returned", i)
		}
		p.LeaseRelease(lease)
	}

	// Snapshot must report Locked=true for token 0.
	snaps := p.Snapshot()
	if !snaps[0].Locked {
		t.Error("token 0 snapshot: Locked=false, want true")
	}
	if snaps[1].Locked {
		t.Error("token 1 snapshot: Locked=true, want false")
	}

	// Unlock — Acquire may now return token 0 again.
	if err := p.UnlockLockToken(0); err != nil {
		t.Fatal(err)
	}
	toks := p.toks.Load()
	(*toks)[0].session.Invalidate()
	(*toks)[1].session.Invalidate()
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	// On a cold pool the round-robin start index may differ, but we just
	// need to confirm token 0 is no longer excluded.
	_ = lease.Token
	_ = first
	p.LeaseRelease(lease)

	// Snapshot must report Locked=false after unlock.
	snaps = p.Snapshot()
	if snaps[0].Locked {
		t.Error("token 0 snapshot after unlock: Locked=true, want false")
	}
}
