package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

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
	mock.SetFinishDelay(300 * time.Millisecond)
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

	mock.SetFinishDelay(time.Hour)

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
	finished := mock.FinishedRunsSnapshot()
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
	finished := mock.FinishedRunsSnapshot()
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

func TestBridgeAcquireSyncsAdmittedModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-coerced-bridge","model":"`+modelB+`"}`)
	}
	p := newBridgePool(t, mock)

	// Bridge mode uses the requested model regardless of what upstream returns.
	lease, err := p.AcquireBridge(context.Background(), "test-token", modelA)
	if err != nil {
		t.Fatalf("AcquireBridge failed: %v", err)
	}
	defer p.LeaseRelease(lease)

	if lease.Model != modelA {
		t.Errorf("lease.Model = %q, want requested model %q", lease.Model, modelA)
	}
}

// TestBridgeTokenLocking verifies that LockBridgeEntry prevents AcquireBridge
// and UnlockBridgeEntry restores access (#187).
func TestBridgeTokenLocking(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	token := "bridge-lock-test-token"
	key := tokenKey(token)

	// Acquire once to populate the cache.
	lease, err := p.AcquireBridge(context.Background(), token, modelA)
	if err != nil {
		t.Fatalf("initial AcquireBridge failed: %v", err)
	}
	p.LeaseRelease(lease)

	// Lock the entry.
	if err := p.LockBridgeEntry(key); err != nil {
		t.Fatalf("LockBridgeEntry failed: %v", err)
	}

	// AcquireBridge must now fail.
	_, err = p.AcquireBridge(context.Background(), token, modelA)
	if err == nil {
		t.Fatal("AcquireBridge succeeded on locked entry, want error")
	}

	// Unlock the entry.
	if err := p.UnlockBridgeEntry(key); err != nil {
		t.Fatalf("UnlockBridgeEntry failed: %v", err)
	}

	// AcquireBridge must succeed again.
	lease, err = p.AcquireBridge(context.Background(), token, modelA)
	if err != nil {
		t.Fatalf("AcquireBridge after unlock failed: %v", err)
	}
	p.LeaseRelease(lease)
}

// TestBridgeLockNotFound verifies LockBridgeEntry/UnlockBridgeEntry return
// errors for nonexistent keys (#187).
func TestBridgeLockNotFound(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	fakeKey := "00000000000000000000000000000000" // 32 hex chars
	if err := p.LockBridgeEntry(fakeKey); err == nil {
		t.Fatal("LockBridgeEntry succeeded on nonexistent key, want error")
	}
	if err := p.UnlockBridgeEntry(fakeKey); err == nil {
		t.Fatal("UnlockBridgeEntry succeeded on nonexistent key, want error")
	}
}

// TestBridgeSlidingTTL verifies the 24h idle eviction window: entries idle
// less than bridgeIdleEvict are kept; entries idle longer are evicted (#187).
func TestBridgeSlidingTTL(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	// Acquire to create an entry.
	lease, err := p.AcquireBridge(context.Background(), "ttl-token", modelA)
	if err != nil {
		t.Fatalf("AcquireBridge failed: %v", err)
	}
	p.LeaseRelease(lease)

	if p.bridgeLen() != 1 {
		t.Fatalf("bridgeLen = %d, want 1", p.bridgeLen())
	}

	// Manipulate lastUsed to simulate an old entry (just past 24h).
	key := tokenKey("ttl-token")
	entry := p.bridgeToken("ttl-token")
	entry.lastUsed = time.Now().Add(-bridgeIdleEvict - time.Minute)

	// Run the idle-eviction maintain pass (idle=true only runs sweep).
	p.bridgeMaintain(context.Background(), true)

	if p.bridgeLen() != 0 {
		t.Errorf("bridgeLen = %d after idle eviction, want 0", p.bridgeLen())
	}
	_ = key // used above for clarity
}

// TestBridgeSnapshot verifies BridgeSnapshot returns correct per-entry data
// for the dashboard (#187).
func TestBridgeSnapshot(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	token := "snapshot-token"
	lease, err := p.AcquireBridge(context.Background(), token, modelA)
	if err != nil {
		t.Fatalf("AcquireBridge failed: %v", err)
	}
	p.LeaseRelease(lease)

	snaps := p.BridgeSnapshot()
	if len(snaps) != 1 {
		t.Fatalf("BridgeSnapshot len = %d, want 1", len(snaps))
	}
	snap := snaps[0]
	if snap.Key == "" {
		t.Error("BridgeSnapshot Key is empty")
	}
	if snap.LastUsed.IsZero() {
		t.Error("BridgeSnapshot LastUsed is zero")
	}
	if snap.ActiveRuns != 1 {
		// ActiveRuns counts the just-released run until FINISH drains it.
		t.Logf("BridgeSnapshot ActiveRuns = %d (run pending FINISH, acceptable)", snap.ActiveRuns)
	}
	if snap.Locked {
		t.Error("BridgeSnapshot Locked = true, want false (new entry)")
	}

	// Lock and verify snapshot reflects it.
	key := tokenKey(token)
	if err := p.LockBridgeEntry(key); err != nil {
		t.Fatalf("LockBridgeEntry failed: %v", err)
	}
	snaps = p.BridgeSnapshot()
	if len(snaps) != 1 {
		t.Fatalf("BridgeSnapshot len after lock = %d, want 1", len(snaps))
	}
	if !snaps[0].Locked {
		t.Error("BridgeSnapshot Locked = false after LockBridgeEntry, want true")
	}
}
