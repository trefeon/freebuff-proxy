package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

func TestCreateActive(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1", mock.SessionCreates)
	}

	// Second call is served from cache.
	instance, err = mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want still 1", mock.SessionCreates)
	}
}

func TestExpiredCacheRefreshes(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Expiry beyond the 30-min grace window (issue #115): a session that
	// expired more than graceWindow ago must refresh. Expiries inside the
	// window are reusable (TestEnsureSessionRidesGraceFastPath).
	mock.ExpiresIn = -31 * time.Minute
	mgr := newTestManager(t, mock)

	// First call: no cache → one create, state trusted on return.
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1", mock.SessionCreates)
	}

	// Second call: stale cache past grace → refresh (create #2).
	instance, err = mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (stale cache past grace → refresh)", mock.SessionCreates)
	}
}

// TestSingleFlightFailureBounded verifies a failed refresh is NOT amplified:
// N concurrent callers must trigger exactly 1 upstream create and all N must
// surface the retained refresh error (instead of each becoming the next
// refresher and re-running the failing POST).
//
// Determinism (issue #205): under loaded -race runners a follower could
// formerly be scheduled only after the leader's instant 429 round trip had
// fully completed, then legitimately re-enter leader election and issue a
// second create — an ordering artifact, not amplification. The leader's
// rate-limited response is now parked on HoldRateLimit until every follower
// has registered as a waiter (proven via testWaiterPark), making the
// exactly-one-request assertion schedule-independent. A caller arriving
// after a completed failed round is a legitimate retry, not amplification.
func TestSingleFlightFailureBounded(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimit = true // every route returns 429 rate_limited
	hold := make(chan struct{})
	mock.HoldRateLimit = hold // park the winning caller's response mid-request
	mgr := newTestManager(t, mock)

	const followers = 7 // +1 in-flight refresher = 8 concurrent callers

	// testWaiterPark runs while m.mu is held when a follower is about to
	// park on the refresh channel; each send proves that follower observed
	// refreshing==true and elected to wait instead of leading.
	arrived := make(chan struct{}, followers)
	mgr.testWaiterPark = func() { arrived <- struct{}{} }

	leaderErr := make(chan error, 1)
	go func() {
		_, err := mgr.EnsureSession(context.Background())
		leaderErr <- err
	}()

	var wg sync.WaitGroup
	followerErrs := make([]error, followers)
	for i := range followers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, followerErrs[i] = mgr.EnsureSession(context.Background())
		}()
	}

	// Whoever wins leadership is provably mid-request while held, so every
	// remaining caller must take the waiter branch before release.
	for i := range followers {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d/%d followers registered as waiters", i, followers)
		}
	}
	close(hold) // release the held response; its failure fans out to waiters
	wg.Wait()

	if got := mock.RequestCount(); got != 1 {
		t.Errorf("upstream requests = %d, want exactly 1 (single-flight failure must not amplify)", got)
	}
	err := <-leaderErr
	assertRateLimited(t, "leader", err)
	for i, ferr := range followerErrs {
		assertRateLimited(t, fmt.Sprintf("follower %d", i), ferr)
	}
}

func assertRateLimited(t *testing.T, who string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s got nil error, want the retained refresh error", who)
		return
	}
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Errorf("%s error = %T %v, want RateLimitError", who, err, err)
	}
}

func TestSingleFlight(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatDelay = 150 * time.Millisecond // slow create
	mgr := newTestManager(t, mock)

	const n = 10
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = mgr.EnsureSession(context.Background())
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] != "inst-abc-123" {
			t.Errorf("caller %d instance = %q", i, results[i])
		}
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1 (single-flight)", mock.SessionCreates)
	}
}

func TestInvalidateRefreshes(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.Invalidate()
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 after Invalidate", mock.SessionCreates)
	}
}

func TestEndSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionEnds != 1 {
		t.Errorf("ends = %d, want 1", mock.SessionEnds)
	}
	// Cache cleared: next ensure re-creates.
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 after EndSession", mock.SessionCreates)
	}
}

func TestSnapshotModelAndExpiresAt(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSessionForModel(context.Background(), "thudm/glm-5.2")
	if err != nil {
		t.Fatal(err)
	}

	snap := mgr.Snapshot()
	if snap.Status != "active" {
		t.Errorf("Status = %q, want active", snap.Status)
	}
	if snap.Model != "thudm/glm-5.2" {
		t.Errorf("Model = %q, want thudm/glm-5.2", snap.Model)
	}
	if snap.ExpiresAt.IsZero() {
		t.Error("ExpiresAt should not be zero")
	}
	// GracePeriodEndsAt (write-only cache field) is surfaced in the snapshot.
	if snap.GracePeriodEndsAt.IsZero() {
		t.Error("GracePeriodEndsAt should not be zero")
	}
	if want := snap.ExpiresAt.Add(graceWindow); !snap.GracePeriodEndsAt.Equal(want) {
		t.Errorf("GracePeriodEndsAt = %v, want %v (expiresAt + graceWindow)", snap.GracePeriodEndsAt, want)
	}
}

func TestSnapshotQuotaByModel(t *testing.T) {
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
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	snap := mgr.Snapshot()
	q, ok := snap.QuotaByModel["z-ai/glm-5.2"]
	if !ok {
		t.Fatalf("QuotaByModel missing z-ai/glm-5.2: %+v", snap.QuotaByModel)
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
	if q.Entitlement["referral"] != 1 || q.Entitlement["streak"] != 3 {
		t.Errorf("entitlement = %+v, want referral=1 streak=3", q.Entitlement)
	}
	if len(snap.Entitlement) != 0 {
		t.Errorf("top-level Entitlement = %+v, want empty (nested per model)", snap.Entitlement)
	}
}

// TestCommitPreservesQuotaAcrossReAdmit verifies issue #146: when a session
// is re-admitted and the upstream response omits rateLimitsByModel, the
// previously-seen quota map is preserved so the dashboard quota table stays
// visible between quota-carrying responses.
func TestCommitPreservesQuotaAcrossReAdmit(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		"z-ai/glm-5.2": map[string]any{
			"model":       "z-ai/glm-5.2",
			"limit":       5,
			"recentCount": 2,
			"period":      "pacific_day",
		},
	}
	mgr := newTestManager(t, mock)

	// First admission: quota data is populated.
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := mgr.Snapshot()
	if q, ok := snap.QuotaByModel["z-ai/glm-5.2"]; !ok || q.Limit != 5 {
		t.Fatalf("first admission quota = %+v, want z-ai/glm-5.2 limit=5", snap.QuotaByModel)
	}

	// Invalidate and clear upstream quota data to simulate a re-admit
	// where the upstream omits rateLimitsByModel.
	mgr.Invalidate()
	mock.RateLimitsByModel = nil // second response carries no quota
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The previously-seen quota must survive the re-admit.
	snap2 := mgr.Snapshot()
	q, ok := snap2.QuotaByModel["z-ai/glm-5.2"]
	if !ok {
		t.Fatalf("quota lost after re-admit without upstream quota: QuotaByModel = %+v", snap2.QuotaByModel)
	}
	if q.Limit != 5 || q.RecentCount != 2 {
		t.Errorf("preserved quota = limit=%v recent=%v, want 5/2", q.Limit, q.RecentCount)
	}
}

// TestEndSessionSwallowsSessionInvalid verifies EndSession returns nil when
// the upstream DELETE fails with a "slot already gone" rejection — 400
// session_expired (ErrSessionInvalid) and 400 session_superseded
// (ErrSessionSuperseded, #119) — nothing to do either way.
func TestEndSessionSwallowsSessionInvalid(t *testing.T) {
	for _, tc := range []struct {
		body string
		want error
	}{
		{`{"error":"session_expired"}`, upstream.ErrSessionInvalid},
		{`{"error":"session_superseded"}`, upstream.ErrSessionSuperseded},
	} {
		t.Run(tc.body, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mgr := newTestManager(t, mock)

			if _, err := mgr.EnsureSession(context.Background()); err != nil {
				t.Fatal(err)
			}

			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				http.NotFound(w, r)
			}

			if err := mgr.EndSession(context.Background()); err != nil {
				t.Fatalf("EndSession with 400 %s = %v, want nil (%v swallowed)", tc.body, err, tc.want)
			}
		})
	}
}

// TestEndSessionSurfacesServerError verifies EndSession surfaces a generic
// upstream error (500) instead of swallowing it.
func TestEndSessionSurfacesServerError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
			return
		}
		http.NotFound(w, r)
	}

	if err := mgr.EndSession(context.Background()); err == nil {
		t.Fatal("EndSession with 500 = nil, want error surfaced")
	}
}

// TestTerminalEventReasons pins that every terminal session event carries a
// reason from the vocabulary (ended|superseded|shutdown|model_lock|expired|
// 409|poll|store), and session invalidated gains the triggering HTTP status
// when known.
func TestTerminalEventReasons(t *testing.T) {
	t.Run("invalidated carries caller reason and status", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)
		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		restore := captureLogs(&buf)
		defer restore()
		mgr.InvalidateWithReason("expired", 400)
		got := buf.String()
		if !strings.Contains(got, `msg="session invalidated"`) ||
			!strings.Contains(got, "reason=expired") ||
			!strings.Contains(got, "status=400") {
			t.Errorf("invalidated log missing reason/status:\n%s", got)
		}
	})

	t.Run("bare invalidate defaults to 409 reason", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)
		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		restore := captureLogs(&buf)
		defer restore()
		mgr.Invalidate()
		got := buf.String()
		if !strings.Contains(got, `msg="session invalidated"`) || !strings.Contains(got, "reason=409") {
			t.Errorf("bare Invalidate log missing default reason=409:\n%s", got)
		}
	})

	t.Run("ended carries reason ended", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)
		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		restore := captureLogs(&buf)
		defer restore()
		if err := mgr.EndSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := buf.String()
		if !strings.Contains(got, `msg="session ended"`) || !strings.Contains(got, "reason=ended") {
			t.Errorf("ended log missing reason=ended:\n%s", got)
		}
	})

	t.Run("shutdown with persistence keeps active session", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr := newTestManagerWithStore(t, mock, store)
		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		restore := captureLogs(&buf)
		defer restore()
		if err := mgr.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := buf.String()
		// Session redesign: persistence keeps the active slot for restart
		// resume — no "ended on shutdown" log, no upstream DELETE.
		if !strings.Contains(got, `msg="session kept on shutdown (persistence, restart resumes)"`) {
			t.Errorf("shutdown log missing keep message:\n%s", got)
		}
	})

	t.Run("dropped during poll carries poll reason and status", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)
		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooEarly) // 428 waiting_room_required
			_, _ = io.WriteString(w, `{"error":"waiting_room_required"}`)
		}
		var buf bytes.Buffer
		restore := captureLogs(&buf)
		defer restore()
		_ = mgr.Poll(context.Background())
		got := buf.String()
		if !strings.Contains(got, `msg="session dropped during poll"`) ||
			!strings.Contains(got, "reason=poll") ||
			!strings.Contains(got, "status=waiting_room_required") {
			t.Errorf("poll drop log missing reason=poll/status:\n%s", got)
		}
	})

	t.Run("ended during poll maps superseded reason", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)
		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"superseded","instanceId":"inst-abc-123"}`)
		}
		var buf bytes.Buffer
		restore := captureLogs(&buf)
		defer restore()
		if err := mgr.Poll(context.Background()); err != nil {
			t.Fatalf("Poll: %v", err)
		}
		got := buf.String()
		if !strings.Contains(got, `msg="session ended during poll"`) ||
			!strings.Contains(got, "reason=superseded") ||
			!strings.Contains(got, "status=superseded") {
			t.Errorf("poll end log missing reason=superseded/status:\n%s", got)
		}
	})

	t.Run("recreated maps upstream status to table reason", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionSequence = []string{"none", "active"}
		mgr := newTestManager(t, mock)
		var buf bytes.Buffer
		restore := captureLogs(&buf)
		defer restore()
		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := buf.String()
		if !strings.Contains(got, `msg="session recreated"`) ||
			!strings.Contains(got, "reason=ended") ||
			!strings.Contains(got, "status=none") {
			t.Errorf("recreated log missing table reason/status:\n%s", got)
		}
	})
}

// TestReAdmitStormDetector pins that more than 3 invalidations within 60s
// emit exactly ONE "session re-admit storm" summary with the count,
// duration_ms, superseded, and burned_slots fields; isolated invalidations
// stay quiet; the detector re-arms only after a full quiet window.
func TestReAdmitStormDetector(t *testing.T) {
	base := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	t.Run("isolated and three-in-window stay quiet", func(t *testing.T) {
		now := base
		m := &Manager{now: func() time.Time { return now }}
		var buf bytes.Buffer
		restore := captureLogs(&buf)
		defer restore()

		m.InvalidateWithReason("expired", 400)
		now = now.Add(30 * time.Second)
		m.InvalidateWithReason("expired", 400)
		now = now.Add(29 * time.Second)
		m.InvalidateWithReason("expired", 400) // 3 within 59s: not >3
		if got := buf.String(); strings.Contains(got, "session re-admit storm") {
			t.Fatalf("isolated/3-in-window invalidations emitted a storm summary:\n%s", got)
		}
	})

	t.Run("burst fires one summary then suppresses until quiet", func(t *testing.T) {
		now := base
		m := &Manager{now: func() time.Time { return now }}
		var buf bytes.Buffer
		restore := captureLogs(&buf)
		defer restore()

		m.InvalidateWithReason("superseded", 409) // t+0s
		now = now.Add(time.Second)
		m.InvalidateWithReason("superseded", 409) // t+1s
		now = now.Add(time.Second)
		m.InvalidateWithReason("expired", 400) // t+2s
		now = now.Add(time.Second)
		m.recordReAdmitTrigger()               // pre-emptive re-admit in the window
		m.InvalidateWithReason("expired", 400) // t+3s: 4th in window → storm

		got := buf.String()
		if n := strings.Count(got, "session re-admit storm"); n != 1 {
			t.Fatalf("storm summaries = %d, want 1:\n%s", n, got)
		}
		for _, want := range []string{"count=4", "duration_ms=3000", "superseded=2", "burned_slots=1"} {
			if !strings.Contains(got, want) {
				t.Errorf("storm summary missing %s:\n%s", want, got)
			}
		}

		// A 5th invalidation right after the burst is suppressed.
		now = now.Add(time.Second)
		m.InvalidateWithReason("expired", 400)
		if n := strings.Count(buf.String(), "session re-admit storm"); n != 1 {
			t.Fatalf("storm summaries after 5th invalidation = %d, want still 1 (suppressed):\n%s", n, buf.String())
		}

		// After a full quiet window the detector re-arms: a new burst of 4
		// fires a second summary.
		now = now.Add(70 * time.Second) // 70s past the last summary
		m.InvalidateWithReason("expired", 400)
		now = now.Add(time.Second)
		m.InvalidateWithReason("expired", 400)
		now = now.Add(time.Second)
		m.InvalidateWithReason("expired", 400)
		now = now.Add(time.Second)
		m.InvalidateWithReason("expired", 400) // 4th in window, quiet passed
		if n := strings.Count(buf.String(), "session re-admit storm"); n != 2 {
			t.Fatalf("storm summaries after re-arm burst = %d, want 2:\n%s", n, buf.String())
		}
	})
}

// TestReAdmitStormTracksPreemptiveTriggers wires the burned_slots count to
// the real pre-emptive re-admit path (issue #99): a triggered re-admit
// whose session is then invalidated in a storm counts as a burned slot.
func TestReAdmitStormTracksPreemptiveTriggers(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusOK, map[string]any{"status": "active", "instanceId": "inst-1", "expiresAt": time.Now().Add(30 * time.Minute).Format(time.RFC3339)})
			return
		}
		n := creates.Add(1)
		id := "inst-1"
		if n >= 2 {
			id = "inst-2"
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "active", "instanceId": id, "expiresAt": time.Now().Add(10 * time.Second).Format(time.RFC3339)})
	}
	m := newTestSession(t, mock)
	var nowMu sync.Mutex
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	// m.now is called from background goroutines (pre-emptive re-admit);
	// the fake clock must be safe for concurrent reads while the test
	// advances it.
	m.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	m.SetReAdmitLead(time.Minute)

	if _, err := m.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Second call: cached active with ~5s left (10s expiry, 60s lead) —
	// triggers the pre-emptive re-admit and rides the old session.
	if _, err := m.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	var buf lockedBuf
	restore := captureLogs(&buf)
	defer restore()
	for i := 0; i < 4; i++ {
		nowMu.Lock()
		now = now.Add(time.Second)
		nowMu.Unlock()
		m.InvalidateWithReason("expired", 400)
	}
	got := buf.String()
	if n := strings.Count(got, "session re-admit storm"); n != 1 {
		t.Fatalf("storm summaries = %d, want 1:\n%s", n, got)
	}
	if !strings.Contains(got, "burned_slots=1") {
		t.Errorf("burned_slots missing/inaccurate, want 1 pre-emptive trigger in window:\n%s", got)
	}
}

// TestPreemptiveReAdmitOncePerExpiry pins issue #132: a pre-emptive re-admit
// fires at most ONCE per expiry window. Every request in the lead window
// must ride the old session instead of re-triggering a fresh upstream
// create — the observed 22-trigger / 30-create storm around a single
// expiry.
func TestPreemptiveReAdmitOncePerExpiry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	mgr.SetReAdmitLead(time.Minute)

	// Land a session, then squeeze its expiry into the lead window.
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	if mgr.state != nil {
		mgr.state.expiresAt = time.Now().Add(30 * time.Second)
	}
	mgr.mu.Unlock()

	// First request in the window triggers the async re-admit (1 create).
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Fatalf("triggered request instance = %q, want the old session being ridden", instance)
	}
	// Let the async create land.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && mock.SessionCreatesSnapshot() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := mock.SessionCreatesSnapshot(); got != 2 {
		t.Fatalf("creates after first trigger = %d, want 2 (initial + one re-admit)", got)
	}

	// Every further request in the same expiry window must NOT re-trigger.
	for i := 0; i < 5; i++ {
		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if got := mock.SessionCreatesSnapshot(); got != 2 {
		t.Errorf("creates after 5 rides = %d, want still 2 (once per expiry window)", got)
	}
}

// TestInvalidateInstanceGuarded pins issue #132: invalidating a session by a
// stale instance id (a chat that rode the old, superseded instance) must not
// drop a newer cached session.
func TestInvalidateInstanceGuarded(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A stale instance id leaves the cache alone.
	mgr.InvalidateInstance("inst-stale-999")
	mgr.mu.Lock()
	cur := ""
	if mgr.state != nil {
		cur = mgr.state.instanceID
	}
	alive := mgr.state != nil
	mgr.mu.Unlock()
	if !alive {
		t.Fatal("cached session invalidated by a stale instance id")
	}

	// The matching instance id clears it.
	mgr.InvalidateInstance(cur)
	mgr.mu.Lock()
	alive = mgr.state != nil
	mgr.mu.Unlock()
	if alive {
		t.Fatal("cached session not invalidated by its own instance id")
	}
}

// TestInvalidateInstanceWithReason pins the #159 superseded invalidation
// path: the reason-aware instance-guarded drop records the "superseded"
// reason (feeding the re-admit storm detector's superseded count) with the
// triggering status, and a stale instance id — a chat still riding the OLD,
// superseded instance after a fresh re-admit — leaves the newer cache alone.
func TestInvalidateInstanceWithReason(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	restore := captureLogs(&buf)
	defer restore()

	// A stale instance id must not drop the newer cached session, even with
	// the superseded reason (#132 guard).
	mgr.InvalidateInstanceWithReason("inst-stale-999", ReasonSuperseded, http.StatusConflict)
	mgr.mu.Lock()
	cur := ""
	if mgr.state != nil {
		cur = mgr.state.instanceID
	}
	alive := mgr.state != nil
	mgr.mu.Unlock()
	if !alive {
		t.Fatal("cached session invalidated by a stale superseded instance id")
	}

	// The matching instance id clears it and records reason=superseded
	// status=409.
	mgr.InvalidateInstanceWithReason(cur, ReasonSuperseded, http.StatusConflict)
	mgr.mu.Lock()
	alive = mgr.state != nil
	mgr.mu.Unlock()
	if alive {
		t.Fatal("cached session not invalidated by its own superseded instance id")
	}
	got := buf.String()
	if !strings.Contains(got, `msg="session invalidated"`) ||
		!strings.Contains(got, "reason=superseded") ||
		!strings.Contains(got, "status=409") {
		t.Errorf("superseded invalidated log missing reason/status:\n%s", got)
	}
}
