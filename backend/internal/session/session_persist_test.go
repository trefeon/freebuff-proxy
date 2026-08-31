package session

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// newPersistTestManager builds a manager wired to a store, like
// newTestManagerWithStore (store_test.go), but returns the store key too so
// tests can seed/assert the persisted slot. The key is the SHA-256 hash of
// the token ("tok"), derived the same way the manager derives it.
func newPersistTestManager(t *testing.T, mock *testutil.MockUpstream, store *Store) (*Manager, string) {
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
	mgr := NewManagerWithStore(client, store)
	return mgr, client.TokenKey()
}

// activeSlot is a persisted active cachedState that pollPersisted would
// consider resumable: unexpired with the grace window still open.
func activeSlot(instanceID, model string) *cachedState {
	expiry := time.Now().Add(time.Hour)
	return &cachedState{
		status:            "active",
		instanceID:        instanceID,
		model:             model,
		expiresAt:         expiry,
		gracePeriodEndsAt: expiry.Add(graceWindow),
	}
}

// TestPersistResumePollTransportError verifies a transport failure on the
// resume poll surfaces as a refresh error (single-flight / TRANSIENT_RETRIES
// territory) instead of being swallowed and falling through to a fresh
// create that burns a daily session slot. The persisted slot is also left in
// place: the transport failure did not prove it dead.
func TestPersistResumePollTransportError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr, key := newPersistTestManager(t, mock, store)
	store.Save(key, activeSlot("inst-abc-123", ""))

	var polls, creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			polls.Add(1)
			// Transport failure: hang up the connection without a response
			// (verified: the Go transport does not retry, single EOF).
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
		case http.MethodPost:
			creates.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","expiresAt":"2030-01-01T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}

	_, err := mgr.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("resume poll transport error must surface, got nil")
	}
	if got := creates.Load(); got != 0 {
		t.Errorf("creates = %d, want 0 (transport error must not fall through to create)", got)
	}
	if got := polls.Load(); got != 1 {
		t.Errorf("polls = %d, want 1 (persisted slot polled once)", got)
	}
	if got := store.Load(key); got == nil || got.instanceID != "inst-abc-123" {
		t.Errorf("store after transport error = %+v, want intact inst-abc-123", got)
	}
}

// TestPersistModelMismatchNotAdopted verifies a persisted slot bound to a
// different model is never re-adopted: it is dropped so the refresh falls
// through to a create for the requested model. Both the pre-flight gate
// (persisted model known before the poll) and the post-flight gate (the
// upstream binds the resumed slot to a model) are exercised.
func TestPersistModelMismatchNotAdopted(t *testing.T) {
	t.Run("preflight persisted model mismatch", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)
		store.Save(key, activeSlot("inst-A", "model/A"))

		var mu sync.Mutex
		var createdModels []string
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				// Not reached: the pre-flight gate rejects before the poll.
				t.Errorf("unexpected resume poll for a model-mismatched slot")
			case http.MethodPost:
				mu.Lock()
				createdModels = append(createdModels, r.Header.Get("x-freebuff-model"))
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-B","model":"`+r.Header.Get("x-freebuff-model")+`","expiresAt":"2030-01-01T00:00:00Z"}`)
			default:
				http.NotFound(w, r)
			}
		}

		// Direct gate check: pollPersisted must refuse and drop the slot.
		st, err := mgr.pollPersisted(context.Background(), "model/B")
		if err != nil {
			t.Fatalf("pollPersisted: %v", err)
		}
		if st != nil {
			t.Fatalf("pollPersisted adopted %q for model/B, want nil", st.InstanceID)
		}
		if got := store.Load(key); got != nil {
			t.Fatalf("store after mismatch pollPersisted = %+v, want nil (slot dropped)", got)
		}

		// Integration: the refresh falls through to a create for model/B.
		store.Save(key, activeSlot("inst-A", "model/A"))
		instance, err := mgr.EnsureSessionForModel(context.Background(), "model/B")
		if err != nil {
			t.Fatal(err)
		}
		if instance != "inst-B" {
			t.Errorf("instance = %q, want inst-B (fresh create for model/B, not inst-A)", instance)
		}
		mu.Lock()
		gotModels := append([]string(nil), createdModels...)
		mu.Unlock()
		if len(gotModels) != 1 || gotModels[0] != "model/B" {
			t.Errorf("created models = %v, want [model/B]", gotModels)
		}
	})

	t.Run("postflight upstream model mismatch", func(t *testing.T) {
		// The persisted entry carries no model (e.g. written before model
		// tracking), but the upstream binds the resumed slot to model/A.
		mock := testutil.NewMock()
		defer mock.Close()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)
		store.Save(key, activeSlot("inst-A", ""))

		var polls atomic.Int32
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				polls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-A","model":"model/A","expiresAt":"2030-01-01T00:00:00Z"}`)
			case http.MethodPost:
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-B","model":"model/B","expiresAt":"2030-01-01T00:00:00Z"}`)
			default:
				http.NotFound(w, r)
			}
		}

		instance, err := mgr.EnsureSessionForModel(context.Background(), "model/B")
		if err != nil {
			t.Fatal(err)
		}
		if instance != "inst-B" {
			t.Errorf("instance = %q, want inst-B (post-flight mismatch → create)", instance)
		}
		if got := polls.Load(); got != 1 {
			t.Errorf("polls = %d, want 1 (slot polled once, then rejected)", got)
		}
		// The model/A slot was dropped; the store now holds the model/B session.
		if got := store.Load(key); got == nil || got.instanceID != "inst-B" {
			t.Errorf("store = %+v, want model/B session inst-B", got)
		}
	})
}

// TestPersistModelMatchAdopted verifies a persisted slot whose model matches
// the request (or carries no model for a default-model request) is adopted
// without a fresh create.
func TestPersistModelMatchAdopted(t *testing.T) {
	t.Run("same model", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)
		store.Save(key, activeSlot("inst-A", "model/A"))

		var polls, creates atomic.Int32
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				polls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-A","model":"model/A","expiresAt":"2030-01-01T00:00:00Z"}`)
			case http.MethodPost:
				creates.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-X","expiresAt":"2030-01-01T00:00:00Z"}`)
			default:
				http.NotFound(w, r)
			}
		}

		instance, err := mgr.EnsureSessionForModel(context.Background(), "model/A")
		if err != nil {
			t.Fatal(err)
		}
		if instance != "inst-A" {
			t.Errorf("instance = %q, want inst-A (adopted, not created)", instance)
		}
		if got := creates.Load(); got != 0 {
			t.Errorf("creates = %d, want 0", got)
		}
		if got := polls.Load(); got != 1 {
			t.Errorf("polls = %d, want 1", got)
		}
	})

	t.Run("default model adopts any slot", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)
		store.Save(key, activeSlot("inst-A", "model/A"))

		var creates atomic.Int32
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-A","model":"model/A","expiresAt":"2030-01-01T00:00:00Z"}`)
			case http.MethodPost:
				creates.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-X","expiresAt":"2030-01-01T00:00:00Z"}`)
			default:
				http.NotFound(w, r)
			}
		}

		instance, err := mgr.EnsureSession(context.Background()) // default model ""
		if err != nil {
			t.Fatal(err)
		}
		if instance != "inst-A" {
			t.Errorf("instance = %q, want inst-A (default-model request adopts)", instance)
		}
		if got := creates.Load(); got != 0 {
			t.Errorf("creates = %d, want 0", got)
		}
	})
}

// TestShutdownAlwaysDeletesEvenWhenPersisting verifies that shutdown
// ALWAYS releases the upstream session slot (DELETE) whether or not
// persistence is enabled — the CLI DELETEs on exit. The store entry is
// still written (and survives the DELETE) so a restart can resume via
// pollPersisted, which drops the dead entry and re-POSTs fresh when the
// DELETE took effect upstream.
func TestShutdownAlwaysDeletesEvenWhenPersisting(t *testing.T) {
	t.Run("active unexpired deleted but entry kept", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)

		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := mgr.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if mock.SessionEnds != 0 {
			t.Errorf("SessionEnds = %d, want 0 (active session KEPT for restart-resume when persisting)", mock.SessionEnds)
		}
		if got := store.Load(key); got == nil || got.instanceID != "inst-abc-123" {
			t.Errorf("store after Shutdown = %+v, want active inst-abc-123 (entry survives the DELETE)", got)
		}
	})

	t.Run("queued ends upstream and entry kept", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionSequence = []string{"queued"}
		mock.EstimatedWaitMs = 60000 // pollAt far in the future
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)

		if _, err := mgr.EnsureSession(context.Background()); err == nil {
			t.Fatal("want WaitingRoomError for queued session")
		} else {
			var wr *WaitingRoomError
			if !errors.As(err, &wr) {
				t.Fatalf("err = %v, want WaitingRoomError", err)
			}
		}
		if got := store.Load(key); got == nil || got.status != "queued" {
			t.Fatalf("store before Shutdown = %+v, want queued entry", got)
		}

		if err := mgr.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if mock.SessionEnds != 1 {
			t.Errorf("SessionEnds = %d, want 1 (queued releases the upstream slot)", mock.SessionEnds)
		}
		// The queued entry survives the DELETE; a restart ignores it
		// (pollPersisted only resumes active slots) and re-POSTs fresh.
		if got := store.Load(key); got == nil || got.status != "queued" {
			t.Errorf("store after Shutdown = %+v, want queued entry kept", got)
		}
	})

	t.Run("expired active deleted but entry kept", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)

		// Craft an active-but-expired cached state (expiry margin already
		// passed) and persist it the way a live manager would.
		mgr.mu.Lock()
		expired := &cachedState{
			status:     "active",
			instanceID: "inst-expired",
			expiresAt:  time.Now().Add(-expiryMargin - time.Second),
		}
		mgr.commit(expired)
		mgr.mu.Unlock()

		if err := mgr.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if mock.SessionEnds != 0 {
			t.Errorf("SessionEnds = %d, want 0 (expired-but-in-grace session kept for restart-resume when persisting)", mock.SessionEnds)
		}
		if got := store.Load(key); got == nil || got.instanceID != "inst-expired" {
			t.Errorf("store after Shutdown = %+v, want inst-expired entry kept", got)
		}
	})
}

// TestShutdownPersistedDisabledNoEnd verifies a disabled session under
// persistence is neither DELETE'd upstream nor persisted: there is no slot to
// keep alive and nothing to resume.
func TestShutdownPersistedDisabledNoEnd(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "disabled"
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr, key := newPersistTestManager(t, mock, store)

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "" {
		t.Fatalf("instance = %q, want empty (disabled)", instance)
	}

	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionEnds != 0 {
		t.Errorf("SessionEnds = %d, want 0 (disabled: no upstream slot to release)", mock.SessionEnds)
	}
	if got := store.Load(key); got != nil {
		t.Errorf("store after Shutdown = %+v, want nil (disabled sessions are never persisted)", got)
	}
}

// TestRestartWithPersistedQueuedCreatesFresh verifies a persisted queued
// entry is ignored on restart (pollPersisted only resumes active slots):
// the manager falls through to a fresh create without polling, and the store
// is eventually overwritten with the active session.
func TestRestartWithPersistedQueuedCreatesFresh(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr, key := newPersistTestManager(t, mock, store)

	store.Save(key, &cachedState{
		status:     "queued",
		instanceID: "inst-q",
		model:      "m",
		pollAt:     time.Now().Add(time.Hour),
	})

	var polls, creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			polls.Add(1)
			http.NotFound(w, r) // must not be reached; visible as a poll count
		case http.MethodPost:
			creates.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","expiresAt":"2030-01-01T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q, want inst-abc-123 (fresh create, queued slot not resumable)", instance)
	}
	if got := creates.Load(); got != 1 {
		t.Errorf("creates = %d, want 1 (fresh create)", got)
	}
	if got := polls.Load(); got != 0 {
		t.Errorf("polls = %d, want 0 (pollPersisted returns before any GET for a queued entry)", got)
	}
	if got := store.Load(key); got == nil || got.instanceID != "inst-abc-123" {
		t.Errorf("store after create = %+v, want active inst-abc-123 (queued entry overwritten)", got)
	}
}

// TestConcurrentInvalidateEnsureSession hammers Invalidate() against
// EnsureSession from many goroutines (run with -race): the single-flight
// refresh and invalidation must not race on the cached state, and the final
// state must be a valid active session. Under this adversarial invalidation
// load a caller may legitimately hit the designed bounded failure mode
// ("not ready after repeated refreshes" — every refresh's commit was
// invalidated before the caller could act); that error is allowed, any other
// error is not.
func TestConcurrentInvalidateEnsureSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	const workers = 8
	const iterations = 100
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if (w+i)%2 == 0 {
					if _, err := mgr.EnsureSession(context.Background()); err != nil {
						// Invalidate racing the commit starves the session:
						// the outer budget is a designed bounded failure, not
						// a race corruption.
						if !strings.Contains(err.Error(), "not ready after repeated refreshes") {
							t.Errorf("EnsureSession: %v", err)
							return
						}
					}
				} else {
					mgr.Invalidate()
				}
			}
		}(w)
	}
	wg.Wait()

	// Final state must be a valid active session.
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("final instance = %q, want inst-abc-123", instance)
	}
}

// TestPersistAdoptThenModelUnavailableDropsSlot covers the adopt-then-unavailable edge: a fresh
// manager ADOPTS a persisted compatible slot, then a live refresh for an
// unavailable model drops the adopted slot (commit(nil)) and falls back to a
// DefaultFallbackModel create — the persisted slot must not survive as a
// stale store entry.
func TestPersistAdoptThenModelUnavailableDropsSlot(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr, key := newPersistTestManager(t, mock, store)
	store.Save(key, activeSlot("inst-A", "model/A"))

	var mu sync.Mutex
	var createdModels []string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-A","model":"model/A","expiresAt":"2030-01-01T00:00:00Z"}`)
		case http.MethodPost:
			model := r.Header.Get("x-freebuff-model")
			mu.Lock()
			createdModels = append(createdModels, model)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if model == "rare/model" {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"status":"model_unavailable","requestedModel":"rare/model"}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-fallback","model":"`+model+`","expiresAt":"2030-01-01T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}

	// Fresh manager: the persisted model/A slot is adopted (no create).
	if _, err := mgr.EnsureSessionForModel(context.Background(), "model/A"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	afterAdopt := len(createdModels)
	mu.Unlock()
	if afterAdopt != 0 {
		t.Errorf("creates after adopt = %d, want 0 (persisted slot adopted, not created)", afterAdopt)
	}

	// Live refresh for an unavailable model: fallback drops the adopted slot
	// from the store and creates on the default fallback model.
	if _, err := mgr.EnsureSessionForModel(context.Background(), "rare/model"); err != nil {
		t.Fatal(err)
	}
	if got := store.Load(key); got == nil || got.instanceID != "inst-fallback" {
		t.Errorf("store after fallback = %+v, want inst-fallback (adopted slot replaced)", got)
	}
	mu.Lock()
	gotModels := append([]string(nil), createdModels...)
	mu.Unlock()
	if len(gotModels) != 2 || gotModels[0] != "rare/model" || gotModels[1] != DefaultFallbackModel {
		t.Errorf("created models = %v, want [rare/model %s]", gotModels, DefaultFallbackModel)
	}
}

// TestPersistStoreNotConsultedOnLiveRefresh verifies the store is only
// consulted when the manager is fresh (cached == nil). A live model-mismatch
// refresh must create for the requested model without polling the persisted
// slot — even when a compatible slot is present, adopting it would pin the
// old model's session on every refresh. The seeded slot carries no model, so
// any store consultation would be visible as a resume poll (GET).
func TestPersistStoreNotConsultedOnLiveRefresh(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr, key := newPersistTestManager(t, mock, store)

	var polls atomic.Int32
	var mu sync.Mutex
	var createdModels []string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			polls.Add(1)
			// Compatible with any requested model: adopted whenever the
			// store is consulted.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-store","expiresAt":"2030-01-01T00:00:00Z"}`)
		case http.MethodPost:
			mu.Lock()
			createdModels = append(createdModels, r.Header.Get("x-freebuff-model"))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-created","model":"`+r.Header.Get("x-freebuff-model")+`","expiresAt":"2030-01-01T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}

	// First call: fresh manager → the store is consulted and the persisted
	// slot adopted.
	store.Save(key, activeSlot("inst-store", ""))
	instance, err := mgr.EnsureSessionForModel(context.Background(), "model/A")
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-store" {
		t.Errorf("first call instance = %q, want inst-store (fresh manager adopts)", instance)
	}
	if got := polls.Load(); got != 1 {
		t.Errorf("polls after first call = %d, want 1", got)
	}

	// Live manager, model mismatch: the store must NOT be consulted even
	// though a compatible slot is present (re-seeded model-less so the
	// pre-flight gate cannot hide a consultation).
	store.Save(key, activeSlot("inst-store", ""))
	instance, err = mgr.EnsureSessionForModel(context.Background(), "model/B")
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-created" {
		t.Errorf("mismatch refresh instance = %q, want inst-created (created, not adopted)", instance)
	}
	if got := polls.Load(); got != 1 {
		t.Errorf("polls after mismatch refresh = %d, want still 1 (store not consulted)", got)
	}
	mu.Lock()
	gotModels := append([]string(nil), createdModels...)
	mu.Unlock()
	if len(gotModels) != 1 || gotModels[0] != "model/B" {
		t.Errorf("created models = %v, want [model/B]", gotModels)
	}
}

func TestPersistQuotaByModelRoundTrip(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	key := "test-token-key"

	resetAt := time.Now().Add(12 * time.Hour).Truncate(time.Second)
	slot := activeSlot("inst-quota-123", "deepseek/deepseek-v4-flash")
	slot.quotaByModel = map[string]upstream.ModelQuota{
		"deepseek/deepseek-v4-flash": {
			Model:       "deepseek/deepseek-v4-flash",
			Limit:       5,
			RecentCount: 2,
			ResetAt:     resetAt,
			Period:      "pacific_day",
			Entitlement: map[string]float64{"base": 5},
		},
	}

	store.Save(key, slot)

	// Fresh store load from disk
	store2 := NewStore(store.path)
	loaded := store2.Load(key)
	if loaded == nil {
		t.Fatal("loaded state is nil")
	}
	if loaded.instanceID != "inst-quota-123" {
		t.Errorf("loaded instanceID = %q, want inst-quota-123", loaded.instanceID)
	}
	q, ok := loaded.quotaByModel["deepseek/deepseek-v4-flash"]
	if !ok {
		t.Fatal("quotaByModel missing deepseek/deepseek-v4-flash")
	}
	if q.Limit != 5 || q.RecentCount != 2 {
		t.Errorf("quota limit/recent = %v/%v, want 5/2", q.Limit, q.RecentCount)
	}
	if q.Period != "pacific_day" {
		t.Errorf("quota period = %q, want pacific_day", q.Period)
	}
	if q.Entitlement["base"] != 5 {
		t.Errorf("quota entitlement base = %v, want 5", q.Entitlement["base"])
	}
}
