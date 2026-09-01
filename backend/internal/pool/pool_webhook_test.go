package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/notify"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// TestAcquireFiresPoolExhaustedWebhook verifies #48: when every token is
// rate-limited, Acquire surfaces the 429 AND fires the pool_exhausted
// webhook (one POST per event type per 5m — the second Acquire is
// throttled).
func TestAcquireFiresPoolExhaustedWebhook(t *testing.T) {
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimit = true

	p := newTestPool(t, mock)
	p.SetNotifier(notify.New(srv.URL, nil))

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil {
		t.Fatal("Acquire succeeded, want rate-limit error")
	}
	// The webhook POST is fire-and-forget: wait for it.
	testutil.WaitFor(t, 3*time.Second, func() bool { return posts.Load() == 1 },
		fmt.Sprintf("pool_exhausted webhook posts = %d, want 1", posts.Load()))

	// Second exhausted Acquire within the 5m throttle window: no new POST.
	_, _ = p.Acquire(context.Background(), modelA)
	time.Sleep(50 * time.Millisecond)
	if posts.Load() != 1 {
		t.Fatalf("webhook posts = %d, want still 1 (throttled)", posts.Load())
	}
}

// TestAcquireFiresPoolExhaustedPayload checks the event contract: event
// name, model, message, RFC3339 timestamp.
func TestAcquireFiresPoolExhaustedPayload(t *testing.T) {
	var got atomic.Value // notify.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err == nil {
			got.Store(ev)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimit = true

	p := newTestPool(t, mock)
	p.SetNotifier(notify.New(srv.URL, nil))
	_, _ = p.Acquire(context.Background(), modelA)

	testutil.WaitFor(t, 3*time.Second, func() bool {
		ev, ok := got.Load().(notify.Event)
		return ok && ev.Event != ""
	}, "webhook payload never arrived")
	ev := got.Load().(notify.Event)
	if ev.Event != "pool_exhausted" || ev.Model != modelA || ev.TokenIndex != 0 {
		t.Fatalf("event = %+v, want pool_exhausted for model %s", ev, modelA)
	}
	if _, err := time.Parse(time.RFC3339, ev.Timestamp); err != nil {
		t.Fatalf("timestamp %q not RFC3339", ev.Timestamp)
	}
}

// TestCooldownTokenBanFiresWebhook verifies #48: classifying a ban fires
// the token_banned webhook with the 1-based token index (both the Acquire
// fail-path and the CooldownTokenBan path through the same sender).
func TestCooldownTokenBanFiresWebhook(t *testing.T) {
	var got atomic.Value // notify.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err == nil {
			got.Store(ev)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true

	p := newTestPool(t, mock)
	p.SetNotifier(notify.New(srv.URL, nil))

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil {
		t.Fatal("Acquire succeeded, want ban error")
	}
	testutil.WaitFor(t, 3*time.Second, func() bool {
		ev, ok := got.Load().(notify.Event)
		return ok && ev.Event != ""
	}, "token_banned webhook never arrived")
	ev := got.Load().(notify.Event)
	if ev.Event != "token_banned" {
		t.Fatalf("event = %q, want token_banned", ev.Event)
	}
	if ev.TokenIndex != 1 {
		t.Errorf("token_index = %d, want 1 (1-based pooled index)", ev.TokenIndex)
	}
}

// chainTracker is a wrapper server that counts /api/v1/ads +
// /api/v1/freebuff/streak hits and forwards everything else to the mock.
type chainTracker struct {
	ads     atomic.Int64
	streaks atomic.Int64
	srv     *httptest.Server
}

func newChainTracker(mock *testutil.MockUpstream) *chainTracker {
	ct := &chainTracker{}
	ct.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/ads":
			ct.ads.Add(1)
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"ads":[]}`)
		case "/api/v1/freebuff/streak":
			ct.streaks.Add(1)
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"streak":0,"todayUsed":0}`)
		default:
			proxyToMock(w, r, mock)
		}
	}))
	return ct
}

// proxyToMock forwards a request to the mock upstream and copies the
// response back (used by the chain-count wrapper server so sessions/runs/
// chat keep working against the real mock).
func proxyToMock(w http.ResponseWriter, r *http.Request, mock *testutil.MockUpstream) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, mock.URL()+r.URL.RequestURI(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	req.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// TestWaitingRoomChainFiresBeforeCreate verifies #94(b): with
// WAITING_ROOM_CHAIN enabled, after the token's client classifies a 428
// waiting_room_required the NEXT Acquire fires the reference pre-session
// ad-chain + streak requests before the session create.
func TestWaitingRoomChainFiresBeforeCreate(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ct := newChainTracker(mock)
	defer ct.srv.Close()

	// Build the pool manually (newTestPoolCfg pins the client to the mock,
	// but the chain must be observable, so the client upstream is the
	// tracker, which forwards session/run/chat to the mock).
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    ct.srv.URL,
		WaitingRoomChain:   true,
	}
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = ct.srv.URL
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := New(cfg, []*upstream.Client{client}, []*session.Manager{session.NewManager(client)}, reg)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Acquire a lease and run one chat against the mock serving 428
	// waiting_room_required: the client classifies it and sets the flag.
	mock.ChatStatus = 428
	mock.ChatErrorBody = `{"error":"waiting_room_required"}`
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	opts := upstream.ChatOptions{Model: modelA, RunID: lease.Run.RunID, SessionInstanceID: lease.SessionInstanceID}
	if _, err := p.Chat(context.Background(), lease, opts, []byte(`{"model":"m"}`)); err == nil {
		t.Fatal("Chat succeeded, want 428")
	}
	p.LeaseRelease(lease)

	// 2. Reset the counters, then a fresh Acquire must fire the chain.
	ct.ads.Store(0)
	ct.streaks.Store(0)
	mock.ChatStatus = 200
	mock.ChatErrorBody = ""
	lease, err = p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("Acquire after 428: %v", err)
	}
	p.LeaseRelease(lease)
	if ct.ads.Load() == 0 {
		t.Error("ad-chain request not fired before session create (#94b)")
	}
	if ct.streaks.Load() == 0 {
		t.Error("streak request not fired before session create (#94b)")
	}
}

// TestWaitingRoomChainOffByDefault verifies the gate is inert when
// WAITING_ROOM_CHAIN is disabled: after a 428 the next Acquire fires no
// chain requests.
func TestWaitingRoomChainOffByDefault(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ct := newChainTracker(mock)
	defer ct.srv.Close()

	p := newTestPoolCfg(t, func(c *config.Config) { c.UpstreamBaseURL = ct.srv.URL }, mock)
	mock.ChatStatus = 428
	mock.ChatErrorBody = `{"error":"waiting_room_required"}`
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	opts := upstream.ChatOptions{Model: modelA, RunID: lease.Run.RunID, SessionInstanceID: lease.SessionInstanceID}
	if _, err := p.Chat(context.Background(), lease, opts, []byte(`{"model":"m"}`)); err == nil {
		t.Fatal("Chat succeeded, want 428")
	}
	p.LeaseRelease(lease)

	mock.ChatStatus = 200
	mock.ChatErrorBody = ""
	lease, err = p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	p.LeaseRelease(lease)
	if ct.ads.Load() != 0 || ct.streaks.Load() != 0 {
		t.Errorf("chain fired with WAITING_ROOM_CHAIN off: ads=%d streaks=%d", ct.ads.Load(), ct.streaks.Load())
	}
}
