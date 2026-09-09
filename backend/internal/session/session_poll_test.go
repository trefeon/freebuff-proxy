package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// TestPoll404Recreates verifies a poll 404 is treated as ended (recreate
// path) rather than a cached permanent "disabled": the session manager must
// re-create the session after the upstream reports it gone.
func TestPoll404Recreates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued", "404"}
	mock.EstimatedWaitMs = 100
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSession(context.Background())
	var wr *WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("want WaitingRoomError from queued create, got %v", err)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1", mock.SessionCreates)
	}

	// Wait for pollAt (queued minimum wait is 1s), then poll → 404 → ended →
	// recreate.
	time.Sleep(1100 * time.Millisecond)
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q, want inst-abc-123 (recreated after poll 404)", instance)
	}
	if mock.SessionPolls != 1 {
		t.Errorf("polls = %d, want 1", mock.SessionPolls)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (poll 404 → recreate)", mock.SessionCreates)
	}
}

func TestPoll(t *testing.T) {
	t.Run("inactive session returns nil", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		if err := mgr.Poll(context.Background()); err != nil {
			t.Fatalf("Poll inactive: %v", err)
		}
	})

	t.Run("active session polls compact without heartbeat header", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		_, err := mgr.EnsureSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		var gotCompact, gotHeartbeat string
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			gotCompact = r.Header.Get("x-freebuff-compact-session")
			gotHeartbeat = r.Header.Get("x-freebuff-heartbeat")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123"}`)
		}

		if err := mgr.Poll(context.Background()); err != nil {
			t.Fatalf("Poll: %v", err)
		}
		// Gap #2: the CLI never beats — x-freebuff-heartbeat is
		// Desktop-only (upstream/freebuff freebuff-models.ts:1212-1215);
		// liveness comes from the recurring compact GET.
		if gotCompact != "1" {
			t.Errorf("x-freebuff-compact-session = %q, want 1", gotCompact)
		}
		if gotHeartbeat != "" {
			t.Errorf("x-freebuff-heartbeat = %q, want absent on polls", gotHeartbeat)
		}
		if snap := mgr.Snapshot(); snap.Status != "active" {
			t.Errorf("status = %q, want active", snap.Status)
		}
	})

	t.Run("ended session status invalidates state", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		_, err := mgr.EnsureSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123"}`)
		}

		if err := mgr.Poll(context.Background()); err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if snap := mgr.Snapshot(); snap.Status != "" {
			t.Errorf("status = %q, want empty after invalidation", snap.Status)
		}
	})
}

// TestPollRidesGraceEndedWithInstance verifies the grace drain on the poll path: an
// "ended" response that still carries the instance id (with a future grace
// end) is kept as a usable ended-with-instance row — the fast path keeps
// serving it until grace closes, with no fresh admission.
func TestPollRidesGraceEndedWithInstance(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	graceEnd := time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","gracePeriodEndsAt":"`+graceEnd+`"}`)
	}

	if err := mgr.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	snap := mgr.Snapshot()
	if snap.Status != "ended" || snap.InstanceID != "inst-abc-123" {
		t.Fatalf("snapshot = %+v, want ended inst-abc-123 (in-grace row kept)", snap)
	}

	// The fast path reuses the in-grace slot: no upstream create.
	creates := mock.SessionCreates
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q, want inst-abc-123 (ride through grace)", instance)
	}
	if mock.SessionCreates != creates {
		t.Errorf("session creates = %d, want %d (no fresh admission inside grace)", mock.SessionCreates, creates)
	}
}

// TestPollEndedPastGraceInvalidates verifies an "ended" poll response whose
// grace window has already closed (or that carries no instance id) drops the
// cached slot so the next EnsureSession re-creates fresh.
func TestPollEndedPastGraceInvalidates(t *testing.T) {
	for name, body := range map[string]string{
		"past grace":  `{"status":"ended","instanceId":"inst-abc-123","gracePeriodEndsAt":"2020-01-01T00:00:00Z"}`,
		"no instance": `{"status":"ended"}`,
	} {
		t.Run(name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mgr := newTestManager(t, mock)

			if _, err := mgr.EnsureSession(context.Background()); err != nil {
				t.Fatal(err)
			}

			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}

			if err := mgr.Poll(context.Background()); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			if snap := mgr.Snapshot(); snap.Status != "" {
				t.Errorf("status = %q, want empty after %s ended poll", snap.Status, name)
			}
		})
	}
}

// TestPollInvalidatesRecreateStatuses verifies Poll invalidates the cached
// admission for "superseded"/"none" polls exactly like "ended" (status
// parity for the poll path).
func TestPollInvalidatesRecreateStatuses(t *testing.T) {
	for _, status := range []string{"superseded", "none"} {
		t.Run(status, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mgr := newTestManager(t, mock)

			if _, err := mgr.EnsureSession(context.Background()); err != nil {
				t.Fatal(err)
			}

			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"`+status+`","instanceId":"inst-abc-123"}`)
			}

			if err := mgr.Poll(context.Background()); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			if snap := mgr.Snapshot(); snap.Status != "" {
				t.Errorf("status = %q, want empty after %s invalidation", snap.Status, status)
			}
		})
	}
}

// TestPollDropsRowOnWaitingRoomRequired verifies #116: a 428
// waiting_room_required poll response is session-ENDING
// (endsTheSession:true) — Poll drops the cached admission so the next
// EnsureSession re-admits fresh, and surfaces the typed error for the
// pool's failure backoff.
func TestPollDropsRowOnWaitingRoomRequired(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	var reAdmits atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// 428 on the poll GET (the compact session poll).
			w.WriteHeader(http.StatusTooEarly) // 428
			_, _ = io.WriteString(w, `{"error":"waiting_room_required"}`)
			return
		}
		// POST (re-admit after the 428 drop): a fresh active slot.
		reAdmits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","expiresAt":"2030-01-01T00:00:00Z"}`)
	}

	err := mgr.Poll(context.Background())
	if !errors.Is(err, upstream.ErrWaitingRoomRequired) {
		t.Fatalf("Poll error = %v, want ErrWaitingRoomRequired", err)
	}
	if snap := mgr.Snapshot(); snap.Status != "" {
		t.Errorf("status = %q, want empty (cached row dropped on 428)", snap.Status)
	}

	// The next EnsureSession re-admits fresh (the pool fires the
	// WAITING_ROOM_CHAIN before the create).
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := reAdmits.Load(); got != 1 {
		t.Errorf("re-admit session creates = %d, want 1 (fresh admission after 428 drop)", got)
	}
}

// TestRefreshDropsQueueRowOnWaitingRoomRequired verifies #140: a 428
// waiting_room_required on the queued row's refresh GET is session-ENDING
// (same as Poll's #116 handling) — refresh drops the dead queued row so the
// next EnsureSession re-admits fresh instead of GETting a dead row, and
// surfaces the typed error for the pool's failover.
func TestRefreshDropsQueueRowOnWaitingRoomRequired(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	var creates, polls atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// Create #1 returns a queued admission whose pollAt has already
			// passed so the next iteration advances to the queued-row GET;
			// the re-admit create after the 428 drop returns an active slot.
			if creates.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"queued","instanceId":"inst-q","position":1,"queueDepth":2,"estimatedWaitMs":100,"pollAt":"2000-01-01T00:00:00.000Z"}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","expiresAt":"2030-01-01T00:00:00Z"}`)
			return
		}
		// GET on the dead queued row → 428 waiting_room_required.
		polls.Add(1)
		w.WriteHeader(http.StatusTooEarly) // 428
		_, _ = io.WriteString(w, `{"error":"waiting_room_required"}`)
	}

	// Call 1: create → queued row cached → its refresh GET 428s → the row
	// must be dropped and the typed error surfaced.
	_, err := mgr.EnsureSessionForModel(context.Background(), "model/A")
	if !errors.Is(err, upstream.ErrWaitingRoomRequired) {
		t.Fatalf("EnsureSessionForModel error = %v, want ErrWaitingRoomRequired", err)
	}
	if snap := mgr.Snapshot(); snap.Status != "" {
		t.Errorf("status after 428 = %q, want empty (dead queued row dropped)", snap.Status)
	}

	// Call 2: the drop enables a fresh session CREATE (+1) instead of a
	// second GET on the dead row.
	if _, err := mgr.EnsureSessionForModel(context.Background(), "model/A"); err != nil {
		t.Fatal(err)
	}
	if got := creates.Load(); got != 2 {
		t.Errorf("session creates = %d, want 2 (fresh create after 428 drop)", got)
	}
	if got := polls.Load(); got != 1 {
		t.Errorf("queued-row GETs = %d, want 1 (no second GET on the dead row)", got)
	}
	if snap := mgr.Snapshot(); snap.Status != "active" {
		t.Errorf("status after re-admit = %q, want active", snap.Status)
	}
}

// TestPollTransportErrorKeepsCachedState verifies a transport error on the
// session poll surfaces as an error (pool backoff path) while the cached
// active admission stays intact — the transport failure did not prove the
// session dead.
func TestPollTransportErrorKeepsCachedState(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Subsequent poll GET hangs up the connection (transport error).
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("mock server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack failed: %v", err)
			return
		}
		_ = conn.Close()
	}

	if err := mgr.Poll(context.Background()); err == nil {
		t.Fatal("poll transport error must surface, got nil")
	}
	snap := mgr.Snapshot()
	if snap.Status != "active" {
		t.Errorf("status = %q, want active kept after transport error", snap.Status)
	}
	if snap.InstanceID != "inst-abc-123" {
		t.Errorf("instance = %q, want inst-abc-123 kept", snap.InstanceID)
	}
}

func TestPollStatusErrors(t *testing.T) {
	t.Run("banned returns BanError and clears cached admission", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}

		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"banned","resumes_at":"2026-08-16T12:00:00Z"}`)
		}

		err := mgr.Poll(context.Background())
		var be *upstream.BanError
		if !errors.As(err, &be) {
			t.Fatalf("want *upstream.BanError, got %v", err)
		}
		if !errors.Is(err, upstream.ErrBanned) {
			t.Error("not unwrap-able to ErrBanned")
		}
		if be.ResumesAt.IsZero() {
			t.Error("resumes_at not parsed into BanError")
		}
		if snap := mgr.Snapshot(); snap.Status != "" {
			t.Errorf("status = %q, want cleared after ban cooldown", snap.Status)
		}
	})

	t.Run("country_blocked returns CountryBlockedError", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}

		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"country_blocked","countryCode":"US","countryBlockReason":"region restricted","ipPrivacySignals":["proxy"]}`)
		}

		err := mgr.Poll(context.Background())
		var cbe *upstream.CountryBlockedError
		if !errors.As(err, &cbe) {
			t.Fatalf("want *upstream.CountryBlockedError, got %v", err)
		}
		if cbe.CountryCode != "US" || cbe.CountryBlockReason != "region restricted" {
			t.Errorf("country block fields = %+v", cbe)
		}
		if !errors.Is(err, upstream.ErrCountryBlocked) {
			t.Error("not unwrap-able to ErrCountryBlocked")
		}
	})

	t.Run("rate_limited returns RateLimitError", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}

		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"rate_limited","retryAfterMs":45000,"limit":5,"recentCount":5}`)
		}

		err := mgr.Poll(context.Background())
		var rle *upstream.RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("want *upstream.RateLimitError, got %v", err)
		}
		if !errors.Is(err, upstream.ErrRateLimited) {
			t.Error("not unwrap-able to ErrRateLimited")
		}
		if rle.RetryAfter != 45*time.Second {
			t.Errorf("RetryAfter = %s, want 45s", rle.RetryAfter)
		}
	})
}

// ── Wave 1 issue tests (#81) ─────────────────────────────────────────────

// TestPollIpCapped verifies #81: an ip_capped session status maps to
// the distinct upstream.IpCappedError (admission-only, bounded to
// retryAfterMs — never the Pacific-midnight quota lock), NOT a
// RateLimitError.
func TestPollIpCapped(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ip_capped","activeUsersForIp":5,"limit":4,"retryAfterMs":30000}`)
	}

	err := mgr.Poll(context.Background())
	if errors.Is(err, upstream.ErrRateLimited) {
		t.Fatal("ip_capped mapped to ErrRateLimited, want distinct ErrIpCapped")
	}
	var ice *upstream.IpCappedError
	if !errors.As(err, &ice) {
		t.Fatalf("want *upstream.IpCappedError, got %v", err)
	}
	if !errors.Is(err, upstream.ErrIpCapped) {
		t.Error("not unwrap-able to ErrIpCapped")
	}
	if ice.ActiveUsersForIP != 5 || ice.Limit != 4 {
		t.Errorf("IpCappedError = %+v, want ActiveUsersForIP 5 limit 4", ice)
	}
	if ice.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %s, want 30s (bounded to retryAfterMs only)", ice.RetryAfter)
	}
}

// TestSnapshotActiveUsersForIP verifies the admission response's
// activeUsersForIp is cached and exposed through SessionSnapshot for the
// pool snapshot (issue #81 "if cheap").
func TestSnapshotActiveUsersForIP(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","activeUsersForIp":3}`)
	}

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := mgr.Snapshot()
	if snap.ActiveUsersForIP != 3 {
		t.Errorf("Snapshot.ActiveUsersForIP = %d, want 3", snap.ActiveUsersForIP)
	}
	if snap.Status != "active" {
		t.Errorf("Status = %q, want active", snap.Status)
	}
}

// TestHeartbeatPollFields pins that the liveness poll's Debug line carries
// instance/ms/status so ops can see each heartbeat beat and its latency.
func TestHeartbeatPollFields(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	restore := captureLogs(&buf)
	defer restore()
	if err := mgr.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, `msg="session: heartbeat poll"`) ||
		!strings.Contains(got, "instance_id=") ||
		!strings.Contains(got, "ms=") ||
		!strings.Contains(got, "status=active") {
		t.Errorf("heartbeat poll log missing instance/ms/status:\n%s", got)
	}
}

// TestPollPersistedDropsRowOnWaitingRoomRequired verifies the persisted-
// resume half of the 428 waiting_room_required drop (#140 parity with the
// live Poll/refresh drop paths): an ENDED seat must not stay in the store,
// or every EnsureSession re-polls the dead row instead of re-admitting
// fresh.
func TestPollPersistedDropsRowOnWaitingRoomRequired(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(t.TempDir() + "/state.json")
	mgr, key := newPersistTestManager(t, mock, store)
	store.Save(key, activeSlot("inst-persist", ""))

	var polls, creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			polls.Add(1)
			w.WriteHeader(http.StatusTooEarly) // 428
			_, _ = io.WriteString(w, `{"error":"waiting_room_required"}`)
		case http.MethodPost:
			creates.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","expiresAt":"2030-01-01T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}

	_, err := mgr.EnsureSession(context.Background())
	if !errors.Is(err, upstream.ErrWaitingRoomRequired) {
		t.Fatalf("EnsureSession error = %v, want ErrWaitingRoomRequired", err)
	}
	if got := polls.Load(); got != 1 {
		t.Errorf("resume polls = %d, want 1 (dead slot polled once)", got)
	}
	if got := creates.Load(); got != 0 {
		t.Errorf("creates = %d, want 0 on the 428 call (no slot burned yet)", got)
	}
	// The dead row must be gone: the next EnsureSession re-admits fresh
	// instead of re-polling.
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := creates.Load(); got != 1 {
		t.Errorf("creates = %d, want 1 (fresh create after the 428 drop)", got)
	}
	if got := polls.Load(); got != 1 {
		t.Errorf("resume polls = %d, want 1 (no re-poll of the dropped row)", got)
	}
	if got := store.Load(key); got == nil || got.instanceID != "inst-abc-123" {
		t.Errorf("store after re-admit = %+v, want the fresh inst-abc-123 (dead inst-persist dropped)", got)
	}
}
