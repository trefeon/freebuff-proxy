package pool

import (
	"context"
	"encoding/json"
	"errors"
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

// probeActiveHandler serves the two session shapes bridge entries need in
// admission-error tests: the zero-cost token probe (GET without an instance
// header) must SUCCEED so the entry is created, while the session POST is
// scripted by the caller-provided refuse function and the session poll
// reports "ended".
func probeActiveHandler(refuse func(w http.ResponseWriter, r *http.Request)) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Header.Get("x-freebuff-instance-id") == "" {
			// Probe: active account state so bridgeEntryFor succeeds.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-probe","rateLimitsByModel":{}}`)
			return
		}
		if r.Method == http.MethodPost && refuse != nil {
			refuse(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ended"}`)
	}
}

// TestBridgeWaitingRoomChainFiresBeforeCreate verifies #94(b) on the bridge
// path: after a 428 waiting_room_required session create sets the client's
// gate flag, the NEXT AcquireBridge fires the reference pre-session ad-chain
// + streak requests before the session create (mirrors
// TestWaitingRoomChainFiresBeforeCreate for the fixed-token path).
func TestBridgeWaitingRoomChainFiresBeforeCreate(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var ads, streaks atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/ads":
			ads.Add(1)
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"ads":[]}`)
		case "/api/v1/freebuff/streak":
			streaks.Add(1)
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"streak":0,"todayUsed":0}`)
		case "/api/v1/freebuff/session/admission":
			if r.Method == http.MethodPost {
				// 428 waiting_room_required on every create: the client
				// classifies it and arms the gate flag. The second create
				// failing again is fine — the test only asserts the chain.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(428)
				_, _ = io.WriteString(w, `{"error":"waiting_room_required"}`)
				return
			}
			proxyToMock(w, r, mock)
		default:
			proxyToMock(w, r, mock)
		}
	}))
	defer srv.Close()

	// Build the pool manually (the bridge entry's upstream client is built
	// from the POOL config, so the pool must point at the wrapper).
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    srv.URL,
		WaitingRoomChain:   true,
	}
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = srv.URL
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

	// 1. First AcquireBridge: the create is refused 428, which classifies
	// the gate and arms the flag — no chain yet.
	_, err = p.AcquireBridge(context.Background(), "client-tok", modelA)
	if err == nil {
		t.Fatal("AcquireBridge succeeded, want 428 waiting_room_required")
		return
	}
	if ads.Load() != 0 || streaks.Load() != 0 {
		t.Fatalf("chain fired on first create: ads=%d streaks=%d, want 0/0", ads.Load(), streaks.Load())
	}

	// 2. Next AcquireBridge must fire the chain before the session create.
	_, err = p.AcquireBridge(context.Background(), "client-tok", modelA)
	if err == nil {
		t.Fatal("AcquireBridge succeeded, want 428")
		return
	}
	if ads.Load() == 0 || streaks.Load() == 0 {
		t.Errorf("waiting-room chain not fired before bridge session create: ads=%d streaks=%d (#94b)", ads.Load(), streaks.Load())
	}
}

// TestBridgeAdmissionBanNotifies verifies #48 parity: a session create
// refused 403 banned surfaces the ban error AND fires the token_banned
// webhook (token_index 0 = bridge) — previously only the chat path alerted.
func TestBridgeAdmissionBanNotifies(t *testing.T) {
	var posts atomic.Int64
	gotEvent := make(chan notify.Event, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		var ev notify.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		select {
		case gotEvent <- ev:
		default: // more than one webhook fired — keep the first
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = probeActiveHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"status":"banned","resumes_at":"`+time.Now().Add(time.Hour).Format(time.RFC3339)+`"}`)
	})

	p := newTestPoolCfg(t, func(c *config.Config) { c.UpstreamBaseURL = mock.URL() }, mock)
	p.SetNotifier(notify.New(srv.URL, nil))

	_, err := p.AcquireBridge(context.Background(), "client-tok", modelA)
	if err == nil {
		t.Fatal("AcquireBridge succeeded, want ban error")
		return
	}
	testutil.WaitFor(t, 3*time.Second, func() bool { return posts.Load() == 1 },
		fmt.Sprintf("token_banned webhook posts = %d, want 1", posts.Load()))
	var ev notify.Event
	select {
	case ev = <-gotEvent:
	case <-time.After(time.Second):
		t.Fatal("webhook event never arrived")
	}
	if ev.Event != "token_banned" {
		t.Errorf("event = %q, want token_banned", ev.Event)
	}
	if ev.TokenIndex != 0 {
		t.Errorf("token_index = %d, want 0 (bridge)", ev.TokenIndex)
	}
}

// TestBridgeAdmissionLimitedIpMarksModel verifies #74 on the bridge
// path: a session POST refused with the limited_ip shape surfaces
// *upstream.LimitedIpError from AcquireBridge AND marks the (egress, model)
// pair unfit so pooled requests refuse fast (the bridge gate itself stays
// skipped by design — bridge clients keep their own token).
func TestBridgeAdmissionLimitedIpMarksModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = probeActiveHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, limitedBody)
	})

	p := newTestPoolCfg(t, func(c *config.Config) { c.UpstreamBaseURL = mock.URL() }, mock)

	_, err := p.AcquireBridge(context.Background(), "client-tok", modelA)
	var lie *upstream.LimitedIpError
	if !errors.As(err, &lie) {
		t.Fatalf("AcquireBridge err = %v, want *upstream.LimitedIpError", err)
	}
	if !errors.Is(err, upstream.ErrModelIPLimited) {
		t.Error("AcquireBridge err not unwrap-able to ErrModelIPLimited")
	}
	until, got := p.ModelUnfit(modelA)
	if until.IsZero() {
		t.Fatal("ModelUnfit after bridge limited_ip admission = zero time, want marked")
	}
	if got == nil || got.Model != modelA {
		t.Errorf("ModelUnfit lie = %v, want marked lie for %s", got, modelA)
	}
}
