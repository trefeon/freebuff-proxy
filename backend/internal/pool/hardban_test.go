package pool

import (
	"context"
	"io"
	"net/http"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// TestAcquireSkipsHardBannedTokenBeforeUpstream pins the pool-level
// no-re-contact guarantee: after a 403 banned session create, subsequent
// Acquires must NOT hit upstream again (skip from the remembered hard-ban
// memory + fixed-token quarantine) instead of re-admitting a dead account.
func TestAcquireSkipsHardBannedTokenBeforeUpstream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var creates int
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"none"}`)
			return
		}
		creates++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"status":"banned"}`)
	}
	p := newTestPoolCfg(t, func(c *config.Config) { c.UpstreamBaseURL = mock.URL() }, mock)

	if _, err := p.Acquire(context.Background(), modelA); err == nil {
		t.Fatal("Acquire succeeded on a 403 banned session create, want error")
		return
	}
	if creates != 1 {
		t.Fatalf("session creates = %d, want 1", creates)
	}

	if _, err := p.Acquire(context.Background(), modelA); err == nil {
		t.Fatal("Acquire succeeded on a hard-banned token, want remembered ban error")
		return
	}
	if creates != 1 {
		t.Errorf("session creates after hard ban = %d, want 1 (no re-contact)", creates)
	}
}
