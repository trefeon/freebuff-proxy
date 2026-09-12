package session

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// TestStatusErrorTerminalRefusals pins the vendor af898dc taxonomy port:
// consent_required and the Desktop purchase-flow trio are terminal
// admission refusals (upstream cli/src/hooks/use-freebuff-session.ts
// nextDelayMs returns null = stop polling). They surface as plain
// *upstream.UpstreamError — no cooldown type, no retry — so the pool's
// classifyAndCooldown leaves the token alone and fails over.
func TestStatusErrorTerminalRefusals(t *testing.T) {
	t.Run("consent_required carries 409 plus spend", func(t *testing.T) {
		st := &upstream.SessionState{
			Status:        "consent_required",
			HTTPStatus:    http.StatusConflict,
			Message:       "",
			WalletConsent: &upstream.WalletConsent{Price: 5, WalletSpend: 2},
		}
		err := statusError("consent_required", st)
		if err == nil {
			t.Fatal("statusError = nil, want terminal refusal")
		}
		ue, ok := err.(*upstream.UpstreamError)
		if !ok {
			t.Fatalf("err type = %T, want *upstream.UpstreamError (no cooldown)", err)
		}
		if ue.Status != http.StatusConflict {
			t.Errorf("status = %d, want 409", ue.Status)
		}
		if !strings.Contains(ue.Body, "2") || !strings.Contains(ue.Body, "consent_required") {
			t.Errorf("body = %q, want spend plus code", ue.Body)
		}
		if ue.Retryable {
			t.Error("Retryable = true, want terminal (CLI retry:null)")
		}
	})

	for _, status := range []string{"purchase_claim_released", "purchase_in_use", "purchase_capacity"} {
		t.Run(status+" surfaces honest status", func(t *testing.T) {
			st := &upstream.SessionState{Status: status, HTTPStatus: http.StatusConflict, Message: "slot busy"}
			err := statusError(status, st)
			ue, ok := err.(*upstream.UpstreamError)
			if !ok {
				t.Fatalf("err type = %T, want *upstream.UpstreamError", err)
			}
			if ue.Status != http.StatusConflict || ue.Body != "slot busy" {
				t.Errorf("got %d %q, want 409 with upstream message", ue.Status, ue.Body)
			}
		})
	}

	t.Run("purchase without message gets default copy", func(t *testing.T) {
		err := statusError("purchase_capacity", &upstream.SessionState{Status: "purchase_capacity"})
		ue, ok := err.(*upstream.UpstreamError)
		if !ok {
			t.Fatalf("err type = %T, want *upstream.UpstreamError", err)
		}
		if ue.Status != http.StatusConflict {
			t.Errorf("status = %d, want 409 fallback", ue.Status)
		}
		if !strings.Contains(ue.Body, "purchase_capacity") {
			t.Errorf("body = %q, want status code name", ue.Body)
		}
	})
}

// TestRefreshTerminalRefusals drives the new statuses end to end through
// the admission loop: the manager returns the terminal error instead of
// looping, caching, or reporting an unknown status.
func TestRefreshTerminalRefusals(t *testing.T) {
	for _, tc := range []struct {
		status string
		body   string
		want   string
	}{
		{"consent_required", `{"status":"consent_required","walletConsent":{"price":5,"walletSpend":2},"freebucks":null}`, "consent_required"},
		{"purchase_in_use", `{"status":"purchase_in_use","message":"hour in use elsewhere"}`, "hour in use"},
		{"purchase_capacity", `{"status":"purchase_capacity"}`, "purchase_capacity"},
		{"purchase_claim_released", `{"status":"purchase_claim_released","message":"claim released"}`, "claim released"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mgr := newTestManager(t, mock)
			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, tc.body)
					return
				}
				http.NotFound(w, r)
			}
			_, err := mgr.EnsureSession(context.Background())
			if err == nil {
				t.Fatalf("%s admitted, want terminal refusal", tc.status)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if _, ok := err.(*upstream.UpstreamError); !ok {
				t.Errorf("err type = %T, want *upstream.UpstreamError (no cooldown)", err)
			}
		})
	}
}

// TestEndSessionRefundSettled pins the receipt handler path: a DELETE
// returning freebucksRefund records lastRefund with no pending entry.
func TestEndSessionRefundSettled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefund":1.5}`)
			return
		}
		http.NotFound(w, r)
	}
	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := mgr.Snapshot()
	if snap.LastRefund == nil || *snap.LastRefund != 1.5 {
		t.Errorf("LastRefund = %+v, want 1.5", snap.LastRefund)
	}
	if snap.PendingRefund != "" {
		t.Errorf("PendingRefund = %q, want empty (settled)", snap.PendingRefund)
	}
}

// TestEndSessionRefundPendingReplay pins the pending-refund store flow with
// replay semantics: a pending receipt parks the instance, a slotless
// EndSession replays its DELETE, and the settled replay records lastRefund
// (same amount on retry, per the wire comment).
func TestEndSessionRefundPendingReplay(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	deletes := 0
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
			w.Header().Set("Content-Type", "application/json")
			if deletes == 1 {
				_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefundPending":true}`)
			} else {
				_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefund":1.5}`)
			}
			return
		}
		http.NotFound(w, r)
	}
	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := mgr.Snapshot()
	if snap.PendingRefund == "" {
		t.Fatal("PendingRefund empty after pending receipt, want parked instance")
	}
	if snap.LastRefund != nil {
		t.Errorf("LastRefund = %v, want nil (unsettled)", *snap.LastRefund)
	}
	// Slotless teardown replays the parked DELETE and settles the same amount.
	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deletes != 2 {
		t.Errorf("deletes = %d, want 2 (release + replay)", deletes)
	}
	snap = mgr.Snapshot()
	if snap.PendingRefund != "" {
		t.Errorf("PendingRefund = %q, want cleared after settle", snap.PendingRefund)
	}
	if snap.LastRefund == nil || *snap.LastRefund != 1.5 {
		t.Errorf("LastRefund = %+v, want 1.5", snap.LastRefund)
	}
}

// TestRefreshRefundKeepsPendingOnError pins that a failed replay keeps the
// entry (a later EndSession retries) while a gone row clears it.
func TestRefreshRefundKeepsPendingOnError(t *testing.T) {
	t.Run("transport error keeps pending", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)
		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"ended","freebucksRefundPending":true}`)
				return
			}
			http.NotFound(w, r)
		}
		if err := mgr.EndSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
		}
		if err := mgr.RefreshRefund(context.Background()); err == nil {
			t.Error("RefreshRefund 500 succeeded, want error")
		}
		if got := mgr.Snapshot().PendingRefund; got == "" {
			t.Error("PendingRefund cleared on failed replay, want kept")
		}
	})

	t.Run("gone row clears pending", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)
		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"ended","freebucksRefundPending":true}`)
				return
			}
			http.NotFound(w, r)
		}
		if err := mgr.EndSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"gone"}`)
		}
		if err := mgr.RefreshRefund(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := mgr.Snapshot().PendingRefund; got != "" {
			t.Errorf("PendingRefund = %q, want cleared (row gone)", got)
		}
	})
}
