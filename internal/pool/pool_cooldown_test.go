package pool

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

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

	// Not idle yet: a maintain pass runs normally (no FINISH — no pruner
	// children exist since G4, so any FINISH here would be a parent run).
	p.maintainTick(context.Background())
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
		t.Fatalf("finished runs = %v before idle, want none", got)
	}

	// Past the idle threshold: one pass FINISHes all runs. The threshold is
	// crossed by mutating lastActive (deterministic; a fixed sleep would
	// race the 10ms threshold on slow CI).
	p.lastActiveMu.Lock()
	p.lastActive = time.Now().Add(-time.Second)
	p.lastActiveMu.Unlock()
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
	// With idle rotation off no PARENT run may be finished (and no pruner
	// children exist since G4).
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
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
	mock.SetFinishDelay(time.Hour)

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

// ── Wave 1 issue tests (#81, #77) ───────────────────────────────────────────────────────────────────────

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

	// Cooldown is bounded to the retry window ±20% jitter (#118), NOT the
	// Pacific midnight quota lock (which would be many hours away).
	snap := p.Snapshot()[0]
	if snap.CooldownUntil.IsZero() {
		t.Fatal("CooldownUntil zero, want bounded window")
	}
	want := time.Now().Add(45 * time.Second)
	diff := snap.CooldownUntil.Sub(want)
	if diff < -11*time.Second || diff > 11*time.Second {
		t.Errorf("CooldownUntil = %v, want ≈ now+45s ±20%% jitter (bounded), not Pacific midnight", snap.CooldownUntil)
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
	want := time.Now().Add(30 * time.Second)
	diff := snap.CooldownUntil.Sub(want)
	if diff < -8*time.Second || diff > 8*time.Second {
		t.Errorf("CooldownUntil = %v, want ≈ now+30s ±20%% jitter (bounded), not Pacific midnight", snap.CooldownUntil)
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

// TestBanViewDerivation pins the #198/#199 snapshot ban view: the type is
// read off BanError.ResumesAt (NOT the folded cooldown deadline, which
// runs.CooldownBan sets to now+24h even for hard bans), a temporary ban
// carries its resumes_at deadline, and expired/absent bans yield zero
// values.
func TestBanViewDerivation(t *testing.T) {
	until := time.Now().Add(time.Hour)

	banType, bannedUntil := banView(&upstream.BanError{Body: "banned", ResumesAt: until}, until.Add(24*time.Hour))
	if banType != "temporary" || !bannedUntil.Equal(until) {
		t.Errorf("temporary ban view = %q/%s, want temporary/%s", banType, bannedUntil, until)
	}

	banType, bannedUntil = banView(&upstream.BanError{Body: "banned"}, time.Now().Add(24*time.Hour))
	if banType != "hard" || !bannedUntil.IsZero() {
		t.Errorf("hard ban view = %q/%s, want hard/zero", banType, bannedUntil)
	}

	banType, bannedUntil = banView(nil, time.Time{})
	if banType != "" || !bannedUntil.IsZero() {
		t.Errorf("no-ban view = %q/%s, want empty/zero", banType, bannedUntil)
	}

	expired := time.Now().Add(-time.Minute)
	banType, _ = banView(&upstream.BanError{Body: "banned", ResumesAt: expired}, expired)
	if banType != "" {
		t.Errorf("expired ban view = %q, want empty", banType)
	}
}
