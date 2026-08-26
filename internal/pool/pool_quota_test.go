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
	if rle.Limit != 3 || rle.RecentCount != 3.6 {
		t.Errorf("quota = %v/%v, want 3/3.6", rle.RecentCount, rle.Limit)
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
	// The first upstream call (agent-runs START during Acquire) fails at the
	// transport level once; TRANSIENT_RETRIES replays it and succeeds.
	client.SetTransport(&flakyFirstRT{base: http.DefaultTransport})

	sess := session.NewManager(client)
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := New(cfg, []*upstream.Client{client}, []*session.Manager{sess}, reg)
	if err != nil {
		t.Fatal(err)
	}

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
