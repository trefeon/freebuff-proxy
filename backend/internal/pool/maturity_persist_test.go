package pool

import (
	"context"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// memMaturityStore is a map-backed MaturityStore double: proves the pool
// persists through callbacks alone, never touching a real database.
type memMaturityStore struct {
	mu   sync.Mutex
	rows map[string]memMaturityRow
}

type memMaturityRow struct {
	state  string
	streak []byte
}

func newMemMaturityStore() *memMaturityStore {
	return &memMaturityStore{rows: map[string]memMaturityRow{}}
}

func (m *memMaturityStore) SaveMaturity(tokenHash string, stateJSON string, streakJSON []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[tokenHash] = memMaturityRow{state: stateJSON, streak: streakJSON}
	return nil
}

func (m *memMaturityStore) LoadMaturity(tokenHash string) (string, []byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[tokenHash]
	if !ok {
		return "", nil, false, nil
	}
	return r.state, r.streak, true, nil
}

// A restart restores automation state: enable + fire on one pool, rebuild a
// fresh pool over the same store, and the snapshot (config, slot, advance
// ledger) plus the streak cache survive.
func TestMaturityRestartRestoresState(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemMaturityStore()

	p1 := newMaturityPool(t, mock, true)
	p1.SetMaturityStore(mem)
	if err := p1.SetMaturity(0, true, 14, "", modelB); err != nil {
		t.Fatalf("SetMaturity: %v", err)
	}
	now := windowNow()
	seedStreak(p1, 0, 2, false, now)
	setMaturitySlot(p1, 0, now.Add(-time.Hour), laDay(now))
	p1.maturityTickAt(context.Background(), now)
	if action, result := maturityResult(p1, 0); action != "probe" || result != "ok" {
		t.Fatalf("p1 touch = %q/%q, want probe/ok", action, result)
	}

	p2 := newMaturityPool(t, mock, true)
	p2.SetMaturityStore(mem)
	if err := p2.RestoreMaturity(); err != nil {
		t.Fatalf("RestoreMaturity: %v", err)
	}
	snap := p2.Snapshot()[0].Maturity
	if snap == nil {
		t.Fatal("restored snapshot is nil, want enabled automation")
		return
	}
	if !snap.Enabled || snap.Target != 14 || snap.Mode != MaturityModeUnmetered || snap.TouchModel != modelB {
		t.Errorf("restored identity = %+v, want enabled/14/unmetered/%s", snap, modelB)
	}
	if snap.LastAction != "probe" || snap.LastResult != "ok" {
		t.Errorf("restored touch = %q/%q, want probe/ok", snap.LastAction, snap.LastResult)
	}
	if snap.Slot.IsZero() {
		t.Error("restored slot is zero, want the pre-restart slot")
	}
	// Restored warming accounts stay leasable: enrollment never locks,
	// not even across a restart.
	if p2.Snapshot()[0].Locked {
		t.Error("restored warming token is locked, want leasable")
	}
	// A pool with no store row restores to never-enrolled (nil snapshot).
	p3 := newMaturityPool(t, mock, true)
	p3.SetMaturityStore(newMemMaturityStore())
	if err := p3.RestoreMaturity(); err != nil {
		t.Fatalf("RestoreMaturity empty: %v", err)
	}
	if got := p3.Snapshot()[0].Maturity; got != nil {
		t.Errorf("empty restore snapshot = %+v, want nil", got)
	}
}

// A touch-model draft on a disabled, never-enrolled token is operator state:
// the snapshot must echo it (so the card survives refresh) and the store
// must persist it (so it survives restart). A disabled token with no draft
// and no history still snapshots nil.
func TestMaturityDisabledTouchDraftSurvives(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	mem := newMemMaturityStore()

	p1 := newMaturityPool(t, mock, true)
	p1.SetMaturityStore(mem)
	const draft = "z-ai/glm-5.3-flash"
	if err := p1.SetMaturity(0, false, 7, "", draft); err != nil {
		t.Fatalf("SetMaturity disabled: %v", err)
	}
	snap := p1.Snapshot()[0].Maturity
	if snap == nil {
		t.Fatal("disabled draft snapshot is nil, want the drafted touch model")
		return
	}
	if snap.TouchModel != draft {
		t.Errorf("snapshot touch = %q, want %q", snap.TouchModel, draft)
	}
	if snap.Enabled {
		t.Error("snapshot enabled, want disabled")
	}

	p2 := newMaturityPool(t, mock, true)
	p2.SetMaturityStore(mem)
	if err := p2.RestoreMaturity(); err != nil {
		t.Fatalf("RestoreMaturity: %v", err)
	}
	got := p2.Snapshot()[0].Maturity
	if got == nil || got.TouchModel != draft {
		t.Fatalf("restored snapshot = %+v, want touch %q", got, draft)
		return
	}

	// No draft, no history, disabled: still nil (never-enrolled).
	p3 := newMaturityPool(t, mock, true)
	p3.SetMaturityStore(newMemMaturityStore())
	if err := p3.SetMaturity(0, false, 7, "", ""); err != nil {
		t.Fatalf("SetMaturity empty: %v", err)
	}
	if got := p3.Snapshot()[0].Maturity; got != nil {
		t.Errorf("empty disabled snapshot = %+v, want nil", got)
	}
}
