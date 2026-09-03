package upstream

import (
	"context"
	"net/http"
	"testing"

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
		_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-fb","model":"openai/gpt-5.6-luna","expiresAt":"2030-01-01T00:00:00Z","freebucks":{"balance":17.5,"daily":{"limit":20,"spent":5,"remaining":15,"resetAt":"2026-09-01T07:00:00Z"},"wallet":{"balance":2.5,"monthlyBonus":10,"nextBonusAt":"2026-10-01T07:00:00Z"},"spend":{"limitUsd":15,"resetAt":"2026-09-01T07:00:00Z"},"planId":"starter","prices":{"openai/gpt-5.6-luna":2}}}`))
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
