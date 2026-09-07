package store

import (
	"testing"
)

// TestLatestQuotaSnapshotsPerGroup pins the boot-seed read (ADR-0024):
// exactly the latest row per (token_idx, model), newest-first.
func TestLatestQuotaSnapshotsPerGroup(t *testing.T) {
	s := openTest(t)
	rows := []QuotaSnapshot{
		{TS: 100, TokenIdx: 0, Model: "mA", Limit: 5, Recent: 1},
		{TS: 200, TokenIdx: 0, Model: "mA", Limit: 5, Recent: 2},
		{TS: 300, TokenIdx: 0, Model: "mA", Limit: 5, Recent: 3, ResetAt: 9000, Entitlements: `{"base":5}`},
		{TS: 150, TokenIdx: 0, Model: "mB", Limit: 7, Recent: 4},
		{TS: 120, TokenIdx: 1, Model: "mA", Limit: 5, Recent: 6},
	}
	for _, r := range rows {
		if err := s.RecordQuota(r); err != nil {
			t.Fatalf("RecordQuota: %v", err)
		}
	}
	got, err := s.LatestQuotaSnapshots(0)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshots: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3 (one per group): %+v", len(got), got)
	}
	// Newest-first: ts 300, 150, 120.
	if got[0].TS != 300 || got[1].TS != 150 || got[2].TS != 120 {
		t.Errorf("order not newest-first: %d,%d,%d", got[0].TS, got[1].TS, got[2].TS)
	}
	head := got[0]
	if head.TokenIdx != 0 || head.Model != "mA" || head.Recent != 3 || head.ResetAt != 9000 || head.Entitlements != `{"base":5}` {
		t.Errorf("head row not the latest sample: %+v", head)
	}
}

func TestLatestQuotaSnapshotsBoundAndEmpty(t *testing.T) {
	s := openTest(t)
	if got, err := s.LatestQuotaSnapshots(0); err != nil || len(got) != 0 {
		t.Fatalf("empty store = %d rows, err=%v, want 0,nil", len(got), err)
	}
	for i, ts := range []int64{10, 20, 30} {
		if err := s.RecordQuota(QuotaSnapshot{TS: ts, TokenIdx: 0, Model: "m", Limit: 5, Recent: float64(i)}); err != nil {
			t.Fatalf("RecordQuota: %v", err)
		}
		if err := s.RecordQuota(QuotaSnapshot{TS: ts, TokenIdx: 1, Model: "m", Limit: 5, Recent: float64(i)}); err != nil {
			t.Fatalf("RecordQuota: %v", err)
		}
	}
	got, err := s.LatestQuotaSnapshots(1)
	if err != nil {
		t.Fatalf("LatestQuotaSnapshots: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d with limit 1, want 1", len(got))
	}
	if got[0].TS != 30 || got[0].Recent != 2 {
		t.Errorf("capped row = %+v, want the newest sample", got[0])
	}
}
