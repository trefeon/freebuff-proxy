package upstream

import (
	"context"
	"net/http"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// TestParseFreebucks pins issue #232: the session response's "freebucks"
// block is parsed into SessionState.Freebucks (balance, daily/weekly/monthly
// windows with resetAt, bindingWindow, planDaily, prices); absent block stays
// nil.
func TestParseFreebucks(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-fb","model":"openai/gpt-5.6-luna","expiresAt":"2030-01-01T00:00:00Z","freebucks":{"balance":12.5,"daily":{"limit":20,"spent":5,"remaining":15,"resetAt":"2026-09-01T07:00:00Z"},"weekly":{"limit":80,"spent":5,"remaining":75,"resetAt":"2026-09-07T07:00:00Z"},"monthly":{"limit":300,"spent":5,"remaining":295,"resetAt":"2026-10-01T07:00:00Z"},"bindingWindow":"daily","planDaily":10,"prices":{"openai/gpt-5.6-luna":2}}}`))
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
	if fb.Balance != 12.5 {
		t.Errorf("Balance = %v, want 12.5", fb.Balance)
	}
	if fb.BindingWindow != "daily" {
		t.Errorf("BindingWindow = %q, want daily", fb.BindingWindow)
	}
	if fb.Daily.Limit != 20 || fb.Daily.Remaining != 15 {
		t.Errorf("Daily = %+v, want limit 20 remaining 15", fb.Daily)
	}
	if fb.Daily.ResetAt.IsZero() {
		t.Error("Daily.ResetAt zero, want parsed time")
	}
	if fb.PlanDaily == nil || *fb.PlanDaily != 10 {
		t.Errorf("PlanDaily = %v, want 10", fb.PlanDaily)
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
