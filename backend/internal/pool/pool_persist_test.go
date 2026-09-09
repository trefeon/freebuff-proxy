package pool

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// memPoolPersist is a map-backed PoolPersist double: proves the pool
// persists through the interface alone, never touching a real database.
type memPoolPersist struct {
	mu       sync.Mutex
	rows     map[string][]byte
	saves    int
	failSave error
}

func newMemPoolPersist() *memPoolPersist {
	return &memPoolPersist{rows: make(map[string][]byte)}
}

func (m *memPoolPersist) SavePoolState(key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	if m.failSave != nil {
		return m.failSave
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	m.rows[key] = cp
	return nil
}

func (m *memPoolPersist) LoadPoolState(key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.rows[key]
	if !ok {
		return nil, false, nil
	}
	cp := make([]byte, len(v))
	copy(cp, v)
	return cp, true, nil
}

func (m *memPoolPersist) DeletePoolState(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, key)
	return nil
}

func (m *memPoolPersist) ListPoolState(prefix string) (map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string][]byte)
	for k, v := range m.rows {
		if strings.HasPrefix(k, prefix) {
			cp := make([]byte, len(v))
			copy(cp, v)
			out[k] = cp
		}
	}
	return out, nil
}

func (m *memPoolPersist) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.rows {
		out = append(out, k)
	}
	return out
}

// TestPoolPersistRestartRestoresLedgerAndBurst records ledger + spend +
// burst + admissions + bridge state on one pool, flushes, rebuilds a fresh
// pool over the same store, and proves the counters survive the restart.
func TestPoolPersistRestartRestoresLedgerAndBurst(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()

	p1 := newTestPoolCfg(t, func(cfg *config.Config) { cfg.BurstBalanceEnabled = true }, mock)
	p1.SetPoolPersist(mem)
	toks := p1.roster.Load()
	entry := (*toks)[0]
	p1.recordChatEntry(entry)
	p1.recordSpendEntry(entry, 100)
	p1.recordSpendLimited(0)
	now := time.Now()
	for range 3 {
		p1.burstRecordAt(modelB, 0, now)
	}
	p1.admissionsMu.Lock()
	if p1.admissions == nil {
		p1.admissions = make(map[string]int)
	}
	p1.admissions[modelA] = 0
	p1.admissionsMu.Unlock()
	p1.markPersistDirty()
	p1.bridgeMu.Lock()
	p1.bridgeDailyUsage = 7
	p1.bridgeSurvivors = append(p1.bridgeSurvivors, bridgeSurvivor{count: 2, evicted: now})
	p1.bridgeMu.Unlock()
	p1.markPersistDirty()

	if err := p1.FlushPoolPersist(); err != nil {
		t.Fatalf("FlushPoolPersist: %v", err)
	}
	if len(mem.keys()) == 0 {
		t.Fatal("flush wrote no rows")
	}
	// Raw tokens never cross the boundary: no key or value may contain one.
	for _, k := range mem.keys() {
		raw, _, _ := mem.LoadPoolState(k)
		if strings.Contains(k, "tok-0") || strings.Contains(string(raw), "tok-0") {
			t.Fatalf("raw token leaked into pool_state row %q", k)
		}
	}

	p2 := newTestPoolCfg(t, func(cfg *config.Config) { cfg.BurstBalanceEnabled = true }, mock)
	p2.SetPoolPersist(mem)
	p2.RestorePoolPersist()

	if got := p2.usageCount(0); got != 1 {
		t.Fatalf("restored usageCount = %d, want 1", got)
	}
	if got := p2.spendSnapshot(0).Day; got != 100 {
		t.Fatalf("restored spend day = %d, want 100", got)
	}
	if got := p2.spendSnapshot(0).SpendLimited; got != 1 {
		t.Fatalf("restored spendLimited = %d, want 1", got)
	}
	p2.burstMu.Lock()
	n := len(p2.burstHits[modelB])
	p2.burstMu.Unlock()
	if n != 3 {
		t.Fatalf("restored burst hits = %d, want 3", n)
	}
	p2.admissionsMu.Lock()
	adm := p2.admissions[modelA]
	p2.admissionsMu.Unlock()
	if adm != 0 {
		t.Fatalf("restored admissions[%q] = %d, want 0", modelA, adm)
	}
	p2.bridgeMu.Lock()
	usage, surv := p2.bridgeDailyUsage, len(p2.bridgeSurvivors)
	p2.bridgeMu.Unlock()
	if usage != 7 || surv != 1 {
		t.Fatalf("restored bridge = usage %d survivors %d, want 7/1", usage, surv)
	}
}

// TestPoolPersistExpiredWindowsIgnored proves TTL/expiry is enforced on
// restore: out-of-window usage, spend, burst and survivor timestamps are
// dropped instead of resurrected.
func TestPoolPersistExpiredWindowsIgnored(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()

	p1 := newTestPoolCfg(t, func(cfg *config.Config) { cfg.BurstBalanceEnabled = true }, mock)
	p1.SetPoolPersist(mem)
	toks := p1.roster.Load()
	entry := (*toks)[0]
	p1.recordChatEntry(entry)
	p1.recordSpendEntry(entry, 100)
	p1.burstRecordAt(modelB, 0, time.Now())
	p1.markPersistDirty()
	if err := p1.FlushPoolPersist(); err != nil {
		t.Fatalf("FlushPoolPersist: %v", err)
	}

	// Age every persisted timestamp 25h into the past (past the 24h usage
	// window, the 60s rpm window, the 1m burst window and the survivor
	// window) and push the spend day bucket 3 days back so it rolls.
	age := func(ms int64) int64 { return ms - int64(25*time.Hour/time.Millisecond) }
	mem.mu.Lock()
	for k, raw := range mem.rows {
		switch {
		case len(k) > len(poolLedgerPrefix) && k[:len(poolLedgerPrefix)] == poolLedgerPrefix:
			var blob poolLedgerBlob
			if err := json.Unmarshal(raw, &blob); err != nil {
				t.Fatalf("unmarshal ledger: %v", err)
			}
			for i := range blob.Usage {
				blob.Usage[i] = age(blob.Usage[i])
			}
			for i := range blob.Requests {
				blob.Requests[i] = age(blob.Requests[i])
			}
			for i := range blob.Spend.Rolling {
				blob.Spend.Rolling[i].At = age(blob.Spend.Rolling[i].At)
			}
			blob.Spend.DayStart = bucketStart(time.Now().Add(-72*time.Hour), "day")
			blob.Spend.DayUsed = 100
			mem.rows[k] = mustMarshalPool(blob)
		case k == poolStateBurst:
			var stored map[string][]poolBurstHit
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatalf("unmarshal burst: %v", err)
			}
			for m, hits := range stored {
				for i := range hits {
					hits[i].At = age(hits[i].At)
				}
				stored[m] = hits
			}
			mem.rows[k] = mustMarshalPool(stored)
		}
	}
	// Age the bridge usage row away entirely: drop it so usage restores 0,
	// and store one expired survivor.
	delete(mem.rows, poolStateBridgeUsage)
	mem.rows[poolStateBridgeSurvivors] = mustMarshalPool([]poolSurvivorBlob{
		{Count: 5, Evicted: age(time.Now().UnixMilli())},
	})
	mem.mu.Unlock()

	p2 := newTestPoolCfg(t, func(cfg *config.Config) { cfg.BurstBalanceEnabled = true }, mock)
	p2.SetPoolPersist(mem)
	p2.RestorePoolPersist()

	if got := p2.usageCount(0); got != 0 {
		t.Fatalf("expired usage restored = %d, want 0", got)
	}
	if got := p2.spendSnapshot(0).Day; got != 0 {
		t.Fatalf("stale spend day restored = %d, want 0 (rolled)", got)
	}
	p2.burstMu.Lock()
	n := len(p2.burstHits[modelB])
	p2.burstMu.Unlock()
	if n != 0 {
		t.Fatalf("expired burst hits restored = %d, want 0", n)
	}
	p2.bridgeMu.Lock()
	usage, surv := p2.bridgeDailyUsage, len(p2.bridgeSurvivors)
	p2.bridgeMu.Unlock()
	if usage != 0 || surv != 0 {
		t.Fatalf("expired bridge restored = usage %d survivors %d, want 0/0", usage, surv)
	}
}

// TestPoolPersistUnfitExpiryIgnored proves an expired unfit mark reads as
// servable (the unfit registry itself is intentionally NOT persisted —
// narrowed scope — so this pins the in-memory TTL-on-read behavior the
// restore path relies on).
func TestPoolPersistUnfitExpiryIgnored(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	old := modelUnfitTTL
	modelUnfitTTL = -time.Second
	defer func() { modelUnfitTTL = old }()
	p.MarkModelUnfit(modelB, nil)
	if until, _ := p.ModelUnfit(modelB); !until.IsZero() {
		t.Fatalf("expired unfit mark still reported until %v", until)
	}
}

// TestPoolPersistDegradesLiveOnly proves a DB failure never blocks the hot
// path: records still land in memory and the flush re-arms for retry.
func TestPoolPersistDegradesLiveOnly(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()
	mem.failSave = errors.New("disk on fire")

	p := newTestPool(t, mock)
	p.SetPoolPersist(mem)
	toks := p.roster.Load()
	p.recordChatEntry((*toks)[0]) // hot path: must not fail, block, or panic
	if got := p.usageCount(0); got != 1 {
		t.Fatalf("usageCount with failing store = %d, want 1 (live-only)", got)
	}
	if err := p.FlushPoolPersist(); err == nil {
		t.Fatal("FlushPoolPersist with failing store: want error")
		return
	}
	if !p.persistDirty.Load() {
		t.Fatal("failed flush did not re-arm the dirty flag")
	}
	if len(mem.keys()) != 0 {
		t.Fatalf("failing store wrote %d rows", len(mem.keys()))
	}
}

// TestPoolPersistDisabledIsNoOp proves nil-store pools behave exactly as
// before (in-memory only, pre-persist behavior).
func TestPoolPersistDisabledIsNoOp(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	toks := p.roster.Load()
	p.recordChatEntry((*toks)[0])
	if err := p.FlushPoolPersist(); err != nil {
		t.Fatalf("FlushPoolPersist nil store: %v", err)
	}
	p.RestorePoolPersist() // must not panic
	if got := p.usageCount(0); got != 1 {
		t.Fatalf("usageCount nil store = %d, want 1", got)
	}
}
