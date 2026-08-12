package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// Test models must map to agents with EXCLUSIVE ownership in the registry
// FALLBACK map (see internal/registry/registry_test.go expectedFallback):
// the five base2-free models are first-seen-assigned to the generic
// base2-free agent, while glm-5.2 and laguna-s-2.1 are owned by their
// dedicated one-model agents. Tests pin the offline (fallback) state.
const (
	modelA = "z-ai/glm-5.2"
	modelB = "poolside/laguna-s-2.1"
	agentA = "base2-free-glm"
	agentB = "base2-free-laguna-s-2-1"
)

// newTestPool wires one mock upstream per token through real clients and
// session managers, backed by the registry fallback map.
func newTestPool(t *testing.T, mocks ...*testutil.MockUpstream) *Pool {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         make([]string, len(mocks)),
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	clients := make([]*upstream.Client, 0, len(mocks))
	sessions := make([]*session.Manager, 0, len(mocks))
	for i, mock := range mocks {
		cfg.AuthTokens[i] = fmt.Sprintf("tok-%d", i)
		clientCfg := *cfg
		clientCfg.UpstreamBaseURL = mock.URL()
		client, err := upstream.New(cfg.AuthTokens[i], &clientCfg)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManager(client))
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := New(cfg, clients, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

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

	const n = 6
	got := make([]int, n)
	for i := 0; i < n; i++ {
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
	// Both tokens created the run for the agent exactly once.
	for i, mock := range []*testutil.MockUpstream{mock0, mock1} {
		if len(mock.StartedRuns) != 1 || mock.StartedRuns[0] != agentA {
			t.Errorf("mock%d started runs = %v, want [%s]", i, mock.StartedRuns, agentA)
		}
		if len(mock.FinishedRuns) != 0 {
			t.Errorf("mock%d finished runs = %v, want none", i, mock.FinishedRuns)
		}
	}

	snaps := p.Snapshot()
	for i, snap := range snaps {
		if snap.ActiveRuns != 1 || snap.Requests != 3 {
			t.Errorf("token %d snapshot: active=%d requests=%d, want 1/3", i, snap.ActiveRuns, snap.Requests)
		}
		if snap.SessionStatus != "active" || snap.SessionInstanceID != "inst-abc-123" {
			t.Errorf("token %d session snapshot = %q/%q", i, snap.SessionStatus, snap.SessionInstanceID)
		}
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

func TestWaitingRoomOnlyWhenEveryTokenQueued(t *testing.T) {
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
	if errors.As(err, &wr) {
		t.Fatalf("waiting-room error surfaced although only one token is queued: %v", err)
	}
	if !strings.Contains(err.Error(), "unable to acquire run from any token") {
		t.Errorf("error = %q, want combined error", err)
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

	p.InvalidateSession(lease.Token)
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
	p.InvalidateSession(-1)
	p.InvalidateSession(99)
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
	if len(mock.StartedRuns) != 1 {
		t.Fatalf("started runs = %v, want 1", mock.StartedRuns)
	}

	p.InvalidateRun(lease.Token, lease.AgentID)
	lease2, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease2)
	if len(mock.StartedRuns) != 2 {
		t.Errorf("started runs = %d, want 2 (restart after invalidate)", len(mock.StartedRuns))
	}
	if len(mock.FinishedRuns) != 0 {
		t.Errorf("finished runs = %v, want none (invalidated run is not FINISHed)", mock.FinishedRuns)
	}

	// Out-of-range tokens are ignored without panicking.
	p.InvalidateRun(-1, agentA)
	p.InvalidateRun(99, agentA)
}

func TestCooldownToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	p.CooldownToken(0, time.Hour)
	snap := p.Snapshot()[0]
	if snap.CooldownUntil.Before(time.Now().Add(59 * time.Minute)) {
		t.Errorf("cooldown until = %v, want ~now+1h", snap.CooldownUntil)
	}

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil {
		t.Fatal("want error while the only token is cooling down")
	}
	if !strings.Contains(err.Error(), "cooling down") {
		t.Errorf("error = %q, want cooldown message", err)
	}

	// Out-of-range tokens are ignored without panicking.
	p.CooldownToken(99, time.Hour)
	p.CooldownToken(-1, time.Hour)
}

func TestAcquireRateLimitCooldowns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimit = true
	p := newTestPool(t, mock)

	_, err := p.Acquire(context.Background(), modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want *upstream.RateLimitError, got %v", err)
	}
	if rle.RetryAfter != 48549499*time.Millisecond {
		t.Errorf("RetryAfter = %s, want 48549499ms", rle.RetryAfter)
	}
	if rle.Limit != 6 || rle.RecentCount != 6.6 {
		t.Errorf("quota = %v/%v, want 6/6.6", rle.RecentCount, rle.Limit)
	}

	// The token cooled down for the upstream retry window, so subsequent
	// acquires skip it AND still surface the remembered 429 (not a generic
	// combined error) — the client keeps getting Retry-After.
	snap := p.Snapshot()[0]
	if snap.CooldownUntil.Before(time.Now().Add(13 * time.Hour)) {
		t.Errorf("cooldown until = %v, want ~now+13.5h", snap.CooldownUntil)
	}
	_, err = p.Acquire(context.Background(), modelA)
	var rle2 *upstream.RateLimitError
	if !errors.As(err, &rle2) {
		t.Fatalf("second acquire: want *upstream.RateLimitError, got %v", err)
	}
	if rle2.RetryAfter != 48549499*time.Millisecond {
		t.Errorf("second acquire RetryAfter = %s, want 48549499ms (remembered)", rle2.RetryAfter)
	}
}

func TestAcquireRateLimitBestWindow(t *testing.T) {
	// Both tokens rate-limited with different windows: the pool surfaces the
	// longest one (the token that unblocks last bounds the wait).
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.RateLimit = true
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.RateLimit = true
	p := newTestPool(t, mock0, mock1)

	_, err := p.Acquire(context.Background(), modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want *upstream.RateLimitError, got %v", err)
	}
	if rle.RetryAfter != 48549499*time.Millisecond {
		t.Errorf("RetryAfter = %s, want 48549499ms (best window)", rle.RetryAfter)
	}
	if err.Error() == "" || !strings.Contains(err.Error(), "upstream rate limited") {
		t.Errorf("error = %q, want rate-limit message", err)
	}
}

func TestAcquireBanCooldowns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true
	p := newTestPool(t, mock)

	_, err := p.Acquire(context.Background(), modelA)
	var be *upstream.BanError
	if !errors.As(err, &be) {
		t.Fatalf("want *upstream.BanError, got %v", err)
	}
	if !errors.Is(err, upstream.ErrBanned) {
		t.Errorf("errors.Is(ErrBanned) = false")
	}

	// The token cooled down for the ban window, so subsequent acquires skip
	// it AND still surface the remembered 403 banned.
	snap := p.Snapshot()[0]
	if snap.CooldownUntil.Before(time.Now().Add(59 * time.Minute)) {
		t.Errorf("cooldown until = %v, want ~now+1h", snap.CooldownUntil)
	}
	_, err = p.Acquire(context.Background(), modelA)
	var be2 *upstream.BanError
	if !errors.As(err, &be2) {
		t.Fatalf("second acquire: want *upstream.BanError, got %v", err)
	}
	if !errors.Is(err, upstream.ErrBanned) {
		t.Errorf("second acquire errors.Is(ErrBanned) = false")
	}
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
	for _, want := range []string{`"codebuff_metadata"`, `"data_collection":"deny"`, `"stream":true`, `"stop":["cb_easp"]`} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s: %s", want, recorded)
		}
	}
	if !strings.Contains(recorded, `"run_id":"run-0001"`) {
		t.Errorf("upstream body missing the leased run id: %s", recorded)
	}
	h := mock.RecordedChatHeaders[0]
	if got := h.Get("x-freebuff-model"); got != modelA {
		t.Errorf("x-freebuff-model = %q, want %q", got, modelA)
	}
	if got := h.Get("x-freebuff-instance-id"); got != "inst-abc-123" {
		t.Errorf("x-freebuff-instance-id = %q, want inst-abc-123", got)
	}

	// Invalid leases fail without panicking.
	if _, err := p.Chat(context.Background(), nil, opts, body); err == nil {
		t.Error("want error for nil lease")
	}
	if _, err := p.Chat(context.Background(), &Lease{Token: 99}, opts, body); err == nil {
		t.Error("want error for out-of-range lease token")
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

func TestStartPrewarmsAndShutdownDrains(t *testing.T) {
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

	// Prewarm runs in the background: wait until every registry agent has a
	// STARTed run.
	agentCount := len(p.regAgentIDs(t))
	eventually(t, "prewarm of all agents", func() bool {
		return len(mock.StartedRunsSnapshot()) >= agentCount
	})

	p.Shutdown(context.Background())

	eventually(t, "shutdown drain FINISHes", func() bool {
		return len(mock.FinishedRunsSnapshot()) >= agentCount
	})
	for _, f := range mock.FinishedRunsSnapshot() {
		if f.Status != "completed" {
			t.Errorf("run %s finished with status %q, want completed", f.RunID, f.Status)
		}
	}
}

// regAgentIDs re-reads the pool's registry agent list through a fresh
// fallback registry (the pool does not export its registry).
func (p *Pool) regAgentIDs(t *testing.T) []string {
	t.Helper()
	reg := registry.New(p.cfg, nil)
	reg.LoadFallback()
	return reg.AgentIDs()
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
	var totalRequests, activeRuns int
	for _, snap := range p.Snapshot() {
		totalRequests += snap.Requests
		activeRuns += snap.ActiveRuns
	}
	if totalRequests != goroutines*perGoroutine {
		t.Errorf("total requests = %d, want %d", totalRequests, goroutines*perGoroutine)
	}
	if activeRuns != len(mocks)*2 {
		t.Errorf("active runs = %d, want %d (both agents on all tokens)", activeRuns, len(mocks)*2)
	}
}

// chatOnce sends one chat through the leased token against the mock
// upstream and closes the body; used to accumulate successful chats for the
// daily-cap tests.
func chatOnce(t *testing.T, p *Pool, lease *Lease) {
	t.Helper()
	opts := upstream.ChatOptions{Model: modelA, RunID: lease.Run.RunID, SessionInstanceID: lease.SessionInstanceID}
	body := []byte(`{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}]}`)
	rc, err := p.Chat(context.Background(), lease, opts, body)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
}

func TestDailyMessageCap(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	p.cfg.MaxMessagesPerDay = 2

	for i := 0; i < 2; i++ {
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatal(err)
		}
		chatOnce(t, p, lease)
		p.LeaseRelease(lease)
	}
	if got := p.Snapshot()[0].Messages24h; got != 2 {
		t.Errorf("Messages24h = %d, want 2", got)
	}

	// The third acquire hits the cap: the only token is daily-limited, so
	// the pool surfaces a 429 with the time until a slot frees.
	_, err := p.Acquire(context.Background(), modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want *upstream.RateLimitError for capped token, got %v", err)
	}
	if !errors.Is(err, upstream.ErrRateLimited) {
		t.Error("errors.Is(ErrRateLimited) = false")
	}
	if rle.RetryAfter <= 0 || rle.RetryAfter > usageWindow {
		t.Errorf("RetryAfter = %s, want within (0, 24h]", rle.RetryAfter)
	}
	if rle.Limit != 2 || rle.RecentCount != 2 {
		t.Errorf("quota = %v/%v, want 2/2", rle.RecentCount, rle.Limit)
	}
	if !strings.Contains(err.Error(), "daily message limit reached") {
		t.Errorf("error = %q, want daily-limit message", err)
	}
	if got := p.Snapshot()[0].Messages24h; got != 2 {
		t.Errorf("Messages24h = %d, want 2 (usage still visible)", got)
	}
}

func TestDailyMessageCapFailover(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)
	p.cfg.MaxMessagesPerDay = 1

	// Round-robin: first acquire lands on token-1; cap it with a chat.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Token != 0 {
		t.Fatalf("first lease token = %d, want 0", lease.Token)
	}
	chatOnce(t, p, lease)
	p.LeaseRelease(lease)

	// Second acquire fails over to token-2 (token-1 is capped); cap it too.
	lease, err = p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Token != 1 {
		t.Fatalf("second lease token = %d, want 1 (failover to uncapped)", lease.Token)
	}
	chatOnce(t, p, lease)
	p.LeaseRelease(lease)

	// Both tokens capped: the pool surfaces the daily-limit 429 (not a
	// combined error).
	_, err = p.Acquire(context.Background(), modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want *upstream.RateLimitError when every token is capped, got %v", err)
	}
	if rle.RetryAfter <= 0 || rle.RetryAfter > usageWindow {
		t.Errorf("RetryAfter = %s, want within (0, 24h]", rle.RetryAfter)
	}
}

func TestDailyMessageCapDisabled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock) // MaxMessagesPerDay = 0: unlimited

	for i := 0; i < 3; i++ {
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatal(err)
		}
		chatOnce(t, p, lease)
		p.LeaseRelease(lease)
	}
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("acquire with cap 0 must not be limited, got %v", err)
	}
	p.LeaseRelease(lease)
	if got := p.Snapshot()[0].Messages24h; got != 3 {
		t.Errorf("Messages24h = %d, want 3 (usage still tracked)", got)
	}
}

func TestIdleRotationFinishesRuns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	p.cfg.IdleRotationTimeout = 10 * time.Millisecond

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	if got := mock.StartedRunsSnapshot(); len(got) != 1 {
		t.Fatalf("started runs = %v, want 1", got)
	}

	// Not idle yet: a maintain pass runs normally (no FINISH).
	p.maintainTick(context.Background())
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
		t.Fatalf("finished runs = %v before idle, want none", got)
	}

	// Past the idle threshold: one pass FINISHes all runs...
	time.Sleep(30 * time.Millisecond)
	p.maintainTick(context.Background())
	finished := mock.FinishedRunsSnapshot()
	if len(finished) != 1 || finished[0].Status != "completed" {
		t.Fatalf("finished runs after idle = %v, want 1 completed", finished)
	}

	// ...and later idle passes stay dormant (no further FINISH or START).
	for i := 0; i < 2; i++ {
		p.maintainTick(context.Background())
	}
	if got := mock.FinishedRunsSnapshot(); len(got) != 1 {
		t.Errorf("finished runs = %v, want still 1 (dormant while idle)", got)
	}
	if got := mock.StartedRunsSnapshot(); len(got) != 1 {
		t.Errorf("started runs = %v, want still 1", got)
	}

	// The next request re-creates the run on demand.
	lease, err = p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	if got := mock.StartedRunsSnapshot(); len(got) != 2 {
		t.Errorf("started runs = %v, want 2 (re-created on demand)", got)
	}
}

func TestIdleRotationDisabled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock) // IdleRotationTimeout = 0: never idle-pauses

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	p.maintainTick(context.Background())
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
		t.Fatalf("finished runs = %v with idle rotation disabled, want none", got)
	}
}
func TestBridgeLRUEviction(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ids := make([]string, 40)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%04d", i)
	}
	mock.RunIDs = ids
	p := newBridgePool(t, mock)

	for i := 0; i < 35; i++ {
		token := fmt.Sprintf("bridge-token-%d", i)
		lease, err := p.AcquireBridge(context.Background(), token, modelA)
		if err != nil {
			t.Fatalf("AcquireBridge token %d failed: %v", i, err)
		}
		p.LeaseRelease(lease)
	}

	if count := p.BridgeCount(); count > 32 {
		t.Errorf("BridgeCount = %d, want <= 32 (LRU eviction)", count)
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

func TestPoolCooldownRateLimitAndBan(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	rle := &upstream.RateLimitError{Body: "rate limit", RetryAfter: 10 * time.Minute}
	p.CooldownTokenRateLimit(0, rle)

	be := &upstream.BanError{Body: "account banned", ResumesAt: time.Now().Add(1 * time.Hour)}
	p.CooldownTokenBan(0, be)

	snap := p.Snapshot()[0]
	if snap.RiskLevel != "critical" && snap.RiskLevel != "high" {
		t.Errorf("RiskLevel = %q, want high or critical", snap.RiskLevel)
	}
}

func TestBridgeInvalidationAndCooldowns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	lease, err := p.AcquireBridge(context.Background(), "bridge-tok-1", modelA)
	if err != nil {
		t.Fatal(err)
	}

	p.InvalidateBridgeSession(lease)
	p.InvalidateBridgeRun(lease, lease.AgentID)
	p.CooldownBridge(lease, 5*time.Minute)
	p.CooldownBridgeRateLimit(lease, &upstream.RateLimitError{Body: "rate limit", RetryAfter: 5 * time.Minute})
	p.CooldownBridgeBan(lease, &upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(5 * time.Minute)})

	p.LeaseRelease(lease)
}

func TestPoolInvalidateToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	p.InvalidateSession(0)
	p.InvalidateSession(999) // out of range safe
	p.InvalidateRun(0, "base2-free")
	p.InvalidateRun(999, "base2-free") // out of range safe
	p.CooldownToken(0, 5*time.Minute)
	p.CooldownToken(999, 5*time.Minute)
	p.CooldownTokenRateLimit(999, nil)
	p.CooldownTokenBan(999, nil)
}

func TestMultiTokenRateLimitAndBanFailover(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	p := newTestPool(t, mock0, mock1)

	rle := &upstream.RateLimitError{Body: "rate limit", RetryAfter: 10 * time.Minute}
	be := &upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(1 * time.Hour)}

	p.CooldownTokenRateLimit(0, rle)
	p.CooldownTokenRateLimit(1, rle)

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil || !errors.Is(err, upstream.ErrRateLimited) {
		t.Errorf("Acquire with all rate limited = %v, want rate limit error", err)
	}

	p.CooldownTokenBan(0, be)
	p.CooldownTokenBan(1, be)

	_, err = p.Acquire(context.Background(), modelA)
	if err == nil || !errors.Is(err, upstream.ErrBanned) {
		t.Errorf("Acquire with all banned = %v, want ban error", err)
	}
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// atomicErr is a thread-safe first-error holder for the hammer.
type atomicErr struct {
	mu  sync.Mutex
	err error
}

func (e *atomicErr) set(err error) {
	e.mu.Lock()
	if e.err == nil {
		e.err = err
	}
	e.mu.Unlock()
}

func (e *atomicErr) get() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// --- bridge mode ---

// newBridgePool wires a pool in bridge mode (no AUTH_TOKENS) whose lazily
// created per-client-token clients talk to the given mock upstream.
func newBridgePool(t *testing.T, mock *testutil.MockUpstream) *Pool {
	t.Helper()
	cfg := &config.Config{
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBridgeAcquireReusesEntry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	const clientToken = "client-tok-1"
	lease1, err := p.AcquireBridge(context.Background(), clientToken, modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease1.Token != -1 {
		t.Errorf("bridge lease token = %d, want -1", lease1.Token)
	}
	if lease1.Bridge == nil {
		t.Fatal("bridge lease missing Bridge entry")
	}
	if lease1.SessionInstanceID != "inst-abc-123" {
		t.Errorf("instance = %q, want inst-abc-123", lease1.SessionInstanceID)
	}
	p.LeaseRelease(lease1)

	lease2, err := p.AcquireBridge(context.Background(), clientToken, modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease2)

	// One entry per client token: the second acquire reused the first.
	if got := p.bridgeLen(); got != 1 {
		t.Errorf("bridge entries = %d, want 1 (reused)", got)
	}
	if entry := p.bridgeToken(clientToken); entry == nil {
		t.Fatal("bridge entry missing after two acquires")
	}
	// The shared entry started the run and created the session exactly once.
	if got := mock.StartedRunsSnapshot(); len(got) != 1 {
		t.Errorf("started runs = %v, want 1 (single entry reused)", got)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("session creates = %d, want 1 (single entry reused)", mock.SessionCreates)
	}

	// Chat through the bridge lease goes out with the CLIENT's token.
	mock.ChatBody = testutil.SSEEvent(`{"id":"chatcmpl-b1","object":"chat.completion.chunk","created":1,"model":"` + modelA + `","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
	opts := upstream.ChatOptions{Model: modelA, RunID: lease2.Run.RunID, SessionInstanceID: lease2.SessionInstanceID}
	body := []byte(`{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}]}`)
	rc, err := p.Chat(context.Background(), lease2, opts, body)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer "+clientToken {
		t.Errorf("upstream Authorization = %q, want %q", got, "Bearer "+clientToken)
	}
}

func TestBridgeEviction(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ids := make([]string, maxBridgeEntries+4)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%04d", i)
	}
	mock.RunIDs = ids
	p := newBridgePool(t, mock)

	// Create more distinct client tokens than the cache cap; the oldest
	// entries must be LRU-evicted.
	for i := 0; i < maxBridgeEntries+2; i++ {
		lease, err := p.AcquireBridge(context.Background(), fmt.Sprintf("client-tok-%02d", i), modelA)
		if err != nil {
			t.Fatal(err)
		}
		p.LeaseRelease(lease)
	}
	if got := p.bridgeLen(); got != maxBridgeEntries {
		t.Errorf("bridge entries = %d, want %d (LRU cap)", got, maxBridgeEntries)
	}
	// The two oldest tokens were evicted; the newest is cached.
	if e := p.bridgeToken("client-tok-00"); e != nil {
		t.Error("oldest bridge entry still cached, want evicted")
	}
	if e := p.bridgeToken("client-tok-01"); e != nil {
		t.Error("second-oldest bridge entry still cached, want evicted")
	}
	if e := p.bridgeToken(fmt.Sprintf("client-tok-%02d", maxBridgeEntries+1)); e == nil {
		t.Error("newest bridge entry not cached")
	}
	// An evicted entry's run was FINISHed best-effort.
	if got := mock.FinishedRunsSnapshot(); len(got) < 2 {
		t.Errorf("finished runs = %d, want >= 2 (evicted entries finished)", len(got))
	}
}

func TestBridgeAcquireEmptyToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	if _, err := p.AcquireBridge(context.Background(), "", modelA); err == nil {
		t.Fatal("want error for empty client token")
	}
	if _, err := p.AcquireBridge(context.Background(), "   ", modelA); err == nil {
		t.Fatal("want error for whitespace-only client token")
	}
	if got := p.bridgeLen(); got != 0 {
		t.Errorf("bridge entries = %d, want 0 (no entry for empty token)", got)
	}
}
