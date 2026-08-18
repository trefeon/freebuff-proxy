package pool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
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
// base2-free agent, while glm-5.2 and claude-fable-5 are owned by their
// dedicated one-model agents. Tests pin the offline (fallback) state.
// (The laguna-s-2.1 pair was retired from the fallback in issue #121.)
const (
	modelA = "z-ai/glm-5.2"
	modelB = "anthropic/claude-fable-5"
	agentA = "base2-free-glm"
	agentB = "base2-free-fable"
)

// newTestPool wires one mock upstream per token through real clients and
// session managers, backed by the registry fallback map.
func newTestPool(t *testing.T, mocks ...*testutil.MockUpstream) *Pool {
	return newTestPoolCfg(t, nil, mocks...)
}

// newTestPoolCfg is newTestPool with a config mutation hook (e.g. enabling
// TRANSIENT_RETRIES / TLS_FINGERPRINT for retry tests).
func newTestPoolCfg(t *testing.T, mut func(*config.Config), mocks ...*testutil.MockUpstream) *Pool {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         make([]string, len(mocks)),
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	if mut != nil {
		mut(cfg)
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

	// Strict round-robin only applies while no token holds a live session:
	// hot-session-first selection routes every acquire to a live session
	// once one exists. Invalidate both cached sessions before each acquire
	// so the cold path is exercised, and assert the historical order is
	// unchanged (selection-order change must not regress cold failover).
	const n = 6
	got := make([]int, n)
	for i := 0; i < n; i++ {
		p.InvalidateSession(0)
		p.InvalidateSession(1)
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
		if len(parentFinished(mock)) != 0 {
			t.Errorf("mock%d finished runs = %v, want none", i, parentFinished(mock))
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

func TestPoolSnapshotQuotaByModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		"z-ai/glm-5.2": map[string]any{
			"model":       "z-ai/glm-5.2",
			"limit":       5,
			"recentCount": 4,
			"period":      "pacific_day",
			"resetAt":     "2026-08-16T07:00:00.000Z",
			"entitlementBreakdown": map[string]any{
				"base":     1,
				"referral": 1,
				"streak":   3,
			},
		},
	}
	p := newTestPool(t, mock)

	// Acquire admits the session (with rateLimitsByModel); the lease is left
	// unreleased so the session cache stays populated for Snapshot().
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	_ = lease

	snaps := p.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snaps))
	}
	q, ok := snaps[0].QuotaByModel["z-ai/glm-5.2"]
	if !ok {
		t.Fatalf("QuotaByModel missing z-ai/glm-5.2: %+v", snaps[0].QuotaByModel)
	}
	if q.Limit != 5 || q.RecentCount != 4 {
		t.Errorf("quota limit/recentCount = %v/%v, want 5/4", q.Limit, q.RecentCount)
	}
	if q.Period != "pacific_day" {
		t.Errorf("period = %q, want pacific_day", q.Period)
	}
	if q.ResetAt.IsZero() {
		t.Error("resetAt not surfaced")
	}
	if q.Entitlement["referral"] != 1 {
		t.Errorf("entitlement = %+v, want referral=1", q.Entitlement)
	}
	if len(snaps[0].Entitlement) != 0 {
		t.Errorf("top-level Entitlement = %+v, want empty", snaps[0].Entitlement)
	}
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
	// Issue #91: run STARTs create+FINISH a context-pruner child run
	// (best-effort, async), so FinishedRuns may contain child-run FINISHes.
	// The assertion that matters: the INVALIDATED parent run must NOT be
	// FINISHed (Invalidate deletes it without an upstream FINISH) — filter
	// out child-run entries for a race-stable check.
	for _, f := range mock.FinishedRunsSnapshot() {
		if strings.HasPrefix(f.RunID, "child-run-") {
			continue
		}
		t.Errorf("finished runs = %v, want no parent FINISH (invalidated run is not FINISHed)", mock.FinishedRunsSnapshot())
		break
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
	// Both tokens rate-limited with DIFFERENT windows (per-mock
	// retryAfterMs): the pool surfaces the shortest one — the token that
	// unblocks earliest bounds the wait.
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.RateLimit = true
	mock0.RateLimitRetryAfterMs = 60000 // 1m window
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.RateLimit = true
	mock1.RateLimitRetryAfterMs = 300000 // 5m window
	p := newTestPool(t, mock0, mock1)

	_, err := p.Acquire(context.Background(), modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want *upstream.RateLimitError, got %v", err)
	}
	if rle.RetryAfter != 1*time.Minute {
		t.Errorf("RetryAfter = %s, want 1m (shortest window wins)", rle.RetryAfter)
	}
	if err.Error() == "" || !strings.Contains(err.Error(), "upstream rate limited") {
		t.Errorf("error = %q, want rate-limit message", err)
	}
}

func TestBestRateLimitMinSelection(t *testing.T) {
	e1 := &upstream.RateLimitError{RetryAfter: 10 * time.Second}
	e2 := &upstream.RateLimitError{RetryAfter: 2 * time.Second}
	e3 := &upstream.RateLimitError{RetryAfter: 5 * time.Second}

	best := bestRateLimit([]*upstream.RateLimitError{e1, e2, e3})
	if best != e2 {
		t.Errorf("bestRateLimit = %v (RetryAfter: %s), want e2 (RetryAfter: %s)", best, best.RetryAfter, e2.RetryAfter)
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
	reg := registry.New(p.cfg.Load(), nil)
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
	cfg := p.cfg.Load()
	cfg.MaxMessagesPerDay = 2
	p.cfg.Store(cfg)

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
	cfg := p.cfg.Load()
	cfg.MaxMessagesPerDay = 1
	p.cfg.Store(cfg)

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

// TestSetConfigReloadsDailyLimit is the regression guard for the P1 stale
// config bug: the pool kept the *config.Config it was built with, so a
// reloaded config (dashboard save / admin reload) never took effect for
// the daily message cap. SetConfig must swap the pointer the pool reads.
func TestSetConfigReloadsDailyLimit(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	// One successful chat under the default (unlimited) config.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	chatOnce(t, p, lease)
	p.LeaseRelease(lease)

	// Reload a config with a daily cap of 1.
	newCfg := *p.cfg.Load()
	newCfg.MaxMessagesPerDay = 1
	p.SetConfig(&newCfg)

	// The next acquire must respect the NEW limit: one chat is already on
	// the books, so the cap bites immediately.
	_, err = p.Acquire(context.Background(), modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want *upstream.RateLimitError after SetConfig cap, got %v", err)
	}
	if !errors.Is(err, upstream.ErrRateLimited) {
		t.Error("errors.Is(ErrRateLimited) = false")
	}
	if rle.Limit != 1 {
		t.Errorf("quota limit = %v, want 1 (reloaded config)", rle.Limit)
	}
}

// TestSetConfigWarnsOnPersistenceChange pins the reload warning: session
// persistence is fixed at startup (the store is built from the boot config
// and injected via SetSessionStore), so a reloaded config that changes
// SESSION_PERSIST / SESSION_STATE_FILE must warn that it only takes effect
// on the next restart instead of silently doing nothing.
func TestSetConfigWarnsOnPersistenceChange(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var buf bytes.Buffer
	testLogger := slog.New(slog.NewTextHandler(&buf, nil))

	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SessionPersist = true
		c.SessionStateFile = ".freebuff-session-state.json"
	}, mock)
	p.logger = testLogger // internal test: capture the pool's logger
	p.SetSessionStore(session.NewStore(filepath.Join(t.TempDir(), "state.json")))

	// Same persistence config on reload → no warning.
	buf.Reset()
	same := *p.cfg.Load()
	p.SetConfig(&same)
	if got := buf.String(); got != "" {
		t.Fatalf("SetConfig with unchanged persistence logged: %q, want none", got)
	}

	// Persistence disabled → warn.
	buf.Reset()
	disabled := *p.cfg.Load()
	disabled.SessionPersist = false
	p.SetConfig(&disabled)
	if got := buf.String(); !strings.Contains(got, "SESSION_PERSIST") {
		t.Fatalf("SetConfig disabling persistence logged %q, want SESSION_PERSIST warning", got)
	}

	// Same persistence, different state file → warn.
	buf.Reset()
	moved := *p.cfg.Load()
	moved.SessionPersist = true
	moved.SessionStateFile = "elsewhere.json"
	p.SetConfig(&moved)
	if got := buf.String(); !strings.Contains(got, "SESSION_PERSIST") || !strings.Contains(got, "elsewhere.json") {
		t.Fatalf("SetConfig moving the state file logged %q, want SESSION_PERSIST warning with new path", got)
	}
}

// TestSetConfigWarnsWhenPersistenceTurnedOn covers the pre-injection state
// (SetSessionStore never called, store nil): a reload that turns
// SESSION_PERSIST on cannot build the store at runtime, so it must warn.
func TestSetConfigWarnsWhenPersistenceTurnedOn(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var buf bytes.Buffer
	p := newTestPool(t, mock)
	p.logger = slog.New(slog.NewTextHandler(&buf, nil))
	// SetSessionStore never called: recorded persistence is off.

	enabled := *p.cfg.Load()
	enabled.SessionPersist = true
	enabled.SessionStateFile = ".freebuff-session-state.json"
	p.SetConfig(&enabled)
	if got := buf.String(); !strings.Contains(got, "SESSION_PERSIST") {
		t.Fatalf("SetConfig enabling persistence logged %q, want SESSION_PERSIST warning", got)
	}
}

func TestIdleRotationFinishesRuns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	cfg := p.cfg.Load()
	cfg.IdleRotationTimeout = 10 * time.Millisecond
	p.cfg.Store(cfg)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	if got := mock.StartedRunsSnapshot(); len(got) != 1 {
		t.Fatalf("started runs = %v, want 1", got)
	}

	// Not idle yet: a maintain pass runs normally (no FINISH). Filter the
	// context-pruner child runs (issue #91) — their async FINISH may land
	// at any point and must not count as a parent-run finish.
	p.maintainTick(context.Background())
	if got := parentFinished(mock); len(got) != 0 {
		t.Fatalf("finished runs = %v before idle, want none", got)
	}

	// Past the idle threshold: one pass FINISHes all runs. The threshold is
	// crossed by mutating lastActive (deterministic; a fixed sleep would
	// race the 10ms threshold on slow CI).
	p.lastActiveMu.Lock()
	p.lastActive = time.Now().Add(-time.Second)
	p.lastActiveMu.Unlock()
	p.maintainTick(context.Background())
	finished := parentFinished(mock)
	if len(finished) != 1 || finished[0].Status != "completed" {
		t.Fatalf("finished runs after idle = %v, want 1 completed", finished)
	}

	// ...and later idle passes stay dormant (no further FINISH or START).
	for i := 0; i < 2; i++ {
		p.maintainTick(context.Background())
	}
	if got := parentFinished(mock); len(got) != 1 {
		t.Errorf("finished runs = %v, want still 1 parent (dormant while idle)", got)
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

// TestIdleRotationSkipsInflight is the regression guard for the P1 idle
// rotation bug: the idle FINISH pass used to FinishAllRuns every token,
// killing in-flight chats. Tokens holding a lease must be skipped — their
// runs stay live until the lease drains (mirrors the bridge idle sweep's
// busy-entry rule).
func TestIdleRotationSkipsInflight(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	cfg := p.cfg.Load()
	cfg.IdleRotationTimeout = 10 * time.Millisecond
	p.cfg.Store(cfg)

	// Acquire a lease and HOLD it: the run stays in the run manager.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(lease)
	if got := mock.StartedRunsSnapshot(); len(got) != 1 {
		t.Fatalf("started runs = %v, want 1", got)
	}

	// Past the idle threshold: an idle pass must NOT FINISH the held run.
	// The threshold is crossed by mutating lastActive (deterministic; a
	// fixed sleep would race the 10ms threshold on slow CI).
	p.lastActiveMu.Lock()
	p.lastActive = time.Now().Add(-time.Second)
	p.lastActiveMu.Unlock()
	p.maintainTick(context.Background())
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
		t.Fatalf("finished runs = %v, want none (in-flight lease held)", got)
	}

	// The held lease's run is still live in the manager.
	if got := p.Snapshot()[0].ActiveRuns; got != 1 {
		t.Errorf("ActiveRuns = %d, want 1 (run not finished)", got)
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
	// Issue #91: STARTs create+FINISH context-pruner child runs async; the
	// assertion is that no PARENT run was finished with idle rotation off.
	if got := parentFinished(mock); len(got) != 0 {
		t.Fatalf("finished runs = %v with idle rotation disabled, want none", got)
	}
}

func TestMaintainTickSkipsCooldownToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	// An active session + run: a normal maintain pass would poll the
	// session (GET) and may rotate the run. With the token cooling down
	// neither the maintain pass nor the session-poll pass may touch the
	// upstream at all.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	p.CooldownToken(0, time.Hour)

	before := mock.Requests
	p.maintainTick(context.Background())
	p.sessionPollTick(context.Background())
	if got := mock.Requests; got != before {
		t.Errorf("upstream requests during cooldown maintain = %d, want %d (no poll/rotate)", got, before)
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
	if snap.RiskLevel != "critical" {
		t.Errorf("RiskLevel = %q, want critical (active ban outranks the cooldown label)", snap.RiskLevel)
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

// TestAcquirePrecedenceBannedOverRateLimit pins the mixed-bucket precedence
// chain: a banned token outranks a rate-limited one, so the pool surfaces
// 403 banned instead of the generic 502 the historical all-or-nothing
// aggregation produced.
func TestAcquirePrecedenceBannedOverRateLimit(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	be := &upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(time.Hour)}
	rle := &upstream.RateLimitError{Body: "rate limit", RetryAfter: 10 * time.Minute}
	p.CooldownTokenBan(0, be)
	p.CooldownTokenRateLimit(1, rle)

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil || !errors.Is(err, upstream.ErrBanned) {
		t.Fatalf("banned + rate-limited = %v, want ban (highest precedence)", err)
	}
}

// TestAcquirePrecedenceCountryOverRateLimit pins country > rate: a
// country-blocked token outranks a rate-limited one.
func TestAcquirePrecedenceCountryOverRateLimit(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	cbe := &upstream.CountryBlockedError{CountryCode: "CN", CountryBlockReason: "region_restricted"}
	rle := &upstream.RateLimitError{Body: "rate limit", RetryAfter: 10 * time.Minute}
	p.CooldownTokenCountryBlocked(0, cbe)
	p.CooldownTokenRateLimit(1, rle)

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil || !errors.Is(err, upstream.ErrCountryBlocked) {
		t.Fatalf("country-blocked + rate-limited = %v, want country (precedence over rate)", err)
	}
}

// TestAcquirePrecedenceRateOverWaiting pins rate > waiting: with one token
// queued and another rate-limited, the remembered 429 wins.
func TestAcquirePrecedenceRateOverWaiting(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.SessionMode = "queued"
	mock0.QueuePosition = 1
	mock0.QueueDepth = 3
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)
	p.CooldownTokenRateLimit(1, &upstream.RateLimitError{Body: "rate limit", RetryAfter: 10 * time.Minute})

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil || !errors.Is(err, upstream.ErrRateLimited) {
		t.Fatalf("waiting + rate-limited = %v, want rate limit (precedence over waiting)", err)
	}
}

// TestAcquireAllCountryBlocked drives the country bucket end-to-end through
// the session layer: every token's admission returns a 403 country_blocked,
// the pool cools each down ~15m, records the block for the snapshot, and
// surfaces the CountryBlockedError (not a generic 502) while remembered.
func TestAcquireAllCountryBlocked(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.SessionMode = "country_blocked"
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.SessionMode = "country_blocked"
	p := newTestPool(t, mock0, mock1)

	_, err := p.Acquire(context.Background(), modelA)
	var cbe *upstream.CountryBlockedError
	if !errors.As(err, &cbe) {
		t.Fatalf("want *upstream.CountryBlockedError, got %v", err)
	}
	if !errors.Is(err, upstream.ErrCountryBlocked) {
		t.Errorf("errors.Is(ErrCountryBlocked) = false")
	}

	// The token cooled down ~15m and the block is recorded in the snapshot
	// even though the session never admitted (session snapshot is empty).
	snap := p.Snapshot()[0]
	if snap.CooldownUntil.Before(time.Now().Add(14 * time.Minute)) {
		t.Errorf("cooldown until = %v, want ~now+15m", snap.CooldownUntil)
	}
	if snap.CountryCode != "CN" || snap.CountryBlockReason != "region_restricted" {
		t.Errorf("snapshot country = %q/%q, want CN/region_restricted (remembered block)", snap.CountryCode, snap.CountryBlockReason)
	}

	// The remembered error keeps surfacing on the cooldown skip, and the
	// blocked tokens are not re-hit upstream.
	creates := mock0.SessionCreates + mock1.SessionCreates
	_, err = p.Acquire(context.Background(), modelA)
	var cbe2 *upstream.CountryBlockedError
	if !errors.As(err, &cbe2) {
		t.Fatalf("second acquire: want *upstream.CountryBlockedError, got %v", err)
	}
	if got := mock0.SessionCreates + mock1.SessionCreates; got != creates {
		t.Errorf("session creates after cooldown = %d, want %d (country-cooled tokens must not re-hit)", got, creates)
	}
}

// TestTokenSnapshotTierAndCountry pins the TokenSnapshot region/tier fields
// carried from the admitted session (healthz / /v1/models annotation).
func TestTokenSnapshotTierAndCountry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.AccessTier = "limited"
	mock.CountryCode = "US"
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease.TierAccess != "limited" || lease.TierCountry != "US" {
		t.Errorf("lease tier/country = %q/%q, want limited/US", lease.TierAccess, lease.TierCountry)
	}
	p.LeaseRelease(lease)

	snap := p.Snapshot()[0]
	if snap.TierAccess != "limited" || snap.CountryCode != "US" {
		t.Errorf("snapshot tier/country = %q/%q, want limited/US", snap.TierAccess, snap.CountryCode)
	}
	if snap.CountryBlockReason != "" {
		t.Errorf("CountryBlockReason = %q, want empty for an admitted session", snap.CountryBlockReason)
	}
}

// TestAcquireBridgeCountryCooldown pins the bridge-mode country cooldown: a
// country_blocked admission cools the entry ~15m, and the cooldown skip
// surfaces the remembered block instead of re-hitting upstream.
func TestAcquireBridgeCountryCooldown(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "country_blocked"
	p := newBridgePool(t, mock)

	_, err := p.AcquireBridge(context.Background(), "client-tok", modelA)
	var cbe *upstream.CountryBlockedError
	if !errors.As(err, &cbe) {
		t.Fatalf("want *upstream.CountryBlockedError, got %v", err)
	}
	if !errors.Is(err, upstream.ErrCountryBlocked) {
		t.Errorf("errors.Is(ErrCountryBlocked) = false")
	}

	// The entry cooled down: the next acquire skips it and surfaces the
	// remembered block without a second admission attempt.
	creates := mock.SessionCreates
	_, err = p.AcquireBridge(context.Background(), "client-tok", modelA)
	var cbe2 *upstream.CountryBlockedError
	if !errors.As(err, &cbe2) {
		t.Fatalf("second acquire: want *upstream.CountryBlockedError, got %v", err)
	}
	if mock.SessionCreates != creates {
		t.Errorf("session creates = %d, want %d (country-cooled entry must not re-hit upstream)", mock.SessionCreates, creates)
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

// TestBridgeEvictionFinishOutsideLock is the regression guard for the P1
// "FINISH under bridgeMu" bug: eviction used to run FinishAllRuns (a
// sequential upstream call bounded by the session-call timeout) while
// holding bridgeMu, stalling every other bridge operation for the whole
// eviction. Here the evicted entry's FINISH is held in flight for
// FinishDelay: a concurrent BridgeCount (which takes bridgeMu) must return
// immediately, proving the FINISH no longer runs under the lock.
func TestBridgeEvictionFinishOutsideLock(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ids := make([]string, maxBridgeEntries+8)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%04d", i)
	}
	mock.RunIDs = ids
	// Slow FINISH responses hold the eviction's upstream call in flight long
	// enough to probe the lock: with the old code BridgeCount would block
	// for the full delay; with the fix it returns in microseconds.
	mock.FinishDelay = 300 * time.Millisecond
	p := newBridgePool(t, mock)

	// Fill the cache to the cap.
	for i := range maxBridgeEntries {
		lease, err := p.AcquireBridge(context.Background(), fmt.Sprintf("client-tok-%02d", i), modelA)
		if err != nil {
			t.Fatal(err)
		}
		p.LeaseRelease(lease)
	}

	// A 33rd distinct token evicts the oldest entry; its FINISH is held in
	// flight by FinishDelay while the rest of the acquire proceeds.
	evictDone := make(chan error, 1)
	go func() {
		lease, err := p.AcquireBridge(context.Background(), "client-tok-evict", modelA)
		if err == nil {
			p.LeaseRelease(lease)
		}
		evictDone <- err
	}()

	// Wait until the eviction FINISH is actually being served by the mock
	// (the handler counts it before sleeping the delay).
	eventually(t, "eviction FINISH in flight", func() bool {
		return mock.FinishesStartedSnapshot() >= 1
	})

	// While the FINISH is in flight, bridge operations must not block:
	// BridgeCount takes bridgeMu, which eviction holds only for the
	// map/order mutation, never across the upstream FINISH.
	start := time.Now()
	count := p.BridgeCount()
	if elapsed := time.Since(start); elapsed >= mock.FinishDelay/2 {
		t.Errorf("BridgeCount during eviction FINISH took %v, want < %v (FINISH ran under bridgeMu)", elapsed, mock.FinishDelay/2)
	}
	if count > maxBridgeEntries {
		t.Errorf("BridgeCount = %d, want <= %d", count, maxBridgeEntries)
	}

	// The evicting acquire completes, the evicted run is FINISHed, and the
	// oldest entry is gone.
	if err := <-evictDone; err != nil {
		t.Fatal(err)
	}
	if got := mock.FinishedRunsSnapshot(); len(got) < 1 {
		t.Errorf("finished runs = %d, want >= 1 (evicted entry finished)", len(got))
	}
	if p.bridgeToken("client-tok-00") != nil {
		t.Error("oldest bridge entry still cached, want evicted")
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

// flakyFirstRT fails the very first request with a transient transport error
// and delegates everything else to base. It drives a real retry through the
// full stack deterministically (a live connection teardown surfaces as
// context.Canceled on some platforms, which must never be retried).
type flakyFirstRT struct {
	mu     sync.Mutex
	failed bool
	base   http.RoundTripper
}

func (f *flakyFirstRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	shouldFail := !f.failed
	if shouldFail {
		f.failed = true
	}
	f.mu.Unlock()
	if shouldFail {
		return nil, errors.New("read tcp 127.0.0.1:443: connection reset by peer")
	}
	return f.base.RoundTrip(req)
}

func TestPoolSnapshotTransientRetryCounters(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"` + modelA + `","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`)

	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		TransientRetries:   1,
		TLSFingerprint:     "chrome126",
	}
	client, err := upstream.New("tok-0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	// #120: the session POST is bodyless (no GetBody) and cannot be replayed
	// by the transient retry (mirrors the CLI's bare bodyless fetch) —
	// pre-admit the session on the healthy transport, then arm the flaky
	// transport so its first victim is the agent-runs START during Acquire:
	// a replayable request, which is what this test exercises.
	sess := session.NewManager(client)
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := New(cfg, []*upstream.Client{client}, []*session.Manager{sess}, reg)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sess.EnsureSessionForModel(context.Background(), modelA); err != nil {
		t.Fatal(err)
	}
	// The first upstream call (agent-runs START during Acquire) fails at the
	// transport level once; TRANSIENT_RETRIES replays it and succeeds.
	client.SetTransport(&flakyFirstRT{base: http.DefaultTransport})

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("acquire failed after transient retry: %v", err)
	}
	defer p.LeaseRelease(lease)

	opts := upstream.ChatOptions{Model: modelA, RunID: lease.Run.RunID, SessionInstanceID: lease.SessionInstanceID}
	body := []byte(`{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}]}`)
	rc, err := p.Chat(context.Background(), lease, opts, body)
	if err != nil {
		t.Fatalf("pool chat failed: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(got), `"content":"hi"`) {
		t.Errorf("stream = %q, want content chunk", got)
	}

	// Pool-wide totals and the per-token row both surface the counters.
	ps := p.PoolSnapshot()
	if ps.TransientRetries != 1 {
		t.Errorf("PoolSnapshot.TransientRetries = %d, want 1", ps.TransientRetries)
	}
	if ps.FingerprintRotations != 1 {
		t.Errorf("PoolSnapshot.FingerprintRotations = %d, want 1 (pinned chrome126 rotated on retry)", ps.FingerprintRotations)
	}
	if len(ps.Tokens) != 1 {
		t.Fatalf("PoolSnapshot.Tokens = %d rows, want 1", len(ps.Tokens))
	}
	if ps.Tokens[0].TransientRetries != 1 {
		t.Errorf("TokenSnapshot.TransientRetries = %d, want 1", ps.Tokens[0].TransientRetries)
	}
	if ps.Tokens[0].FingerprintRotations != 1 {
		t.Errorf("TokenSnapshot.FingerprintRotations = %d, want 1", ps.Tokens[0].FingerprintRotations)
	}

	// Snapshot() (healthz) carries the same per-token counters.
	snaps := p.Snapshot()
	if len(snaps) != 1 || snaps[0].TransientRetries != 1 || snaps[0].FingerprintRotations != 1 {
		t.Errorf("Snapshot() = %+v, want 1/1 retry counters", snaps)
	}
}

func TestPoolSnapshotZeroCountersWhenNoRetries(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	ps := p.PoolSnapshot()
	if ps.TransientRetries != 0 || ps.FingerprintRotations != 0 {
		t.Errorf("PoolSnapshot = %+v, want zero counters without retries", ps)
	}
	if len(ps.Tokens) != 1 || ps.Tokens[0].TransientRetries != 0 {
		t.Errorf("per-token counters = %+v, want zero", ps.Tokens)
	}
}

// TestSnapshotBanRiskLevel is the regression guard for the P2 ban
// mislabeling: a banned token must show "critical" during the ban window
// (not "high" from the cooldown case shadowing it), and the risk must drop
// after the window expires instead of staying sticky "critical" forever.
func TestSnapshotBanRiskLevel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	// Short ban window: CooldownBan also fills the shared cooldown
	// deadline, so before the fix the cooldown case matched first ("high")
	// and the remembered BanError stayed non-nil past the window.
	p.CooldownTokenBan(0, &upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(60 * time.Millisecond)})

	if got := p.Snapshot()[0].RiskLevel; got != "critical" {
		t.Errorf("RiskLevel during ban = %q, want critical", got)
	}

	// Once the ban window expires the label must drop (not sticky).
	eventually(t, "risk drop after ban window", func() bool {
		return p.Snapshot()[0].RiskLevel != "critical"
	})
	if got := p.Snapshot()[0].RiskLevel; got != "low" {
		t.Errorf("RiskLevel after ban window = %q, want low", got)
	}
}

// TestIdleFinishAllRunsHonorsMaintainCtx is the regression guard for the P2
// context.Background bug in the idle FINISH: Pool.Shutdown cancels the
// maintain ctx first and waits on the maintain goroutine, so a mid-drain
// FinishAllRuns must abort on cancel instead of blocking shutdown for the
// full upstream call timeout.
func TestIdleFinishAllRunsHonorsMaintainCtx(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	cfg := p.cfg.Load()
	cfg.IdleRotationTimeout = time.Millisecond
	p.cfg.Store(cfg)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	// Cross the idle threshold by mutating lastActive (deterministic; a
	// fixed sleep would race the 1ms threshold on slow CI).
	p.lastActiveMu.Lock()
	p.lastActive = time.Now().Add(-time.Second)
	p.lastActiveMu.Unlock()

	// Hold every FINISH upstream: only ctx cancellation can end it.
	mock.FinishDelay = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.maintainTick(ctx)
		close(done)
	}()

	eventually(t, "idle FINISH in flight", func() bool {
		return mock.FinishesStartedSnapshot() >= 1
	})
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("maintainTick did not return after ctx cancel (FinishAllRuns used context.Background)")
	}
}

// TestBridgeMaintainEvictHonorsCtx is the bridge-mode half of the same P2
// fix: the idle-eviction FinishAllRuns in bridgeMaintain must honor the
// maintain ctx so shutdown is not blocked by an in-flight FINISH.
func TestBridgeMaintainEvictHonorsCtx(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	lease, err := p.AcquireBridge(context.Background(), "idle-bridge-tok", modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	// Age the entry past bridgeIdleEvict so the sweep evicts it.
	entry := p.bridgeToken("idle-bridge-tok")
	if entry == nil {
		t.Fatal("bridge entry missing")
	}
	entry.lastUsed = time.Now().Add(-bridgeIdleEvict - time.Minute)

	mock.FinishDelay = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.bridgeMaintain(ctx, false)
		close(done)
	}()

	eventually(t, "idle-eviction FINISH in flight", func() bool {
		return mock.FinishesStartedSnapshot() >= 1
	})
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridgeMaintain did not return after ctx cancel (FinishAllRuns used context.Background)")
	}
}

// TestBridgeEvictionSkipsBusyEntry is the regression guard for the P2
// eviction bug: LRU eviction used to FINISH the runs of any victim, even
// one with an outstanding lease, killing the in-flight request. Eviction
// must skip busy entries (the idle sweep handles them once leases drain).
func TestBridgeEvictionSkipsBusyEntry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ids := make([]string, maxBridgeEntries+8)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%04d", i)
	}
	mock.RunIDs = ids
	p := newBridgePool(t, mock)

	// Fill the cache to the cap, holding an ACTIVE LEASE on the oldest
	// entry (client-tok-00) so it must survive eviction.
	var busy *Lease
	for i := 0; i < maxBridgeEntries; i++ {
		lease, err := p.AcquireBridge(context.Background(), fmt.Sprintf("client-tok-%02d", i), modelA)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			busy = lease
		} else {
			p.LeaseRelease(lease)
		}
	}

	// A new distinct token pushes the cache over the cap: eviction must
	// pick an idle victim, not the busy oldest entry.
	lease, err := p.AcquireBridge(context.Background(), "client-tok-new", modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	if e := p.bridgeToken("client-tok-00"); e == nil {
		t.Fatal("busy bridge entry was evicted while its lease is outstanding")
	}
	finished := parentFinished(mock)
	if len(finished) != 1 {
		t.Errorf("finished runs = %d, want 1 (only the idle evicted entry)", len(finished))
	}
	for _, f := range finished {
		if f.RunID == busy.Run.RunID {
			t.Errorf("busy entry's run %s FINISHed during eviction", f.RunID)
		}
	}
	if got := p.bridgeLen(); got != maxBridgeEntries {
		t.Errorf("bridge entries = %d, want %d", got, maxBridgeEntries)
	}
	p.LeaseRelease(busy)
}

// TestShutdownDrainsBridgeEntries is the regression guard for the P3 gap:
// Pool.Shutdown only drained the fixed tokens, leaving cached bridge
// entries' runs and sessions alive upstream. Shutdown must drain them
// best-effort after the fixed-token pass.
func TestShutdownDrainsBridgeEntries(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	lease1, err := p.AcquireBridge(context.Background(), "shutdown-tok-1", modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease1)
	lease2, err := p.AcquireBridge(context.Background(), "shutdown-tok-2", modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease2)

	p.Shutdown(context.Background())

	// Both bridge entries' runs were FINISHed and sessions ended.
	finished := parentFinished(mock)
	if len(finished) != 2 {
		t.Errorf("finished runs = %d, want 2 (bridge runs drained on shutdown)", len(finished))
	}
	for _, f := range finished {
		if f.Status != "completed" {
			t.Errorf("run %s finished with status %q, want completed", f.RunID, f.Status)
		}
	}
	if mock.SessionEnds != 2 {
		t.Errorf("session ends = %d, want 2 (bridge sessions ended on shutdown)", mock.SessionEnds)
	}
}

// Runtime token management: AddToken/RemoveLastToken/RemoveAllTokens mutate
// the pool safely, and a chat through an added token works end to end.
func TestRuntimeTokenManagement(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Bridge-mode pool (zero fixed tokens) pointed at the mock.
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.AuthTokens = nil
		c.UpstreamBaseURL = mock.URL()
	})
	if p.TokenCount() != 0 {
		t.Fatalf("TokenCount = %d, want 0 at start", p.TokenCount())
	}

	idx, err := p.AddToken("rt-token")
	if err != nil {
		t.Fatal(err)
	}
	if idx != 0 || p.TokenCount() != 1 {
		t.Fatalf("after AddToken: idx=%d count=%d, want 0/1", idx, p.TokenCount())
	}

	// A real chat through the added token works (mock upstream).
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	mock.ChatBody = testutil.SSEEvent(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n")
	rc, err := p.Chat(context.Background(), lease, upstream.ChatOptions{Model: modelA}, []byte(`{"model":"z-ai/glm-5.2"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	p.LeaseRelease(lease)

	// RemoveLastToken refuses while a lease is in flight.
	lease2, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveLastToken(); err == nil {
		t.Fatal("RemoveLastToken succeeded with an in-flight lease, want refusal")
	}
	p.LeaseRelease(lease2)

	if err := p.RemoveLastToken(); err != nil {
		t.Fatalf("RemoveLastToken: %v", err)
	}
	if p.TokenCount() != 0 {
		t.Fatalf("TokenCount = %d, want 0 after removal", p.TokenCount())
	}

	// Re-add + remove-all path.
	if _, err := p.AddToken("rt-2"); err != nil {
		t.Fatal(err)
	}
	p.RemoveAllTokens(context.Background())
	if p.TokenCount() != 0 {
		t.Fatalf("TokenCount = %d, want 0 after RemoveAllTokens", p.TokenCount())
	}
}

// TestAcquireChatConcurrentTokenMutation is the P1 regression guard for the
// snapshot double-load race: Acquire used to load p.toks once, then
// acquireOrder loaded it AGAIN and built indices against the newer
// (longer) snapshot — an AddToken between the two loads made the failover
// loop index the stale snapshot past its end and panic with
// index-out-of-range. The fix passes the single snapshot into acquireOrder
// (plus a defensive bounds check in the loop), so this hammers Acquire+Chat
// while a driver goroutine churns AddToken/RemoveLastToken/RemoveAllTokens.
// The panic window is narrow, so the loop repeats many times; with -race any
// reintroduced double-load that survives the panics still trips the race
// detector. Assertion: no panic, and every attempt either succeeds or fails
// cleanly (never an index-out-of-range).
func TestAcquireChatConcurrentTokenMutation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n")
	// Session-admission churn: ~2/3 of admits fail (404) in adjacent pairs,
	// so an Acquire pass walks PAST every failing token to the end of the
	// order — exactly the path that indexed past the stale snapshot in the
	// original double-load bug (a success early in the order would return
	// before the out-of-range index was reached). The sequence is long
	// enough to cover the whole hammer so the failure mix never exhausts.
	seq := make([]string, 8000)
	for i := range seq {
		if i%3 == 2 {
			seq[i] = "active"
		} else {
			seq[i] = "404"
		}
	}
	mock.SessionSequence = seq
	// Two fixed tokens to start; the driver churns the list from there.
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.UpstreamBaseURL = mock.URL()
	}, mock, mock)

	ctx := context.Background()
	body := []byte(`{"model":"z-ai/glm-5.2"}`)
	const (
		workers = 8
		iters   = 250
		cycles  = 8
	)

	var (
		mu       sync.Mutex
		panics   []string
		attempts int
		success  int
		failure  int
	)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				func() {
					defer func() {
						if r := recover(); r != nil {
							mu.Lock()
							panics = append(panics, fmt.Sprintf("%v", r))
							mu.Unlock()
						}
					}()
					lease, err := p.Acquire(ctx, modelA)
					if err != nil {
						mu.Lock()
						attempts++
						failure++
						mu.Unlock()
						return
					}
					rc, err := p.Chat(ctx, lease, upstream.ChatOptions{Model: modelA}, body)
					if err == nil {
						_ = rc.Close()
					}
					p.LeaseRelease(lease)
					mu.Lock()
					attempts++
					success++
					mu.Unlock()
				}()
			}
		}()
	}

	// Driver: churn the token list while the workers acquire/chat. AddToken
	// is the dangerous direction (it grows the snapshot acquireOrder builds
	// indices against); RemoveLastToken is refused while a lease is in
	// flight (ignored here), RemoveAllTokens empties the list.
	for i := 0; i < cycles; i++ {
		if _, err := p.AddToken(fmt.Sprintf("hammer-%d", i)); err != nil {
			t.Fatalf("AddToken: %v", err)
		}
		_ = p.RemoveLastToken()
		if _, err := p.AddToken(fmt.Sprintf("hammer-%d", i+100)); err != nil {
			t.Fatalf("AddToken: %v", err)
		}
		p.RemoveAllTokens(ctx)
		if _, err := p.AddToken(fmt.Sprintf("hammer-%d", i+200)); err != nil {
			t.Fatalf("AddToken: %v", err)
		}
	}
	wg.Wait()

	if len(panics) > 0 {
		t.Fatalf("panic(s) under concurrent token mutation: %v", panics)
	}
	if attempts != success+failure {
		t.Fatalf("attempts=%d but success=%d failure=%d", attempts, success, failure)
	}
	if success == 0 {
		t.Fatal("no chat succeeded under the hammer; mutation churn starved the workers")
	}
}

// TestUsageAccountingConcurrentTokenMutation is the P2 regression guard for
// the usage-slice indexing race: recordChat/usageCount/usageResetIn index
// p.msgsPerToken, which RemoveAllTokens (nil) and RemoveLastToken (truncate)
// mutate concurrently — usageResetIn previously had no bounds check at all
// and panicked the moment a capped Acquire raced a removal. This hammers the
// daily-cap path (usageCount + dailyLimitError -> usageResetIn) and feeds
// usage via recordChat from a seeder goroutine while the driver churns the
// token list. Assertion: no panic, every Acquire succeeds or fails cleanly,
// and the cap path actually fired.
func TestUsageAccountingConcurrentTokenMutation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.UpstreamBaseURL = mock.URL()
		c.MaxMessagesPerDay = 3
	}, mock)

	ctx := context.Background()
	const (
		workers = 8
		iters   = 250
		cycles  = 6
	)

	// Deterministic mechanism check: with token 0 pre-seeded past the cap,
	// a single-threaded Acquire MUST surface the daily-cap 429. This pins
	// the usageCount + dailyLimitError -> usageResetIn path (the functions
	// that index p.msgsPerToken) without depending on goroutine scheduling;
	// the concurrent hammer below covers the mutation race.
	for range 5 {
		p.recordChat(0)
	}
	if _, err := p.Acquire(ctx, modelA); !errors.Is(err, upstream.ErrRateLimited) {
		t.Fatalf("capped Acquire err = %v, want ErrRateLimited", err)
	}

	var (
		mu        sync.Mutex
		panics    []string
		attempts  int
		success   int
		failure   int
		capped429 int
	)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				func() {
					defer func() {
						if r := recover(); r != nil {
							mu.Lock()
							panics = append(panics, fmt.Sprintf("%v", r))
							mu.Unlock()
						}
					}()
					lease, err := p.Acquire(ctx, modelA)
					if err != nil {
						mu.Lock()
						attempts++
						failure++
						if errors.Is(err, upstream.ErrRateLimited) {
							capped429++
						}
						mu.Unlock()
						return
					}
					p.LeaseRelease(lease)
					mu.Lock()
					attempts++
					success++
					mu.Unlock()
				}()
			}
		}()
	}

	// Seeder: record usage on arbitrary indices (valid and stale) so
	// recordChat itself runs under the driver's mutations (P2 class).
	seedDone := make(chan struct{})
	go func() {
		defer close(seedDone)
		for i := range cycles * workers * 20 {
			p.recordChat(i % 8)
		}
	}()

	for i := 0; i < cycles; i++ {
		idx, err := p.AddToken(fmt.Sprintf("usage-%d", i))
		if err != nil {
			t.Fatalf("AddToken: %v", err)
		}
		// Seed the fresh generation past the cap immediately so the pool is
		// capped for nearly its whole lifetime: a worker Acquire that lands
		// here hits the daily-cap path instead of a fresh-token success.
		for range 3 {
			p.recordChat(idx)
		}
		_ = p.RemoveLastToken() // refused while a lease is in flight — fine
		p.RemoveAllTokens(ctx)
		idx, err = p.AddToken(fmt.Sprintf("usage-%d", i+100))
		if err != nil {
			t.Fatalf("AddToken: %v", err)
		}
		for range 3 {
			p.recordChat(idx)
		}
	}
	wg.Wait()
	<-seedDone

	if len(panics) > 0 {
		t.Fatalf("panic(s) under concurrent token mutation: %v", panics)
	}
	if attempts != success+failure {
		t.Fatalf("attempts=%d but success=%d failure=%d", attempts, success, failure)
	}
	t.Logf("hammer: attempts=%d success=%d failure=%d capped429=%d", attempts, success, failure, capped429)
}

// ── Wave 1 issue tests (#81, #77) ────────────────────────────────────────

// TestAcquireIpCappedCooldownBounded verifies #81: an ip_capped admission
// refusal surfaces the distinct IpCappedError and cools the token ONLY until
// the body's retryAfterMs — never the Pacific-midnight quota lock — and the
// remembered error keeps surfacing 429 ip_capped during the window.
func TestAcquireIpCappedCooldownBounded(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"status":"ip_capped","activeUsersForIp":7,"limit":4,"retryAfterMs":45000}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ended"}`)
	}
	p := newTestPool(t, mock)

	_, err := p.Acquire(context.Background(), modelA)
	if errors.Is(err, upstream.ErrRateLimited) {
		t.Fatal("ip_capped surfaced as ErrRateLimited, want distinct ErrIpCapped")
	}
	var ice *upstream.IpCappedError
	if !errors.As(err, &ice) {
		t.Fatalf("want *upstream.IpCappedError, got %v", err)
	}
	if !errors.Is(err, upstream.ErrIpCapped) {
		t.Error("not unwrap-able to ErrIpCapped")
	}
	if ice.ActiveUsersForIP != 7 || ice.Limit != 4 {
		t.Errorf("IpCappedError = %+v, want ActiveUsersForIP 7 limit 4", ice)
	}
	if ice.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %s, want 45s (bounded to retryAfterMs)", ice.RetryAfter)
	}

	// Cooldown is bounded to the retry window (+ the CLI-style up-to-20%
	// jitter since #118), NOT the Pacific midnight quota lock (which would
	// be many hours away).
	snap := p.Snapshot()[0]
	if snap.CooldownUntil.IsZero() {
		t.Fatal("CooldownUntil zero, want bounded window")
	}
	maxWant := time.Now().Add(45*time.Second + 45*time.Second/5 + time.Second)
	if snap.CooldownUntil.Before(time.Now()) || snap.CooldownUntil.After(maxWant) {
		t.Errorf("CooldownUntil = %v, want now..now+45s(+20%% jitter), not Pacific midnight", snap.CooldownUntil)
	}

	// While the window is active, a second acquire surfaces the remembered
	// ip_capped error (not a generic cooldown 502).
	_, err = p.Acquire(context.Background(), modelA)
	var ice2 *upstream.IpCappedError
	if !errors.As(err, &ice2) {
		t.Fatalf("second acquire: want *upstream.IpCappedError, got %v", err)
	}
}

// TestPoolCooldownTokenIpCappedBounded verifies the pool-level cooldown
// entry point (used by the server's chat-path recovery) bounds the window
// to the error's RetryAfter only.
func TestPoolCooldownTokenIpCappedBounded(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	p.CooldownTokenIpCapped(0, &upstream.IpCappedError{RetryAfter: 30 * time.Second, ActiveUsersForIP: 5, Limit: 4})
	snap := p.Snapshot()[0]
	if snap.CooldownUntil.IsZero() {
		t.Fatal("CooldownUntil zero, want bounded window")
	}
	// #118: the window is the full retryAfterMs plus up to 20% jitter (the
	// CLI's poll-jitter pattern), never the Pacific-midnight quota lock.
	maxWant := time.Now().Add(30*time.Second + 30*time.Second/5 + time.Second)
	if snap.CooldownUntil.Before(time.Now()) || snap.CooldownUntil.After(maxWant) {
		t.Errorf("CooldownUntil = %v, want now..now+30s(+20%% jitter), not Pacific midnight", snap.CooldownUntil)
	}

	// Out-of-range tokens are ignored without panicking.
	p.CooldownTokenIpCapped(99, &upstream.IpCappedError{RetryAfter: time.Second})
	p.CooldownTokenIpCapped(-1, &upstream.IpCappedError{RetryAfter: time.Second})
	p.CooldownTokenIpCapped(0, nil)
}

// TestSessionPollSkipsWhileChatInFlight verifies #77: the session-liveness
// poll is skipped while any run holds an in-flight lease (a poll landing
// mid-chat can kick the active session with 428), and resumes once the lease
// drains.
func TestSessionPollSkipsWhileChatInFlight(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	// Admit an active session; the lease holds InflightCount() > 0.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease == nil || lease.Run == nil {
		t.Fatal("nil lease/run")
	}

	before := mock.SessionPolls
	p.sessionPollTick(context.Background())
	if got := mock.SessionPolls; got != before {
		t.Errorf("session polls during in-flight chat = %d, want %d (poll skipped)", got, before)
	}

	// Release the lease: the next poll pass polls again.
	p.LeaseRelease(lease)
	p.sessionPollTick(context.Background())
	if got := mock.SessionPolls; got <= before {
		t.Errorf("session polls after release = %d, want > %d (poll resumed)", got, before)
	}
}

// TestSessionPollSchedule pins the liveness-poll cadence helpers (gap #2;
// reference/freebuff sdk polling-backoff.ts): the success interval is ~30s
// ±20% jitter capped to remaining+1s near expiry, and the failure backoff
// grows 20s→300s while never scheduling a retry before the server's
// Retry-After floor.
func TestSessionPollSchedule(t *testing.T) {
	t.Run("success interval jittered around 30s", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			d := sessionPollSuccessDelay(session.SessionSnapshot{})
			if d < 24*time.Second || d > 36*time.Second {
				t.Fatalf("success delay = %s, want 30s ±20%%", d)
			}
		}
	})

	t.Run("success interval capped near expiry", func(t *testing.T) {
		rem := 10 * time.Second
		d := sessionPollSuccessDelay(session.SessionSnapshot{ExpiresAt: time.Now().Add(rem)})
		// remaining+1s (clock-drift tolerant: allow a few ms either side of
		// the two time.Now() samples).
		if d < rem || d > rem+2*time.Second {
			t.Errorf("success delay near expiry = %s, want ≈ %s (remaining+1s)", d, rem+time.Second)
		}
	})

	t.Run("failure backoff doubles and caps at 300s", func(t *testing.T) {
		cases := []struct {
			failures int
			min      time.Duration
			max      time.Duration
		}{
			{1, 10 * time.Second, 20 * time.Second},   // 20s, lower-half jitter
			{2, 20 * time.Second, 40 * time.Second},   // 40s
			{3, 40 * time.Second, 80 * time.Second},   // 80s
			{6, 150 * time.Second, 300 * time.Second}, // capped at 300s
		}
		for _, tc := range cases {
			d := sessionPollBackoffDelay(tc.failures, 0)
			if d < tc.min || d > tc.max {
				t.Errorf("backoff(%d) = %s, want [%s, %s]", tc.failures, d, tc.min, tc.max)
			}
		}
	})

	t.Run("failure backoff honors Retry-After floor", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			d := sessionPollBackoffDelay(1, 60*time.Second)
			// retryAfter × (1 ± 0.2) jitter: [48s, 72s], max'd with the 20s
			// base backoff — never before the floor.
			if d < 48*time.Second || d > 300*time.Second {
				t.Errorf("backoff with Retry-After 60s = %s, want ≥ 48s (never before the floor)", d)
			}
		}
	})

	t.Run("retry-after extracted from classified errors", func(t *testing.T) {
		if got := sessionPollRetryAfter(&upstream.UpstreamError{Status: 503, RetryAfter: 45 * time.Second}); got != 45*time.Second {
			t.Errorf("UpstreamError RetryAfter = %s, want 45s", got)
		}
		if got := sessionPollRetryAfter(&upstream.RateLimitError{RetryAfter: 90 * time.Second}); got != 90*time.Second {
			t.Errorf("RateLimitError RetryAfter = %s, want 90s", got)
		}
		if got := sessionPollRetryAfter(errors.New("plain")); got != 0 {
			t.Errorf("plain error RetryAfter = %s, want 0", got)
		}
	})
}
