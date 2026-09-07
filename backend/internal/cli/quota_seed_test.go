package cli

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/pool"
	history "freebuff-proxy/backend/internal/store"
)

// fakeSeedStore is the store half of the boot-seed seam.
type fakeSeedStore struct {
	rows []history.QuotaSnapshot
	err  error
}

func (f *fakeSeedStore) LatestQuotaSnapshots(limit int) ([]history.QuotaSnapshot, error) {
	return f.rows, f.err
}

// fakeSeedPool captures pushed seed rows.
type fakeSeedPool struct {
	got []pool.QuotaSeedRow
}

func (f *fakeSeedPool) SeedQuotaSnapshot(rows []pool.QuotaSeedRow) {
	f.got = append(f.got, rows...)
}

func seedTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSeedQuotaFromStoreMapsAndFilters(t *testing.T) {
	reset := time.Date(2026, 9, 8, 7, 0, 0, 0, time.UTC)
	probed := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	st := &fakeSeedStore{rows: []history.QuotaSnapshot{
		{TS: probed.UnixMilli(), TokenIdx: 0, Model: "m/a", Limit: 5, Recent: 2, ResetAt: reset.UnixMilli(), Entitlements: `{"base":5}`},
		{TS: probed.UnixMilli(), TokenIdx: 1, Model: "m/b", Limit: 7, Recent: 1, Entitlements: "not-json"},
		{TS: probed.UnixMilli(), TokenIdx: 7, Model: "m/c", Limit: 5}, // stale token idx
		{TS: probed.UnixMilli(), TokenIdx: 0, Model: ""},              // empty model
	}}
	p := &fakeSeedPool{}

	seedQuotaFromStore(seedTestLogger(), st, p, 2)

	if len(p.got) != 2 {
		t.Fatalf("pushed rows = %d, want 2 (stale idx + empty model dropped)", len(p.got))
	}
	first := p.got[0]
	if first.Token != 0 || first.Model != "m/a" || first.Limit != 5 || first.Recent != 2 {
		t.Errorf("first row = %+v, want token 0 m/a 5/2", first)
	}
	if !first.ResetAt.Equal(reset) || !first.ProbedAt.Equal(probed) {
		t.Errorf("first row times = %v/%v, want %v/%v", first.ResetAt, first.ProbedAt, reset, probed)
	}
	if first.Entitlement["base"] != 5 {
		t.Errorf("first row entitlement = %v, want base=5", first.Entitlement)
	}
	// Corrupt entitlements degrade to nil; the row still seeds.
	if p.got[1].Model != "m/b" || p.got[1].Entitlement != nil {
		t.Errorf("second row = %+v, want m/b with nil entitlement", p.got[1])
	}
}

func TestSeedQuotaFromStoreWarnOnly(t *testing.T) {
	p := &fakeSeedPool{}
	// Store failure keeps the boot green: no panic, no pool push.
	seedQuotaFromStore(seedTestLogger(), &fakeSeedStore{err: errors.New("db locked")}, p, 2)
	if len(p.got) != 0 {
		t.Errorf("pushed rows on store error = %d, want 0", len(p.got))
	}
	// Nil store, nil pool, and zero tokens are all no-ops.
	seedQuotaFromStore(seedTestLogger(), nil, p, 2)
	seedQuotaFromStore(seedTestLogger(), &fakeSeedStore{}, nil, 2)
	seedQuotaFromStore(seedTestLogger(), &fakeSeedStore{rows: []history.QuotaSnapshot{{TokenIdx: 0, Model: "m"}}}, p, 0)
	if len(p.got) != 0 {
		t.Errorf("pushed rows on nil/empty inputs = %d, want 0", len(p.got))
	}
}

func TestQuotaSeedRowZeroTimes(t *testing.T) {
	s, ok := quotaSeedRow(history.QuotaSnapshot{TokenIdx: 0, Model: "m"}, 1)
	if !ok {
		t.Fatal("valid row rejected")
	}
	if !s.ResetAt.IsZero() || !s.ProbedAt.IsZero() {
		t.Errorf("zero millis must map to zero times: %+v", s)
	}
}
