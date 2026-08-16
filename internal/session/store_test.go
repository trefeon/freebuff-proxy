package session

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

func newTestManagerWithStore(t *testing.T, mock *testutil.MockUpstream, store *Store) *Manager {
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
	return NewManagerWithStore(client, store)
}

func TestStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := NewStore(path)

	if got := store.Load("key"); got != nil {
		t.Fatalf("Load on empty store = %+v, want nil", got)
	}

	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	store.Save("key", &cachedState{
		status:            "active",
		instanceID:        "inst-1",
		model:             "m",
		expiresAt:         expiry,
		gracePeriodEndsAt: expiry.Add(graceWindow),
		accessTier:        "limited",
		countryCode:       "US",
	})

	// A second Store instance over the same file must see the write.
	store2 := NewStore(path)
	got := store2.Load("key")
	if got == nil {
		t.Fatal("Load after Save = nil")
	}
	if got.instanceID != "inst-1" || got.status != "active" {
		t.Errorf("Load = %+v, want inst-1/active", got)
	}
	if !got.expiresAt.Equal(expiry) {
		t.Errorf("expiresAt = %v, want %v", got.expiresAt, expiry)
	}

	store.Remove("key")
	if got := NewStore(path).Load("key"); got != nil {
		t.Errorf("Load after Remove = %+v, want nil", got)
	}
}

func TestStoreDropsExpiredGrace(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.Save("key", &cachedState{
		status:            "active",
		instanceID:        "inst-1",
		expiresAt:         time.Now().Add(-40 * time.Minute),
		gracePeriodEndsAt: time.Now().Add(-10 * time.Minute), // grace already closed
	})
	if got := store.Load("key"); got != nil {
		t.Fatalf("Load of expired entry = %+v, want nil", got)
	}
}

func TestShutdownKeepsSessionWhenPersist(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr := newTestManagerWithStore(t, mock, store)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreates != 1 {
		t.Fatalf("SessionCreates = %d, want 1", mock.SessionCreates)
	}

	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionEnds != 0 {
		t.Errorf("SessionEnds = %d, want 0 (session kept alive for restart)", mock.SessionEnds)
	}
	if got := store.Load(mgr.key); got == nil || got.instanceID != "inst-abc-123" {
		t.Errorf("store after Shutdown = %+v, want active inst-abc-123", got)
	}
}

func TestShutdownEndsSessionWithoutPersist(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionEnds != 1 {
		t.Errorf("SessionEnds = %d, want 1 (no persistence → DELETE upstream)", mock.SessionEnds)
	}
}

func TestResumePersistedOnRestart(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	path := filepath.Join(t.TempDir(), "state.json")

	// First process: create a session and shut down (keeps it alive).
	mgr1 := newTestManagerWithStore(t, mock, NewStore(path))
	if _, err := mgr1.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mgr1.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreates != 1 {
		t.Fatalf("SessionCreates after first process = %d, want 1", mock.SessionCreates)
	}

	// Second process (same token → same store key): must resume, not create.
	mgr2 := newTestManagerWithStore(t, mock, NewStore(path))
	instance, err := mgr2.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("resumed instance = %q, want inst-abc-123", instance)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("SessionCreates after resume = %d, want still 1 (no new quota burned)", mock.SessionCreates)
	}
	if mock.SessionPolls != 1 {
		t.Errorf("SessionPolls = %d, want 1 (resume poll)", mock.SessionPolls)
	}
}

func TestResumeSkipsDeadPersistedSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)

	// The upstream reports the persisted slot as ended, then serves a fresh
	// active session for the re-create.
	mock.SessionSequence = []string{"ended", "active"}

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
	store.Save(mgr.key, &cachedState{
		status:            "active",
		instanceID:        "inst-dead",
		expiresAt:         time.Now().Add(time.Hour),
		gracePeriodEndsAt: time.Now().Add(time.Hour + graceWindow),
	})

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The dead slot must have been discarded and a fresh session created.
	if mock.SessionCreates != 1 {
		t.Errorf("SessionCreates = %d, want 1 (dead slot re-created)", mock.SessionCreates)
	}
	got := store.Load(mgr.key)
	if got == nil || got.instanceID != "inst-abc-123" {
		t.Errorf("store after re-create = %+v, want fresh active inst-abc-123", got)
	}
}

func TestStoreFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)
	store.Save("key", &cachedState{status: "active", instanceID: "i", expiresAt: time.Now().Add(time.Hour)})

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Unix enforces 0600; Windows ignores the perm bits (only the read-only
	// attribute is honored), so the mode assertion is Unix-only.
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("state file mode = %o, want 600", perm)
		}
	}
}
