package upstream

import (
	"context"
	"net/http"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// TestParseFreebucks pins issue #232 with the issue #321 wire shape: the
// session response's "freebucks" block is parsed into SessionState.Freebucks
// (balance, daily window with resetAt, wallet, spend ceiling, planId,
// prices); absent block stays nil.
func TestParseFreebucks(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-fb","model":"openai/gpt-5.6-luna","expiresAt":"2030-01-01T00:00:00Z","freebucks":{"balance":17.5,"daily":{"limit":20,"spent":5,"remaining":15,"resetAt":"2026-09-01T07:00:00Z"},"wallet":{"balance":2.5,"monthlyBonus":10,"nextBonusAt":"2026-10-01T07:00:00Z"},"spend":{"limitUsd":15,"resetAt":"2026-09-01T07:00:00Z"},"monthly":{"limitUsd":50,"spentUsd":10,"remainingUsd":40,"resetAt":"2026-10-01T07:00:00Z"},"planId":"starter","prices":{"openai/gpt-5.6-luna":2}}}`))
	}

	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Freebucks == nil {
		t.Fatal("Freebucks = nil, want parsed block")
	}
	fb := st.Freebucks
	if fb.Balance != 17.5 {
		t.Errorf("Balance = %v, want 17.5", fb.Balance)
	}
	if fb.Daily.Limit != 20 || fb.Daily.Remaining != 15 {
		t.Errorf("Daily = %+v, want limit 20 remaining 15", fb.Daily)
	}
	if fb.Daily.ResetAt.IsZero() {
		t.Error("Daily.ResetAt zero, want parsed time")
	}
	if fb.Wallet.Balance != 2.5 {
		t.Errorf("Wallet.Balance = %v, want 2.5", fb.Wallet.Balance)
	}
	if fb.Wallet.MonthlyBonus != 10 {
		t.Errorf("Wallet.MonthlyBonus = %v, want 10", fb.Wallet.MonthlyBonus)
	}
	if fb.Wallet.NextBonusAt.IsZero() {
		t.Error("Wallet.NextBonusAt zero, want parsed time")
	}
	if fb.Spend.LimitUsd != 15 {
		t.Errorf("Spend.LimitUsd = %v, want 15", fb.Spend.LimitUsd)
	}
	if fb.Spend.ResetAt.IsZero() {
		t.Error("Spend.ResetAt zero, want parsed time")
	}
	if fb.PlanID != "starter" {
		t.Errorf("PlanID = %q, want starter", fb.PlanID)
	}
	if fb.Prices["openai/gpt-5.6-luna"] != 2 {
		t.Errorf("Prices = %v, want luna 2", fb.Prices)
	}
	if fb.Monthly == nil {
		t.Fatal("Monthly = nil, want parsed allowance")
	}
	if fb.Monthly.LimitUsd != 50 || fb.Monthly.SpentUsd != 10 || fb.Monthly.RemainingUsd != 40 {
		t.Errorf("Monthly = %+v, want limit 50 spent 10 remaining 40", fb.Monthly)
	}
	if fb.Monthly.ResetAt.IsZero() {
		t.Error("Monthly.ResetAt zero, want parsed time")
	}
}

// TestParseFreebucksAbsent: no freebucks block -> nil, other fields intact.
func TestParseFreebucksAbsent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-plain","model":"mimo/mimo-v2.5","expiresAt":"2030-01-01T00:00:00Z"}`))
	}
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Freebucks != nil {
		t.Errorf("Freebucks = %+v, want nil", st.Freebucks)
	}
	if st.InstanceID != "inst-plain" {
		t.Errorf("InstanceID = %q, want inst-plain", st.InstanceID)
	}
}

// TestParseFreebucksPriceDrift pins issue #350: quotaExempt, priceNotices
// and priceChanges parse; due changes apply at parse time (reprice +
// notice refresh + consume), future ones are kept, and models off the
// meter are never repriced.
func TestParseFreebucksPriceDrift(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-fb350","model":"openai/gpt-5.6-luna","expiresAt":"2030-01-01T00:00:00Z","freebucks":{"balance":0,"daily":{"limit":20,"spent":20,"remaining":0,"resetAt":"2026-09-01T07:00:00Z"},"quotaExempt":true,"prices":{"openai/gpt-5.6-luna":2},"priceNotices":{"openai/gpt-5.6-luna":"old copy"},"priceChanges":[{"at":"2020-01-02T00:00:00Z","modelId":"openai/gpt-5.6-luna","price":5,"tagline":"new copy"},{"at":"2020-01-01T00:00:00Z","modelId":"openai/gpt-5.6-luna","price":3,"tagline":"mid copy"},{"at":"2999-01-01T00:00:00Z","modelId":"openai/gpt-5.6-luna","price":9,"tagline":"future copy"},{"at":"2020-01-03T00:00:00Z","modelId":"off/meter","price":1,"tagline":"unpriced"}]}}`))
	}
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fb := st.Freebucks
	if fb == nil {
		t.Fatal("Freebucks = nil, want parsed block")
	}
	if !fb.QuotaExempt {
		t.Error("QuotaExempt = false, want true")
	}
	// Due changes apply oldest-first: 2 -> 3 -> 5.
	if fb.Prices["openai/gpt-5.6-luna"] != 5 {
		t.Errorf("Prices[luna] = %v, want 5 (last due change wins)", fb.Prices["openai/gpt-5.6-luna"])
	}
	if fb.PriceNotices["openai/gpt-5.6-luna"] != "new copy" {
		t.Errorf("PriceNotices[luna] = %q, want new copy", fb.PriceNotices["openai/gpt-5.6-luna"])
	}
	if _, ok := fb.Prices["off/meter"]; ok {
		t.Error("off-meter model repriced, want untouched")
	}
	if len(fb.PriceChanges) != 1 || fb.PriceChanges[0].Tagline != "future copy" {
		t.Errorf("PriceChanges = %+v, want only the future change", fb.PriceChanges)
	}
}

// TestApplyFreebucksPriceChangesUnit: bad timestamps are kept pending (never
// applied), nil prices map is allocated, empty schedule is a no-op.
func TestApplyFreebucksPriceChangesUnit(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	fb := &FreebucksInfo{
		Prices: map[string]float64{"m": 2},
		PriceChanges: []FreebucksPriceChange{
			{At: "not-a-time", ModelID: "m", Price: 7, Tagline: "bad"},
			{At: "2020-05-05T00:00:00Z", ModelID: "m", Price: 4, Tagline: "due"},
		},
	}
	ApplyFreebucksPriceChanges(fb, now)
	if fb.Prices["m"] != 4 {
		t.Errorf("Prices[m] = %v, want 4", fb.Prices["m"])
	}
	if len(fb.PriceChanges) != 1 || fb.PriceChanges[0].Tagline != "bad" {
		t.Errorf("PriceChanges = %+v, want only the unparsable change kept", fb.PriceChanges)
	}
	var nilFB *FreebucksInfo
	ApplyFreebucksPriceChanges(nilFB, now) // must not panic
	empty := &FreebucksInfo{}
	ApplyFreebucksPriceChanges(empty, now) // must not panic
}
