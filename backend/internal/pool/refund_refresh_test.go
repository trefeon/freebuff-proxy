package pool

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// activeSessionHandler serves the minimal active-session shape for admits
// (mirroring the mock's default create) while delegating DELETEs to fn.
func activeSessionHandler(fn func(w http.ResponseWriter, r *http.Request)) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			fn(w, r)
			return
		}
		expiresAt := time.Now().Add(30 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","expiresAt":"`+expiresAt+`"}`)
	}
}

// TestRefreshTokenRefundSettlesSameInstance pins the on-view refresh trigger:
// a parked pending refund replays its DELETE with the SAME instance id, the
// settled receipt records last_refund (and clears pending) on the pool
// snapshot the dashboard token card reads, and a repeat call with nothing
// parked is a no-op without upstream traffic.
func TestRefreshTokenRefundSettlesSameInstance(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	ctx := context.Background()

	var deletes atomic.Int64
	var mu sync.Mutex
	carried := []string{}
	mock.SessionHandler = activeSessionHandler(func(w http.ResponseWriter, r *http.Request) {
		n := deletes.Add(1)
		mu.Lock()
		carried = append(carried, r.Header.Get("x-freebuff-instance-id"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefundPending":true}`)
		} else {
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefund":1.5}`)
		}
	})
	if _, err := p.EnsureTokenSession(ctx, 0, modelB); err != nil {
		t.Fatalf("EnsureTokenSession: %v", err)
	}
	if err := p.DropTokenSession(ctx, 0); err != nil {
		t.Fatalf("DropTokenSession: %v", err)
	}
	if got := p.Snapshot()[0].PendingRefund; got != "inst-abc-123" {
		t.Fatalf("PendingRefund = %q, want inst-abc-123 (parked release)", got)
	}

	res, err := p.RefreshTokenRefund(ctx, 0)
	if err != nil {
		t.Fatalf("RefreshTokenRefund: %v", err)
	}
	if !res.Settled || res.Amount != 1.5 {
		t.Errorf("result = %+v, want Settled with Amount 1.5", res)
	}
	if deletes.Load() != 2 {
		t.Errorf("deletes = %d, want 2 (release + same-instance replay)", deletes.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(carried) != 2 || carried[0] != "inst-abc-123" || carried[1] != "inst-abc-123" {
		t.Errorf("replay instance ids = %q, want both inst-abc-123 (same-instance replay)", carried)
	}
	psnap := p.Snapshot()[0]
	if psnap.PendingRefund != "" {
		t.Errorf("pool PendingRefund = %q, want cleared after settle", psnap.PendingRefund)
	}
	if psnap.LastRefund == nil || *psnap.LastRefund != 1.5 {
		t.Errorf("pool LastRefund = %+v, want 1.5 (mirrored session snapshot)", psnap.LastRefund)
	}

	before := deletes.Load()
	res, err = p.RefreshTokenRefund(ctx, 0)
	if err != nil {
		t.Fatalf("repeat RefreshTokenRefund: %v", err)
	}
	if res.Settled || res.Pending || res.Dropped {
		t.Errorf("repeat result = %+v, want zero (nothing parked)", res)
	}
	if deletes.Load() != before {
		t.Errorf("deletes grew %d -> %d on a no-op refresh, want no upstream traffic", before, deletes.Load())
	}
}

// TestRefreshTokenRefundDropsOnAccountSwitch pins the current-token match
// guard (CLI refreshRefund): when the account at the slot changes mid-flight
// (AUTH_TOKENS reload rebuilds the entry), the replay ran against the
// retired entry, so the result is dropped and the replacement account shows
// neither a pending nor a settled refund.
func TestRefreshTokenRefundDropsOnAccountSwitch(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	ctx := context.Background()

	var deletes atomic.Int64
	arrived := make(chan struct{}, 8)
	release := make(chan struct{})
	// releaseAll unparks the blocked DELETE handlers. It MUST run on every
	// exit path: a Fatalf below Goexits the test goroutine, and a handler
	// still parked on <-release keeps its connection open, so the deferred
	// mock.Close() blocks until the go test timeout and the failure surfaces
	// as an opaque package timeout instead of the assertion that missed.
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	mock.SessionHandler = activeSessionHandler(func(w http.ResponseWriter, _ *http.Request) {
		n := deletes.Add(1)
		arrived <- struct{}{}
		if n >= 2 {
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefundPending":true}`)
		} else {
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefund":1.5}`)
		}
	})
	if _, err := p.EnsureTokenSession(ctx, 0, modelB); err != nil {
		t.Fatalf("EnsureTokenSession: %v", err)
	}
	if err := p.DropTokenSession(ctx, 0); err != nil {
		t.Fatalf("DropTokenSession: %v", err)
	}

	type outcome struct {
		res RefundRefreshResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := p.RefreshTokenRefund(ctx, 0)
		done <- outcome{res, err}
	}()
	// consumed tracks arrivals already attributed to counted DELETEs. Each
	// DELETE sends exactly one arrival, so consuming in order keeps the
	// channel in sync with the counter: a later wait cannot mistake a
	// stale buffered arrival for a new DELETE. (The old form seeded from
	// deletes.Load() but consumed from the channel, so a wait issued
	// after its DELETEs had already arrived could return on stale
	// arrivals — e.g. waitDeletes(3) returning with only 2 DELETEs, which
	// then surfaced as Settled instead of Dropped.)
	var consumed int64
	waitDeletes := func(n int64, what string) {
		t.Helper()
		for ; consumed < n; consumed++ {
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out waiting for %s DELETE (seen %d, want %d)", what, deletes.Load(), n)
			}
		}
	}
	// The replay DELETE is in flight (blocked on release).
	waitDeletes(2, "replay")

	// Swap the account mid-flight: SetConfig replaces the roster wholesale
	// before draining the retired entry (whose own replay also blocks).
	cur := p.cfg.Load()
	next := *cur
	next.AuthTokens = []string{"tok-replacement"}
	setCfgDone := make(chan struct{})
	go func() {
		defer close(setCfgDone)
		p.SetConfig(&next)
	}()
	// The retired entry's drain replay must also be in flight: the roster
	// swap precedes the drain, so the replay below runs against the old
	// account while the slot already resolves to the replacement.
	waitDeletes(3, "drain")
	releaseAll()

	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("RefreshTokenRefund: %v", out.err)
		}
		if !out.res.Dropped {
			t.Errorf("result = %+v, want Dropped (account switched mid-flight)", out.res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for RefreshTokenRefund after account switch")
	}
	select {
	case <-setCfgDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for SetConfig drain after account switch")
	}

	psnap := p.Snapshot()[0]
	if psnap.PendingRefund != "" {
		t.Errorf("replacement PendingRefund = %q, want empty (old receipt dropped)", psnap.PendingRefund)
	}
	if psnap.LastRefund != nil {
		t.Errorf("replacement LastRefund = %+v, want nil (old receipt dropped)", psnap.LastRefund)
	}
}

// TestRefreshTokenRefundSingleFlight pins the per-account cap: concurrent
// refreshes for one slot join the in-flight replay instead of re-DELETEing.
func TestRefreshTokenRefundSingleFlight(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	ctx := context.Background()

	var deletes atomic.Int64
	release := make(chan struct{})
	mock.SessionHandler = activeSessionHandler(func(w http.ResponseWriter, _ *http.Request) {
		n := deletes.Add(1)
		if n >= 2 {
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefundPending":true}`)
		} else {
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefund":1.5}`)
		}
	})
	if _, err := p.EnsureTokenSession(ctx, 0, modelB); err != nil {
		t.Fatalf("EnsureTokenSession: %v", err)
	}
	if err := p.DropTokenSession(ctx, 0); err != nil {
		t.Fatalf("DropTokenSession: %v", err)
	}

	const callers = 5
	type outcome struct {
		res RefundRefreshResult
		err error
	}
	results := make([]outcome, callers)
	var wg sync.WaitGroup
	started := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-started
			res, err := p.RefreshTokenRefund(ctx, 0)
			results[i] = outcome{res, err}
		}(i)
	}
	close(started)
	// Let every caller park on the flight, then release the single replay.
	deadline := time.Now().Add(5 * time.Second)
	for deletes.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	if got := deletes.Load(); got != 2 {
		t.Errorf("deletes = %d, want 2 (one shared replay for %d callers)", got, callers)
	}
	for i, out := range results {
		if out.err != nil {
			t.Errorf("caller %d: RefreshTokenRefund: %v", i, out.err)
		} else if !out.res.Settled || out.res.Amount != 1.5 {
			t.Errorf("caller %d: result = %+v, want Settled 1.5 (shared flight)", i, out.res)
		}
	}
}
