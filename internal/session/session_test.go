package session

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

func newTestManager(t *testing.T, mock *testutil.MockUpstream) *Manager {
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
	return NewManager(client)
}

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

func TestWaitingRoomThenActive(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued", "active"}
	mock.EstimatedWaitMs = 400
	mock.QueuePosition = 2
	mock.QueueDepth = 5
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSession(context.Background())
	var wr *WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("want WaitingRoomError, got %v", err)
	}
	if wr.Position != 2 || wr.QueueDepth != 5 {
		t.Errorf("waiting room position/depth = %d/%d", wr.Position, wr.QueueDepth)
	}
	if wr.RetryAfter <= 0 || wr.RetryAfter > time.Second {
		t.Errorf("RetryAfter = %s, want ~400ms", wr.RetryAfter)
	}

	time.Sleep(500 * time.Millisecond)
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionPolls != 1 {
		t.Errorf("polls = %d, want 1", mock.SessionPolls)
	}
}

func TestDisabled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "disabled"
	mgr := newTestManager(t, mock)

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "" {
		t.Errorf("instance = %q, want empty for disabled", instance)
	}
}

func TestEndedRecreates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"ended", "active"}
	mgr := newTestManager(t, mock)

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (ended → recreate)", mock.SessionCreates)
	}
}

func TestExpiredCacheRefreshes(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ExpiresIn = -1 * time.Minute // already past expiry margin
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

	// Second call: stale cache → refresh (create #2).
	instance, err = mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (stale cache → refresh)", mock.SessionCreates)
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

func TestConcurrentQueuedSharedState(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued", "active"}
	mock.EstimatedWaitMs = 100
	mgr := newTestManager(t, mock)

	var wg sync.WaitGroup
	waitRooms := make([]bool, 8)
	instances := make([]string, 8)
	for i := range waitRooms {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			instance, err := mgr.EnsureSession(context.Background())
			if err == nil {
				instances[i] = instance
			} else {
				var wr *WaitingRoomError
				waitRooms[i] = errors.As(err, &wr)
			}
		}(i)
	}
	wg.Wait()

	// All callers must either get the waiting room error or the instance;
	// the shared state must not race (exercised under -race).
	gotInstance := false
	for i := range waitRooms {
		if !waitRooms[i] && instances[i] == "" {
			t.Errorf("caller %d: neither waiting room nor instance", i)
		}
		if instances[i] != "" {
			gotInstance = true
		}
	}
	if !gotInstance {
		// All queued is legal; but then no one may hold garbage.
		t.Log("all callers observed the waiting room")
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

func TestCtxCancelPropagates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionCreateDelay = 2 * time.Second
	mgr := newTestManager(t, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := mgr.EnsureSession(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestBannedSessionReturnsError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "banned"
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("want error for banned session")
	}
	if !strings.Contains(err.Error(), "banned") {
		t.Errorf("error = %q, want banned message", err)
	}
}

func TestModelLockedRecreates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"model_locked", "active"}
	mgr := newTestManager(t, mock)

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (model_locked → recreate)", mock.SessionCreates)
	}
}
func TestModelUnavailableFallback(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var createdModels []string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			model := r.Header.Get("x-freebuff-model")
			createdModels = append(createdModels, model)
			w.Header().Set("Content-Type", "application/json")
			if model == "rare/model" {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"status":"model_unavailable","requestedModel":"rare/model"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-fallback","model":"`+model+`","expiresAt":"2030-01-01T00:00:00Z"}`)
			return
		}
		http.NotFound(w, r)
	}

	mgr := newTestManager(t, mock)
	instance, err := mgr.EnsureSessionForModel(context.Background(), "rare/model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if instance != "inst-fallback" {
		t.Errorf("instance = %q, want inst-fallback", instance)
	}
	if len(createdModels) != 2 {
		t.Fatalf("createdModels = %v, want 2 attempts", createdModels)
	}
	if createdModels[0] != "rare/model" || createdModels[1] != "deepseek/deepseek-v4-flash" {
		t.Errorf("createdModels = %v, want rare/model then fallback", createdModels)
	}
}

func TestRateLimitedError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"status":"rate_limited","retryAfterMs":45000,"limit":5,"recentCount":5}`)
	}

	mgr := newTestManager(t, mock)
	_, err := mgr.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("want error on rate limited session")
	}
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want RateLimitError, got %T: %v", err, err)
	}
	if rle.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %v, want 45s", rle.RetryAfter)
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
}
