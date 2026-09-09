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
// fresh pool over the same store, and the snapshot (config, slot, warning
// counters) plus the streak cache survive.
func TestMaturityRestartRestoresState(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	mem := newMemMaturityStore()

	p1 := newMaturityPool(t, mock, true)
	p1.SetMaturityStore(mem)
	if err := p1.SetMaturity(0, true, 14, "", modelB); err != nil {
		t.Fatalf("SetMaturity: %v", err)
	}
	now := time.Now()
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
	// Warming accounts leave rotation even across the restart.
	if !p2.Snapshot()[0].Locked {
		t.Error("restored warming token is unlocked, want locked")
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

// A released token whose streak drops below its release target re-locks
// after 2 consecutive below-target days (counter survives restarts via the
// blob). One bad day alone never re-locks; recovery resets the counter.
func TestMaturityRelockAfterTwoBelowDays(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(7, false)
	p := newMaturityPool(t, mock, true)
	p.SetMaturityStore(newMemMaturityStore())
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatalf("SetMaturity: %v", err)
	}
	day1 := time.Now()
	p.maturityTickAt(context.Background(), day1)
	if snap := p.Snapshot()[0]; snap.Maturity == nil || snap.Maturity.Enabled || snap.Locked {
		t.Fatalf("day1 = %+v, want released (disabled+unlocked)", snap.Maturity)
		return
	}
	belowDays := func() int {
		toks := p.roster.Load()
		e := (*toks)[0]
		e.maturityMu.Lock()
		defer e.maturityMu.Unlock()
		return e.maturity.belowTargetDays
	}
	// Day 2: streak drops below target — counted, not re-locked.
	mock.StreakBody = streakBody(5, false)
	day2 := day1.Add(24 * time.Hour)
	p.maturityTickAt(context.Background(), day2)
	if got := belowDays(); got != 1 {
		t.Fatalf("below days after day2 = %d, want 1", got)
	}
	if snap := p.Snapshot()[0]; snap.Maturity.Enabled || snap.Locked {
		t.Fatalf("day2 locked/enabled, want still released (one bad day is noise)")
	}
	// Day 3: second consecutive below day — re-lock for warming.
	day3 := day1.Add(48 * time.Hour)
	p.maturityTickAt(context.Background(), day3)
	snap := p.Snapshot()[0]
	if snap.Maturity == nil || !snap.Maturity.Enabled || !snap.Locked {
		t.Fatalf("day3 = %+v, want re-enabled + locked", snap.Maturity)
		return
	}
}

func TestMaturityRelockRecoveryResetsCounter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(7, false)
	p := newMaturityPool(t, mock, true)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatalf("SetMaturity: %v", err)
	}
	day1 := time.Now()
	p.maturityTickAt(context.Background(), day1)
	mock.StreakBody = streakBody(5, false)
	p.maturityTickAt(context.Background(), day1.Add(24*time.Hour))
	// Recovery before the second bad day: counter resets, never re-locks.
	mock.StreakBody = streakBody(7, false)
	p.maturityTickAt(context.Background(), day1.Add(48*time.Hour))
	toks := p.roster.Load()
	e := (*toks)[0]
	e.maturityMu.Lock()
	below := e.maturity.belowTargetDays
	e.maturityMu.Unlock()
	if below != 0 {
		t.Errorf("below days after recovery = %d, want 0", below)
	}
	if snap := p.Snapshot()[0]; snap.Maturity.Enabled || snap.Locked {
		t.Errorf("recovered token locked/enabled, want still released")
	}
}

// Clearing a non-advance warning re-arms the loop: warn + counters drop,
// config (enabled/target/mode/touch model) is untouched, and the next due
// tick fires again instead of early-returning.
func TestClearMaturityWarnRearms(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, true)
	if err := p.SetMaturity(0, true, 7, "", modelB); err != nil {
		t.Fatalf("SetMaturity: %v", err)
	}
	toks := p.roster.Load()
	e := (*toks)[0]
	e.maturityMu.Lock()
	e.maturity.warn = true
	e.maturity.noAdvanceDays = 3
	e.maturity.lastNoAdvanceDay = "2026-09-06"
	e.maturityMu.Unlock()
	if err := p.ClearMaturityWarn(0); err != nil {
		t.Fatalf("ClearMaturityWarn: %v", err)
	}
	snap := p.Snapshot()[0].Maturity
	if snap == nil || snap.Warn || snap.NoAdvanceDays != 0 {
		t.Fatalf("after reset = %+v, want warn cleared", snap)
		return
	}
	if !snap.Enabled || snap.Target != 7 || snap.TouchModel != modelB {
		t.Errorf("after reset identity = %+v, want enabled/7/%s kept", snap, modelB)
	}
	// Out-of-range tokens reject.
	if err := p.ClearMaturityWarn(99); err == nil {
		t.Error("ClearMaturityWarn(99) = nil, want range error")
	}
	// The next due tick fires (probe) instead of stopping at the warning.
	now := time.Now()
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))
	p.maturityTickAt(context.Background(), now)
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes after reset = %d, want 1 (loop re-armed)", got)
	}
}
