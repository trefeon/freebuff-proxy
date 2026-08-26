package pool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/internal/notify"
	"freebuff-proxy/internal/upstream"
)

// TestMismatchEscalationFiresOncePerWindow pins the issue #140 P1 guard:
// 3+ free_mode_invalid_agent_model refusals on one token inside 60s fire ONE
// agent_model_mismatch_escalation webhook (the operator alert the v0.11.3
// 403-storm ban never had), and a second storm inside the window does not
// re-fire. Non-matching refusal codes never count.
func TestMismatchEscalationFiresOncePerWindow(t *testing.T) {
	var posts atomic.Int64
	var gotEvent atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err == nil {
			gotEvent.Store(ev)
		}
		posts.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	p := newTestPool(t)
	p.SetNotifier(notify.New(srv.URL, nil))

	rle := &upstream.RateLimitError{Status: "free_mode_invalid_agent_model", RetryAfter: upstream.InvalidModelCooldown}

	// Two hits: below threshold, no POST.
	p.recordMismatchEscalation(0, rle)
	p.recordMismatchEscalation(0, rle)
	time.Sleep(50 * time.Millisecond)
	if posts.Load() != 0 {
		t.Fatalf("webhook posts after 2 hits = %d, want 0 (threshold is %d)", posts.Load(), mismatchThreshold)
	}

	// Third hit crosses the threshold: exactly one POST.
	p.recordMismatchEscalation(0, rle)
	deadline := time.Now().Add(3 * time.Second)
	for posts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if posts.Load() != 1 {
		t.Fatalf("webhook posts after 3 hits = %d, want 1", posts.Load())
	}
	ev, _ := gotEvent.Load().(notify.Event)
	if ev.Event != "agent_model_mismatch_escalation" {
		t.Errorf("event = %q, want agent_model_mismatch_escalation", ev.Event)
	}

	// A fourth hit inside the storm window: no new POST (one alert per storm).
	p.recordMismatchEscalation(0, rle)
	time.Sleep(50 * time.Millisecond)
	if posts.Load() != 1 {
		t.Fatalf("webhook posts after 4th in-window hit = %d, want still 1", posts.Load())
	}

	// Other refusal codes never count toward the window.
	p.recordMismatchEscalation(1, &upstream.RateLimitError{Status: "rate_limited"})
	p.recordMismatchEscalation(1, &upstream.RateLimitError{Status: "rate_limited"})
	p.recordMismatchEscalation(1, &upstream.RateLimitError{Status: "rate_limited"})
	time.Sleep(50 * time.Millisecond)
	if posts.Load() != 1 {
		t.Fatalf("rate_limited hits fired an escalation; posts = %d, want still 1", posts.Load())
	}
}
