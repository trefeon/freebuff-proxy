package pool

// Edge-case and E2E tests for the pool: live failover matrix, bridge daily
// cap, idle handling in bridge mode, RemoveLastToken drain + race, maintain
// queued-advance/session-poll, runtime token actions, and exact daily-cap
// accounting. Regression guards for the audit's pool bugs (AcquireBridge
// idle tracking, RemoveLastToken drain/TOCTOU, Cooldown ban memory, idle
// bridge sweep).

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

// TestLiveFailoverMatrix drives the LIVE failover path end-to-end for every
// upstream failure class: the first token fails its session admission with
// the real wire error, the pool cools it down, and the second healthy token
// serves the lease. Subsequent acquires skip the cooled token without
// re-hitting it. (Existing tests only mix remembered-cooldown states; this
// exercises the live admission failures, including the country 403 path
// with no server E2E coverage.)
func TestLiveFailoverMatrix(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testutil.MockUpstream)
	}{
		{"ban", func(m *testutil.MockUpstream) { m.Ban = true }},
		{"rate-limit", func(m *testutil.MockUpstream) { m.RateLimit = true }},
		{"country-blocked", func(m *testutil.MockUpstream) { m.SessionMode = "country_blocked" }},
		{"auth-reject", func(m *testutil.MockUpstream) { m.AuthReject = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failing := testutil.NewMock()
			defer failing.Close()
			tc.setup(failing)
			healthy := testutil.NewMock()
			defer healthy.Close()
			p := newTestPool(t, failing, healthy)

			// First acquire: token-1's live admission fails, the pool fails
			// over to the healthy token.
			lease, err := p.Acquire(context.Background(), modelA)
			if err != nil {
				t.Fatalf("acquire with failover: %v", err)
			}
			if lease.Token != 1 {
				t.Errorf("lease token = %d, want 1 (failover to healthy)", lease.Token)
			}
			if lease.AgentID != agentA {
				t.Errorf("lease agent = %q, want %q", lease.AgentID, agentA)
			}
			p.LeaseRelease(lease)

			// The failing token was tried once (live path) and cooled down.
			// Note: the flag-based failure modes (Ban/RateLimit/AuthReject)
			// short-circuit the mock BEFORE the session counter, so the
			// admission attempt is observed via the total request counter.
			if snap := p.Snapshot()[0]; snap.CooldownUntil.Before(time.Now()) {
				t.Errorf("failing token not cooled down: %v", snap.CooldownUntil)
			}
			reqs := failing.Requests
			if reqs == 0 {
				t.Errorf("failing token never hit (live admission expected)")
			}

			// Subsequent acquires skip the cooled token entirely (zero
			// re-hits) and reuse the healthy token's live session.
			lease, err = p.Acquire(context.Background(), modelA)
			if err != nil {
				t.Fatal(err)
			}
			if lease.Token != 1 {
				t.Errorf("second lease token = %d, want 1 (cooled token skipped)", lease.Token)
			}
			p.LeaseRelease(lease)
			if got := failing.Requests; got != reqs {
				t.Errorf("failing token re-hit: requests = %d, want %d", got, reqs)
			}
			if got := healthy.SessionCreatesSnapshot(); got != 1 {
				t.Errorf("healthy token session creates = %d, want 1 (reused)", got)
			}
		})
	}
}

// TestLiveFailoverAllBanned covers the all-failing live path: every token's
// admission returns 403 banned, the pool surfaces the typed ban bucket (not
// a generic combined error), cools every token, and never re-hits them.
func TestLiveFailoverAllBanned(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.Ban = true
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.Ban = true
	p := newTestPool(t, mock0, mock1)

	_, err := p.Acquire(context.Background(), modelA)
	var be *upstream.BanError
	if !errors.As(err, &be) {
		t.Fatalf("want *upstream.BanError (typed bucket), got %v", err)
	}
	if !errors.Is(err, upstream.ErrBanned) {
		t.Errorf("errors.Is(ErrBanned) = false")
	}

	// Both tokens were tried once (live path — the Ban flag short-circuits
	// before the session counter, so observe via total requests) and cooled
	// for the ban window; the remembered error keeps surfacing without
	// re-hitting.
	reqs := mock0.Requests + mock1.Requests
	if reqs == 0 {
		t.Error("banned tokens never hit (live admission expected)")
	}
	for _, snap := range p.Snapshot() {
		if snap.CooldownUntil.Before(time.Now()) {
			t.Errorf("token %d not cooled: %v", snap.Token, snap.CooldownUntil)
		}
	}
	_, err = p.Acquire(context.Background(), modelA)
	if !errors.Is(err, upstream.ErrBanned) {
		t.Errorf("second acquire = %v, want remembered ErrBanned", err)
	}
	if got := mock0.Requests + mock1.Requests; got != reqs {
		t.Errorf("requests after cooldown = %d, want %d (no re-hit)", got, reqs)
	}
}

// TestAcquireCancelledMidFailover cancels the request while token-1's
// admission is in flight: the pool must abort with ctx.Err() instead of
// failing over to token-2 (no further upstream calls after the cancel).
func TestAcquireCancelledMidFailover(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.SessionCreateDelay = 500 * time.Millisecond // admission hangs
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.Acquire(ctx, modelA)
		done <- err
	}()

	// Wait until token-1's admission is in flight, then cancel.
	eventually(t, "token-1 admission started", func() bool {
		return mock0.SessionCreatesSnapshot() >= 1
	})
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Acquire after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return after ctx cancel")
	}
	if got := mock1.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("token-2 session creates = %d, want 0 (no failover after cancel)", got)
	}
}

// TestBridgeDailyMessageCap drives the bridge usage-accounting path
// (bridgeUsageCount / bridgeDailyLimitError), which had zero coverage: a
// bridge entry capped at MAX_MESSAGES_PER_DAY=1 gets a 429 on the second
// acquire, and two client tokens have independent caps.
func TestBridgeDailyMessageCap(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"chatcmpl-b1","object":"chat.completion.chunk","created":1,"model":"` + modelA + `","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
	p := newBridgePool(t, mock)
	cfg := p.cfg.Load()
	cfg.MaxMessagesPerDay = 1
	p.cfg.Store(cfg)

	// Client A: the first chat succeeds (and records usage)...
	lease, err := p.AcquireBridge(context.Background(), "client-a", modelA)
	if err != nil {
		t.Fatal(err)
	}
	chatOnce(t, p, lease)
	p.LeaseRelease(lease)

	// ...the second acquire is capped with a 429 + window-reset RetryAfter.
	_, err = p.AcquireBridge(context.Background(), "client-a", modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("capped acquire: want *upstream.RateLimitError, got %v", err)
	}
	if !errors.Is(err, upstream.ErrRateLimited) {
		t.Error("errors.Is(ErrRateLimited) = false")
	}
	if rle.RetryAfter <= 0 || rle.RetryAfter > usageWindow {
		t.Errorf("RetryAfter = %s, want within (0, 24h]", rle.RetryAfter)
	}
	if rle.Limit != 1 || rle.RecentCount != 1 {
		t.Errorf("quota = %v/%v, want 1/1", rle.RecentCount, rle.Limit)
	}

	// Client B's cap is independent: its first chat still succeeds even
	// though client A already used its slot.
	leaseB, err := p.AcquireBridge(context.Background(), "client-b", modelA)
	if err != nil {
		t.Fatalf("client B acquire failed despite independent cap: %v", err)
	}
	chatOnce(t, p, leaseB)
	p.LeaseRelease(leaseB)

	// And client B is capped on its second acquire too.
	_, err = p.AcquireBridge(context.Background(), "client-b", modelA)
	if !errors.Is(err, upstream.ErrRateLimited) {
		t.Fatalf("client B second acquire = %v, want ErrRateLimited", err)
	}
	if got := p.bridgeUsageCount(p.bridgeToken("client-a")); got != 1 {
		t.Errorf("client A usage = %d, want 1 (unchanged by client B)", got)
	}
}

// TestBridgeDailyUsageCounter verifies that the global bridgeDailyUsage
// counter is incremented by bridgeRecordChat and reset by bridgeMaintain.
func TestBridgeDailyUsageCounter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"chatcmpl-b1","object":"chat.completion.chunk","created":1,"model":"` + modelA + `","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
	p := newBridgePool(t, mock)

	// Create a bridge entry and record 5 chats.
	lease, err := p.AcquireBridge(context.Background(), "counter-client", modelA)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		chatOnce(t, p, lease)
	}
	p.LeaseRelease(lease)

	// Verify global counter matches per-entry usage.
	p.bridgeMu.Lock()
	got := p.bridgeDailyUsage
	p.bridgeMu.Unlock()
	if got != 5 {
		t.Fatalf("bridgeDailyUsage = %d, want 5", got)
	}
	entry := p.bridgeToken("counter-client")
	if got := p.bridgeUsageCount(entry); got != 5 {
		t.Fatalf("per-entry usage = %d, want 5", got)
	}

	// Set BridgeDailyLimit=3; AcquireBridge must return an error.
	cfg := p.cfg.Load()
	cfg.BridgeDailyLimit = 3
	p.cfg.Store(cfg)
	_, err = p.AcquireBridge(context.Background(), "counter-client", modelA)
	if err == nil {
		t.Fatal("expected error for bridge daily limit, got nil")
	}
	if !strings.Contains(err.Error(), "daily limit") {
		t.Fatalf("error = %q, want substring 'daily limit'", err)
	}

	// Run bridgeMaintain to trigger the counter reset (no entries evict
	// since the entry was just used). The counter should recompute from
	// live entries (still 5, all within the 24h window).
	p.bridgeMaintain(context.Background(), false)
	p.bridgeMu.Lock()
	got = p.bridgeDailyUsage
	p.bridgeMu.Unlock()
	if got != 5 {
		t.Fatalf("after maintain: bridgeDailyUsage = %d, want 5", got)
	}
}

// TestBridgeIdlePause is the regression guard for the P1 bridge idle bug:

// TestBridgeIdlePause is the regression guard for the P1 bridge idle bug:
// AcquireBridge never updated p.lastActive, so IDLE_ROTATION_TIMEOUT was
// dead config in bridge mode — lastActive stayed zero, the pool never
// idle-paused, and bridge entries were polled/queued-advanced every
// maintain pass indefinitely. Bridge traffic must mark the pool active, and
// once idle a maintain pass must not touch the upstream at all. Fails
// before the fix (lastActive stays zero → every pass maintains the entry).
func TestBridgeIdlePause(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)
	cfg := p.cfg.Load()
	cfg.IdleRotationTimeout = 10 * time.Millisecond
	p.cfg.Store(cfg)

	lease, err := p.AcquireBridge(context.Background(), "client-tok", modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	if got := mock.StartedRunsSnapshot(); len(got) != 1 {
		t.Fatalf("started runs = %v, want 1", got)
	}

	// The fix: bridge traffic must mark the pool active — without it
	// lastActive stays zero, idleFor() reports 0, and the pool never enters
	// the idle branch (IDLE_ROTATION_TIMEOUT is dead config in bridge
	// mode). Fails before the fix.
	p.lastActiveMu.Lock()
	active := !p.lastActive.IsZero()
	p.lastActiveMu.Unlock()
	if !active {
		t.Fatal("AcquireBridge did not mark the pool active (lastActive zero)")
	}

	// Cross the idle threshold by mutating lastActive (deterministic — a
	// fixed sleep would race the 10ms threshold on slow CI), then assert
	// idle passes are quiet.
	p.lastActiveMu.Lock()
	p.lastActive = time.Now().Add(-time.Second)
	p.lastActiveMu.Unlock()

	// Idle passes must not touch the upstream (no session poll / queued
	// advance / rotation) and must not evict the recently-used entry.
	reqs := mock.Requests
	p.maintainTick(context.Background())
	p.maintainTick(context.Background())
	if got := mock.Requests; got != reqs {
		t.Errorf("upstream requests during idle passes = %d, want %d (no maintain activity)", got, reqs)
	}
	if got := p.bridgeLen(); got != 1 {
		t.Errorf("bridge entries = %d, want 1 (recently-used entry not evicted)", got)
	}
}

// TestBridgeMaintainRunsOnIdlePass is the regression guard for the P2 idle
// sweep bug: maintainTick's idle branch returned before bridgeMaintain, so
// in mixed mode bridge entries idle past bridgeIdleEvict were never swept
// while the pool stayed idle — their sessions stayed admitted upstream until
// expiry. An idle pass must still run the bridge sweep (only the per-token
// session-poll/queued-advance pauses). Fails before the fix (the idle branch
// returns before the sweep).
func TestBridgeMaintainRunsOnIdlePass(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.UpstreamBaseURL = mock.URL() // bridge entries must hit the mock, not the real gateway
	}, mock) // one fixed token: mixed mode
	cfg := p.cfg.Load()
	cfg.IdleRotationTimeout = 10 * time.Millisecond
	p.cfg.Store(cfg)

	// A pooled acquire marks the pool active (lastActive); then a bridge
	// entry is created and aged past bridgeIdleEvict.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	bridgeLease, err := p.AcquireBridge(context.Background(), "idle-bridge-tok", modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(bridgeLease)
	entry := p.bridgeToken("idle-bridge-tok")
	if entry == nil {
		t.Fatal("bridge entry missing")
	}
	entry.lastUsed = time.Now().Add(-bridgeIdleEvict - time.Minute)

	// Cross the idle threshold by mutating lastActive (deterministic).
	p.lastActiveMu.Lock()
	p.lastActive = time.Now().Add(-time.Second)
	p.lastActiveMu.Unlock()

	// The idle pass must evict the idle bridge entry: its run is FINISHed
	// and its session ended, and the entry is dropped from the cache. The
	// fixed token's run is FINISHed by the idle pass too (idle semantics).
	p.maintainTick(context.Background())
	if got := p.bridgeToken("idle-bridge-tok"); got != nil {
		t.Error("idle bridge entry not evicted on an idle pass")
	}
	if got := mock.FinishedRunsSnapshot(); len(got) < 2 {
		t.Errorf("finished runs = %d, want >= 2 (fixed + bridge idle FINISH)", len(got))
	}
	if mock.SessionEnds == 0 {
		t.Error("bridge session not ended on idle eviction")
	}
}

// TestBridgeIdleSweepSkipsBusy pins the busy-entry rule for the IDLE sweep
// (TestBridgeEvictionSkipsBusyEntry covers LRU eviction): an entry idle past
// bridgeIdleEvict with an outstanding lease must NOT be evicted — FINISHing
// its run would kill the in-flight chat — even though the sweep considers
// it idle.
func TestBridgeIdleSweepSkipsBusy(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	lease, err := p.AcquireBridge(context.Background(), "busy-tok", modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(lease)

	entry := p.bridgeToken("busy-tok")
	if entry == nil {
		t.Fatal("bridge entry missing")
	}
	entry.lastUsed = time.Now().Add(-bridgeIdleEvict - time.Minute)

	p.bridgeMaintain(context.Background(), false)

	if p.bridgeToken("busy-tok") == nil {
		t.Error("busy bridge entry evicted while its lease is outstanding")
	}
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
		t.Errorf("finished runs = %d, want 0 (busy run must not be finished)", len(got))
	}
}

// TestBridgeDeadTokenEvictDefersWhenBusy is the regression guard for the
// dead-token eviction race (B6): a token confirmed dead (ErrAuthRejected)
// by one request while ANOTHER request on the same token is mid-stream must
// not be evicted — FinishAllRuns on the busy entry would kill the
// concurrent chat, and dropping it from the cache would orphan the
// stream's draining run outside bridgeMaintain's and Pool.Shutdown's
// reach. The eviction is deferred to the idle sweep: the entry stays
// cached (cooled down, so no new request passes it) until its leases
// drain and it sits idle past bridgeIdleEvict. Fails before the fix (the
// dead-token path FINISHed the busy entry's run and ended its session).
func TestBridgeDeadTokenEvictDefersWhenBusy(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	// A live chat holds a lease on the token (inflight = 1).
	lease, err := p.AcquireBridge(context.Background(), "dead-tok", modelA)
	if err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreatesSnapshot() != 1 {
		t.Fatalf("session creates = %d, want 1", mock.SessionCreatesSnapshot())
	}

	// The token dies while the chat is in flight: a concurrent request on
	// the same token hits a 401 (session admission for another model forces
	// an upstream create past the cached session) and triggers the
	// dead-token eviction path.
	mock.SetAuthReject(true)
	_, err = p.AcquireBridge(context.Background(), "dead-tok", modelB)
	if !errors.Is(err, upstream.ErrAuthRejected) {
		t.Fatalf("second acquire err = %v, want ErrAuthRejected", err)
	}

	// The busy entry must NOT be evicted or cleaned up: the in-flight run
	// is not FINISHed and the session is not ended.
	if got := p.bridgeToken("dead-tok"); got == nil {
		t.Fatal("busy dead-token entry evicted while its lease is outstanding")
	}
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
		t.Errorf("finished runs = %d, want 0 (busy run must not be finished)", len(got))
	}
	if mock.SessionEnds != 0 {
		t.Errorf("session ends = %d, want 0 (busy entry session must not be ended)", mock.SessionEnds)
	}
	// The token-death knob also 401s the FINISH/EndSession cleanup calls,
	// so restore it before the reclaim phase (the eviction decision, not
	// the mock's persistent rejection, is what the sweep must honor).
	mock.SetAuthReject(false)

	// Once the lease drains and the entry idles, the sweep reclaims it:
	// FINISH + EndSession + cache removal.
	p.LeaseRelease(lease)
	entry := p.bridgeToken("dead-tok")
	if entry == nil {
		t.Fatal("deferred dead-token entry missing")
	}
	entry.lastUsed = time.Now().Add(-bridgeIdleEvict - time.Minute)
	p.bridgeMaintain(context.Background(), false)
	if got := p.bridgeToken("dead-tok"); got != nil {
		t.Error("idle dead-token entry not evicted by the sweep")
	}
	if got := mock.FinishedRunsSnapshot(); len(got) != 1 {
		t.Errorf("finished runs = %d, want 1 (idle sweep FINISH)", len(got))
	}
	if mock.SessionEnds != 1 {
		t.Errorf("session ends = %d, want 1 (idle sweep EndSession)", mock.SessionEnds)
	}
}

// TestBridgeDeadTokenEvictsWhenIdle pins the non-racing half of the B6
// gate: a dead token with NO outstanding lease is still evicted
// immediately (run FINISHed + session ended, entry dropped from the
// cache), not left for the idle sweep — that is the point of B6 (a dead
// token must not sit in the cache for the full bridgeIdleEvict window).
// The acquire-path wiring is exercised by
// TestBridgeDeadTokenEvictDefersWhenBusy; this test calls the eviction
// directly so the cleanup assertions are deterministic (the mock's
// AuthReject knob 401s the FINISH/EndSession calls too).
func TestBridgeDeadTokenEvictsWhenIdle(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	lease, err := p.AcquireBridge(context.Background(), "dead-tok", modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	p.bridgeEvictToken("dead-tok")

	if got := p.bridgeToken("dead-tok"); got != nil {
		t.Error("idle dead-token entry not evicted immediately")
	}
	if got := mock.FinishedRunsSnapshot(); len(got) != 1 {
		t.Errorf("finished runs = %d, want 1 (dead-token FINISH)", len(got))
	}
	if mock.SessionEnds != 1 {
		t.Errorf("session ends = %d, want 1 (dead-token EndSession)", mock.SessionEnds)
	}
}

// TestBridgeEvictionAllBusyKeepsCap pins the all-busy eviction behavior:
// when every cached entry holds an outstanding lease, a new distinct token
// cannot evict any of them (FINISHing a busy entry would kill the in-flight
// chat). The new entry is itself unleased at creation time, but it must
// ALSO never be evicted: bridgeEntryFor hands it back for immediate use —
// admitting a session and starting a run on an entry that was dropped from
// the cache would leave that run and session invisible to bridgeMaintain
// and Pool.Shutdown (leaked upstream + a daily session slot burned per new
// client under saturation). The cache may sit one over the cap until an
// older entry's lease drains and the idle sweep reclaims it.
func TestBridgeEvictionAllBusyKeepsCap(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ids := make([]string, maxBridgeEntries+4)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%04d", i)
	}
	mock.RunIDs = ids
	p := newBridgePool(t, mock)

	// Hold leases on ALL cache slots.
	held := make([]*Lease, 0, maxBridgeEntries)
	for i := 0; i < maxBridgeEntries; i++ {
		lease, err := p.AcquireBridge(context.Background(), fmt.Sprintf("client-tok-%02d", i), modelA)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, lease)
	}

	// A 33rd distinct token: the existing entries are all busy, and the new
	// (still unleased) entry cannot be its own eviction victim either — no
	// eviction happens this pass and the cache sits at cap+1.
	lease33, err := p.AcquireBridge(context.Background(), "client-tok-new", modelA)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.bridgeLen(); got != maxBridgeEntries+1 {
		t.Errorf("bridge entries = %d, want %d (new entry kept; cap briefly exceeded until a lease drains)", got, maxBridgeEntries+1)
	}
	if e := p.bridgeToken("client-tok-new"); e == nil {
		t.Error("new entry not cached after all-busy creation, want kept (its run would otherwise leak)")
	}
	// The busy entries all survived.
	for i := 0; i < maxBridgeEntries; i++ {
		if e := p.bridgeToken(fmt.Sprintf("client-tok-%02d", i)); e == nil {
			t.Errorf("busy entry client-tok-%02d evicted", i)
		}
	}
	// No runs were FINISHed: nothing was evictable this pass.
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
		t.Errorf("finished runs = %d, want 0 (nothing evictable while all entries busy)", len(got))
	}
	for _, l := range held {
		p.LeaseRelease(l)
	}
	p.LeaseRelease(lease33)
}

// TestRemoveLastTokenDrainsRun is the regression guard for the P1 removal
// leak: RemoveLastToken removed the token without finishing its run or
// ending its admitted session (contrast RemoveAllTokens), so the run and
// session stayed alive upstream. Removal must drain the token.
func TestRemoveLastTokenDrainsRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	if got := mock.StartedRunsSnapshot(); len(got) != 1 {
		t.Fatalf("started runs = %v, want 1", got)
	}
	if mock.SessionEnds != 0 {
		t.Fatalf("session ends = %d before removal, want 0", mock.SessionEnds)
	}

	if err := p.RemoveLastToken(); err != nil {
		t.Fatalf("RemoveLastToken: %v", err)
	}
	if p.TokenCount() != 0 {
		t.Fatalf("TokenCount = %d, want 0", p.TokenCount())
	}

	// The removed token's run was FINISHed and its admitted session ended
	// (both synchronous inside RemoveLastToken).
	if got := mock.FinishedRunsSnapshot(); len(got) != 1 || got[0].Status != "completed" {
		t.Errorf("finished runs = %v, want 1 completed", got)
	}
	if mock.SessionEnds != 1 {
		t.Errorf("session ends = %d, want 1 (removed token's session ended)", mock.SessionEnds)
	}
}

// TestRemoveLastTokenRaceHammer hammers Acquire/Chat/LeaseRelease against a
// concurrent driver churning RemoveLastToken/AddToken: the removal's busy
// check and snapshot swap are TOCTOU, so a lease can slip through onto the
// removed token. The pool must not panic, must release every slipped lease
// (no leaked inflight), and must not leave any removed token's run active —
// the retired-entry drain finishes them. Run with -race.
func TestRemoveLastTokenRaceHammer(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n")
	ids := make([]string, 4000)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%04d", i)
	}
	mock.RunIDs = ids
	// Interleave active/disabled admissions so Acquire passes exercise both
	// paths while the driver churns the token list.
	seq := make([]string, 6000)
	for i := range seq {
		if i%2 == 0 {
			seq[i] = "active"
		} else {
			seq[i] = "404"
		}
	}
	mock.SessionSequence = seq
	p := newTestPool(t, mock, mock)

	ctx := context.Background()
	body := []byte(`{"model":"` + modelA + `"}`)
	const (
		workers = 8
		iters   = 300
		cycles  = 40
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

	// Driver: churn the token list so removals race in-flight acquires
	// (RemoveLastToken refuses while a lease is in flight — fine).
	for i := 0; i < cycles; i++ {
		if _, err := p.AddToken(fmt.Sprintf("rm-%d", i)); err != nil {
			t.Fatalf("AddToken: %v", err)
		}
		_ = p.RemoveLastToken()
		if _, err := p.AddToken(fmt.Sprintf("rm-%d", i+100)); err != nil {
			t.Fatalf("AddToken: %v", err)
		}
	}
	if _, err := p.AddToken("rm-final"); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	if len(panics) > 0 {
		t.Fatalf("panic(s) under concurrent removal: %v", panics)
	}
	if attempts != success+failure {
		t.Fatalf("attempts=%d but success=%d failure=%d", attempts, success, failure)
	}
	if success == 0 {
		t.Fatal("no chat succeeded under the hammer; removal churn starved the workers")
	}

	// Every retired (removed) token must be fully drained: no leaked
	// inflight, no active runs.
	p.retiredMu.Lock()
	for entry := range p.retired {
		if got := entry.runs.InflightCount(); got != 0 {
			t.Errorf("retired token leaked inflight = %d", got)
		}
		if got := entry.runs.Snapshot().ActiveRuns; got != 0 {
			t.Errorf("retired token left %d active runs", got)
		}
	}
	p.retiredMu.Unlock()

	// Current tokens hold no leaked inflight either.
	toks := p.toks.Load()
	for i, tok := range *toks {
		if got := tok.runs.InflightCount(); got != 0 {
			t.Errorf("current token %d leaked inflight = %d", i, got)
		}
	}
}

// TestMaintainTickAdvancesQueuedAndPollsActive is the first E2E coverage of
// the maintain pass's session work: a queued session is advanced
// (EnsureSession polls upstream) once its pollAt passes, and an active
// session is liveness-polled on the jittered sessionPollTick schedule. Both
// are observable via mock.SessionPolls.
func TestMaintainTickAdvancesQueuedAndPollsActive(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued", "active"}
	mock.QueuePosition = 1
	mock.QueueDepth = 3
	mock.EstimatedWaitMs = 2000 // mock pollAt = now+2s: admission returns the waiting-room error
	p := newTestPool(t, mock)

	// Acquire admits a queued session (the mock clamps the wait to >= 1s,
	// so the admission returns the waiting-room error).
	_, err := p.Acquire(context.Background(), modelA)
	var wr *session.WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("want waiting-room error from queued admission, got %v", err)
	}
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("session creates = %d, want 1", got)
	}

	// Maintain passes poll and advance the queued session once its pollAt
	// passes (~1s; deadline-polling replaces a fixed sleep).
	eventually(t, "queued session advanced by maintain", func() bool {
		p.maintainTick(context.Background())
		return mock.SessionPolls >= 1
	})
	if got := p.Snapshot()[0].SessionStatus; got != "active" {
		t.Errorf("session status = %q, want active after queued advance", got)
	}

	// Once active, a sessionPollTick pass with a due schedule polls again
	// (the plain compact poll — no heartbeat header; gap #2).
	polls := mock.SessionPolls
	p.sessionPollTick(context.Background())
	if got := mock.SessionPolls; got <= polls {
		t.Errorf("session polls = %d, want > %d (poll on active)", got, polls)
	}
}

// TestEmptyPoolAcquire pins the empty-pool error: Acquire over a pool with
// no fixed tokens (bridge mode without a bridge token in hand) must return
// a clear error, not panic.
func TestEmptyPoolAcquire(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.AuthTokens = nil
		c.UpstreamBaseURL = mock.URL()
	})

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil {
		t.Fatal("want error for empty pool")
	}
	if !strings.Contains(err.Error(), "no auth tokens configured") {
		t.Errorf("error = %q, want 'no auth tokens configured'", err)
	}
}

// TestProbeToken pins the dashboard test action: ProbeToken runs a
// zero-cost GET session probe (no session claimed) against the token's
// upstream client and returns the live session state with quota.
func TestProbeToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	st, err := p.ProbeToken(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("ProbeToken returned nil state, want live session state")
	}
	// The probe is a GET with no instance header: it claims no session slot.
	if mock.SessionCreates != 0 {
		t.Errorf("session creates = %d, want 0 (probe must not claim a session)", mock.SessionCreates)
	}
	// The probe surfaces the live quota from rateLimitsByModel.
	if len(st.RateLimitsByModel) == 0 {
		t.Errorf("RateLimitsByModel = %v, want quota from the probe response", st.RateLimitsByModel)
	}

	// Out-of-range tokens error without panicking.
	if _, err := p.ProbeToken(context.Background(), 99); err == nil {
		t.Error("ProbeToken(99) succeeded, want out-of-range error")
	}
}

// TestFinishTokenRuns pins the dashboard finish action: all active runs of a
// token are FINISHed upstream and dropped from the manager.
func TestFinishTokenRuns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	if err := p.FinishTokenRuns(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := p.Snapshot()[0].ActiveRuns; got != 0 {
		t.Errorf("ActiveRuns = %d, want 0 after FinishTokenRuns", got)
	}
	finished := mock.FinishedRunsSnapshot()
	// FinishTokenRuns is synchronous, so the parent run-0001 FINISH is
	// already recorded when it returns (no async pruner children since G4).
	parentDone := false
	for _, f := range finished {
		if f.RunID == "run-0001" && f.Status == "completed" {
			parentDone = true
		}
	}
	if !parentDone {
		t.Errorf("finished runs = %v, want run-0001 completed", finished)
	}

	// Out-of-range tokens error without panicking.
	if err := p.FinishTokenRuns(context.Background(), 99); err == nil {
		t.Error("FinishTokenRuns(99) succeeded, want out-of-range error")
	}
}

// TestUnlockToken pins the dashboard unlock action: clearing a ban/rate
// cooldown makes the token acquirable again immediately.
func TestUnlockToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	p.CooldownTokenBan(0, &upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(time.Hour)})
	if err := p.UnlockToken(0); err != nil {
		t.Fatal(err)
	}
	snap := p.Snapshot()[0]
	if !snap.CooldownUntil.IsZero() {
		t.Errorf("CooldownUntil = %v after unlock, want zero", snap.CooldownUntil)
	}
	if snap.RiskLevel != "low" {
		t.Errorf("RiskLevel = %q after unlock, want low", snap.RiskLevel)
	}
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("acquire after unlock: %v", err)
	}
	p.LeaseRelease(lease)

	if err := p.UnlockToken(99); err == nil {
		t.Error("UnlockToken(99) succeeded, want out-of-range error")
	}
}

// TestDailyCapExactRetryAfter pins the daily-cap RetryAfter exactly: with a
// known oldest usage timestamp, usageResetIn is time.Until(oldest+24h) (the
// moment the slot frees) — the existing tests only bounds-check (0, 24h].
func TestDailyCapExactRetryAfter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	cfg := p.cfg.Load()
	cfg.MaxMessagesPerDay = 1
	p.cfg.Store(cfg)

	// Seed usage with a KNOWN oldest timestamp.
	oldest := time.Now().Add(-2 * time.Hour)
	p.usageMu.Lock()
	p.msgsPerToken[0] = []time.Time{oldest}
	p.usageMu.Unlock()

	want := time.Until(oldest.Add(usageWindow))
	got := p.usageResetIn(0)
	if d := got - want; d < -time.Second || d > time.Second {
		t.Errorf("usageResetIn = %v, want ~%v (until oldest+24h)", got, want)
	}
	if got <= 0 {
		t.Fatalf("usageResetIn = %v, want > 0", got)
	}

	// The surfaced 429 carries the same exact reset.
	_, err := p.Acquire(context.Background(), modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want *upstream.RateLimitError, got %v", err)
	}
	if d := rle.RetryAfter - want; d < -time.Second || d > time.Second {
		t.Errorf("RetryAfter = %v, want ~%v (until oldest+24h)", rle.RetryAfter, want)
	}
}

// TestCooldownAfterBanClearsBanMemory is the pool-level pin for the P2
// Cooldown bug (see runs.TestCooldownClearsBanAndCountryWindows): after a
// dashboard Cooldown, an acquire must surface the plain cooldown error, not
// a stale ban.
func TestCooldownAfterBanClearsBanMemory(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true
	p := newTestPool(t, mock)

	// Live admission ban.
	_, err := p.Acquire(context.Background(), modelA)
	if !errors.Is(err, upstream.ErrBanned) {
		t.Fatalf("want ErrBanned from live ban, got %v", err)
	}

	// Dashboard-style cooldown clears the remembered ban.
	p.CooldownToken(0, time.Hour)
	_, err = p.Acquire(context.Background(), modelA)
	if errors.Is(err, upstream.ErrBanned) {
		t.Fatal("stale ban surfaced after Cooldown")
	}
	if err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Errorf("error = %v, want plain cooldown error", err)
	}
}

// TestAcquireHotSessionModelTiebreak pins the hot-session model tiebreak:
// among hot tokens, the one whose live session already serves the requested
// model is tried first (token-2 for modelB here), so its session is reused.
func TestAcquireHotSessionModelTiebreak(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	// Token 1 admits a modelA session; token 2 admits a modelB session
	// (token 1 is cooled down so the second admission lands on token 2).
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	p.CooldownToken(0, time.Hour)
	lease, err = p.Acquire(context.Background(), modelB)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Token != 1 {
		t.Fatalf("modelB lease token = %d, want 1", lease.Token)
	}
	p.LeaseRelease(lease)
	_ = p.UnlockToken(0)

	// Both tokens are hot; the modelB request must prefer token 2 (its
	// session already serves modelB) over token 1.
	lease, err = p.Acquire(context.Background(), modelB)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Token != 1 {
		t.Errorf("tiebreak lease token = %d, want 1 (hot session serves modelB)", lease.Token)
	}
	p.LeaseRelease(lease)
	if mock1.SessionCreates != 1 {
		t.Errorf("token 2 session creates = %d, want 1 (session reused)", mock1.SessionCreates)
	}
	if mock0.SessionCreates != 1 {
		t.Errorf("token 1 session creates = %d, want 1 (never re-admitted for modelB)", mock0.SessionCreates)
	}
}
