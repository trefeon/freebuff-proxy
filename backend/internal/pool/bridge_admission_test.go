// bridge_reviewfix_test.go — regression tests for the 2026-08-31
// full-stack review fixes in the bridge path:
//
//   - P1-1: BridgeSnapshot must copy each entry's spend view under
//     bridgeMu — concurrent snapshot readers vs spend recorders must not
//     race the ledger window write.
//   - P2-2: a bridge entry evicted while requests are parked on the
//     admission single-flight must not hand out runs against the dead
//     session — the acquire aborts with a retryable error instead.
//   - P2-4: a follower that parked on a leader admitted for a different
//     model must re-admit for its own model instead of riding the
//     foreign session while reporting it.
//   - P3: BRIDGE_DAILY_LIMIT accounting survives eviction via the
//     bounded survivor list.
package pool

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// acquireResult carries one AcquireBridge outcome from a test goroutine
// back to the test body (a bare struct field would race the reader).
type acquireResult struct {
	lease *Lease
	err   error
}

// quietBridgePool is newBridgePool with the per-record debug logging
// silenced: the spend-race test records hundreds of spends and
// logSpendBuckets emits three Debug lines per record.
func quietBridgePool(t *testing.T, mock *testutil.MockUpstream) *Pool {
	t.Helper()
	p := newBridgePool(t, mock)
	p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return p
}

// TestBridgeSnapshotSpendConcurrent hammers BridgeSnapshot readers
// against bridgeRecordSpend writers on one entry (P1-1): the pre-fix
// code read the ledger outside bridgeMu — a guaranteed -race flag on CI
// and a torn-window hazard here. After the quiesce, the snapshot's
// copied spend view must match the live ledger exactly.
func TestBridgeSnapshotSpendConcurrent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := quietBridgePool(t, mock)

	const token = "spend-race-tok"
	entry, err := p.bridgeEntryFor(token)
	if err != nil {
		t.Fatalf("bridgeEntryFor: %v", err)
	}

	const (
		writers   = 4
		perWriter = 200
		amount    = int64(10)
	)
	want := int64(writers * perWriter * amount)

	var done atomic.Bool
	var bad atomic.Int32
	var readers sync.WaitGroup
	for range 2 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for !done.Load() {
				for _, s := range p.BridgeSnapshot() {
					if s.SpendDay < 0 {
						// A torn ledger read can surface as a negative
						// or garbage day total; the fixed snapshot
						// copies under bridgeMu and cannot produce one.
						bad.Add(1)
					}
				}
			}
		}()
	}

	var writersWG sync.WaitGroup
	for range writers {
		writersWG.Add(1)
		go func() {
			defer writersWG.Done()
			for range perWriter {
				p.bridgeRecordSpend(entry, amount)
			}
		}()
	}
	writersWG.Wait()
	done.Store(true)
	readers.Wait()

	if bad.Load() > 0 {
		t.Errorf("%d concurrent snapshots reported a negative SpendDay", bad.Load())
	}

	// Quiesced: a fresh snapshot must carry the exact live total.
	snaps := p.BridgeSnapshot()
	view := p.bridgeSpendSnapshot(entry)
	if view.Rolling24h != want {
		t.Errorf("ledger Rolling24h = %d, want %d (every record is in-window)", view.Rolling24h, want)
	}
	var row *BridgeTokenSnapshot
	for i := range snaps {
		if snaps[i].Key == tokenKey(token) {
			row = &snaps[i]
			break
		}
	}
	if row == nil {
		t.Fatal("BridgeSnapshot is missing the spend-race entry")
	}
	if row.SpendDay != float64(view.Day) {
		t.Errorf("snapshot SpendDay = %v, want %v (BridgeSnapshot must copy the spend view under bridgeMu)",
			row.SpendDay, float64(view.Day))
	}
}

// TestBridgeAcquireFollowerWrongModelReadmits pins P2-4: a follower that
// blocks on a leader admitted for a DIFFERENT model must reset the gate
// and re-admit for its own model, not ride the foreign session while its
// lease reports its own model. Both creates are parked so the
// interleaving is deterministic: the leader's re-check cannot observe the
// follower's (still parked) second admission, and the follower's re-check
// always observes the leader's committed model.
func TestBridgeAcquireFollowerWrongModelReadmits(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var (
		mu       sync.Mutex
		curInst  string
		curModel string
		release1 = make(chan struct{})
		release2 = make(chan struct{})
		creates  atomic.Int32
	)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		// Token-level probe (GET without instance id): zero-cost account
		// state, same shape as the mock's handleProbe.
		if r.Method == http.MethodGet && r.Header.Get("x-freebuff-instance-id") == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-probe","rateLimitsByModel":{}}`)
			return
		}
		if r.Method == http.MethodPost {
			n := creates.Add(1)
			switch n {
			case 1:
				<-release1 // park the leader mid-admission
			case 2:
				<-release2 // park the follower's re-admission
			}
			model := r.Header.Get("x-freebuff-model")
			inst := "inst-" + strconv.Itoa(int(n))
			mu.Lock()
			curInst, curModel = inst, model
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"`+inst+`","model":"`+model+`","expiresAt":"2030-01-01T00:00:00Z"}`)
			return
		}
		mu.Lock()
		inst, model := curInst, curModel
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"ended"}`)
			return
		}
		// Session poll: echo the currently admitted session.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"`+inst+`","model":"`+model+`","expiresAt":"2030-01-01T00:00:00Z"}`)
	}

	p := newBridgePool(t, mock)
	const clientToken = "follower-model-tok"

	leaderRes := make(chan acquireResult, 1)
	go func() {
		lease, err := p.AcquireBridge(context.Background(), clientToken, modelA)
		leaderRes <- acquireResult{lease: lease, err: err}
	}()

	// Wait until the leader's admission create is parked in the handler.
	eventually(t, "leader admission parked", func() bool { return creates.Load() == 1 })

	followerRes := make(chan acquireResult, 1)
	go func() {
		lease, err := p.AcquireBridge(context.Background(), clientToken, modelB)
		followerRes <- acquireResult{lease: lease, err: err}
	}()

	// The follower must block on the leader's single-flight: no second
	// create while the first admission is parked.
	time.Sleep(100 * time.Millisecond)
	if got := creates.Load(); got != 1 {
		t.Fatalf("session creates = %d while the leader admission is parked, want 1 (follower did not block on the single-flight)", got)
	}

	close(release1)

	lres := <-leaderRes
	if lres.err != nil {
		t.Fatalf("leader acquire failed: %v", lres.err)
	}
	defer p.LeaseRelease(lres.lease)
	if lres.lease.SessionInstanceID != "inst-1" {
		t.Errorf("leader instance = %q, want inst-1", lres.lease.SessionInstanceID)
	}

	close(release2)

	// P2-4: the follower must NOT ride the leader's modelA session — it
	// re-admits for modelB and leases THAT admission.
	fres := <-followerRes
	if fres.err != nil {
		t.Fatalf("follower acquire failed: %v (P2-4: follower must re-admit for its own model)", fres.err)
	}
	defer p.LeaseRelease(fres.lease)

	if got := creates.Load(); got != 2 {
		t.Errorf("session creates = %d, want 2 (follower re-admitted for %s)", got, modelB)
	}
	mu.Lock()
	model := curModel
	mu.Unlock()
	if model != modelB {
		t.Errorf("admitted session model = %q, want %q (the follower's model)", model, modelB)
	}
	if fres.lease.SessionInstanceID != "inst-2" {
		t.Errorf("follower instance = %q, want inst-2 (its own admission, not the leader's)", fres.lease.SessionInstanceID)
	}
	if ss := p.bridgeToken(clientToken).session.Snapshot(); ss.Model != modelB {
		t.Errorf("entry session model = %q, want %q", ss.Model, modelB)
	}
}

// TestBridgeAcquireEvictedMidAdmission pins P2-2: an eviction that lands
// while requests are parked on the admission single-flight (zero
// in-flight leases — the review's TOCTOU window) must abort the acquires
// with the retryable eviction error instead of handing out runs against
// the just-ended session.
func TestBridgeAcquireEvictedMidAdmission(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// The default handler counts the create before the delay, so polling
	// SessionCreatesSnapshot()==1 guarantees the admission is parked.
	mock.SessionCreateDelay = 500 * time.Millisecond

	p := newBridgePool(t, mock)
	const clientToken = "evict-race-tok"

	leaderRes := make(chan acquireResult, 1)
	go func() {
		lease, err := p.AcquireBridge(context.Background(), clientToken, modelA)
		leaderRes <- acquireResult{lease: lease, err: err}
	}()
	eventually(t, "leader session create parked", func() bool { return mock.SessionCreatesSnapshot() >= 1 })

	followerRes := make(chan acquireResult, 1)
	go func() {
		lease, err := p.AcquireBridge(context.Background(), clientToken, modelA)
		followerRes <- acquireResult{lease: lease, err: err}
	}()
	time.Sleep(100 * time.Millisecond)
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("session creates = %d while the leader admission is parked, want 1 (follower did not block on the single-flight)", got)
	}

	// Evict exactly while both requests are parked mid-admission.
	p.bridgeEvictToken(clientToken)

	for _, res := range []struct {
		name string
		ch   chan acquireResult
	}{{"leader", leaderRes}, {"follower", followerRes}} {
		r := <-res.ch
		if r.err == nil {
			t.Errorf("%s acquire succeeded after mid-admission eviction, want the eviction error (P2-2)", res.name)
			continue
		}
		if !strings.Contains(r.err.Error(), "evicted during admission") {
			t.Errorf("%s acquire err = %v, want the mid-admission eviction error", res.name, r.err)
		}
	}

	// No run may have started against the evicted entry: the pre-fix code
	// pre-created and acquired runs on the dead session here.
	if starts := mock.StartedRunsSnapshot(); len(starts) != 0 {
		t.Errorf("agent-runs STARTs = %d, want 0 (nothing may run against an evicted session)", len(starts))
	}
	if p.bridgeToken(clientToken) != nil {
		t.Error("entry still cached after eviction")
	}

	// A fresh acquire recreates the entry and succeeds — the documented
	// retry contract of the eviction error.
	lease, err := p.AcquireBridge(context.Background(), clientToken, modelA)
	if err != nil {
		t.Fatalf("re-acquire after eviction failed: %v", err)
	}
	p.LeaseRelease(lease)
}

// TestBridgeDailyUsageSurvivesEviction pins the P3 fix: evicting an entry
// must not reset its in-window contribution to the global
// BRIDGE_DAILY_LIMIT counter, and the survivor must expire one usage
// window after the eviction.
func TestBridgeDailyUsageSurvivesEviction(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := quietBridgePool(t, mock)

	const token = "usage-survivor-tok"
	entry, err := p.bridgeEntryFor(token)
	if err != nil {
		t.Fatalf("bridgeEntryFor: %v", err)
	}

	// Seed the entry's usage: 3 in-window chats, 1 already aged out.
	now := time.Now()
	p.bridgeMu.Lock()
	entry.ledger.usage = []time.Time{
		now.Add(-1 * time.Hour), now.Add(-2 * time.Hour), now.Add(-3 * time.Hour), now.Add(-30 * time.Hour),
	}
	p.bridgeMu.Unlock()

	p.bridgeEvictToken(token)
	p.bridgeMaintain(context.Background(), false)
	p.bridgeMu.Lock()
	got := p.bridgeDailyUsage
	p.bridgeMu.Unlock()
	if got != 3 {
		t.Fatalf("bridgeDailyUsage after eviction+recompute = %d, want 3 (evicted in-window usage must survive)", got)
	}

	// A re-created entry's new usage adds on top of the survivor.
	entry2, err := p.bridgeEntryFor(token)
	if err != nil {
		t.Fatalf("re-create bridgeEntryFor: %v", err)
	}
	p.bridgeRecordChat(entry2)
	p.bridgeMaintain(context.Background(), false)
	p.bridgeMu.Lock()
	got = p.bridgeDailyUsage
	p.bridgeMu.Unlock()
	if got != 4 {
		t.Fatalf("bridgeDailyUsage after re-create+chat = %d, want 4 (survivor 3 + live 1)", got)
	}

	// The survivor expires one usage window after its eviction.
	p.bridgeMu.Lock()
	if len(p.bridgeSurvivors) != 1 {
		t.Fatalf("bridgeSurvivors = %d, want 1", len(p.bridgeSurvivors))
	}
	p.bridgeSurvivors[0].evicted = time.Now().Add(-25 * time.Hour)
	p.bridgeMu.Unlock()
	p.bridgeMaintain(context.Background(), false)
	p.bridgeMu.Lock()
	got = p.bridgeDailyUsage
	p.bridgeMu.Unlock()
	if got != 1 {
		t.Fatalf("bridgeDailyUsage after survivor expiry = %d, want 1 (only the live entry)", got)
	}
}

// TestBridgeDailyUsageSurvivorCap pins the survivor list bound: captures
// beyond maxBridgeSurvivors drop the oldest survivors, and the recompute
// counts the retained ones.
func TestBridgeDailyUsageSurvivorCap(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := quietBridgePool(t, mock)

	entry, err := p.bridgeEntryFor("survivor-cap-tok")
	if err != nil {
		t.Fatalf("bridgeEntryFor: %v", err)
	}

	now := time.Now()
	p.bridgeMu.Lock()
	for range 300 {
		entry.ledger.usage = []time.Time{now}
		p.bridgeRecordSurvivorLocked(entry, now)
	}
	n := len(p.bridgeSurvivors)
	entry.ledger.usage = nil
	p.bridgeMu.Unlock()
	if n != maxBridgeSurvivors {
		t.Errorf("bridgeSurvivors = %d, want %d (cap enforced)", n, maxBridgeSurvivors)
	}

	p.bridgeMaintain(context.Background(), false)
	p.bridgeMu.Lock()
	got := p.bridgeDailyUsage
	p.bridgeMu.Unlock()
	if got != maxBridgeSurvivors {
		t.Errorf("bridgeDailyUsage = %d, want %d (survivors counted after recompute)", got, maxBridgeSurvivors)
	}
}
