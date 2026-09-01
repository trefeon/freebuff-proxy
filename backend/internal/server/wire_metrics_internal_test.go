package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/upstream"
)

// requestFailedFields returns the Fields of the newest `request failed`
// record, or nil when absent.
func requestFailedFields(entries []logring.Entry) []string {
	for _, e := range entries {
		if e.Message == "request failed" {
			return e.Fields
		}
	}
	return nil
}

// countRequestFailedCode counts `request failed` records carrying the exact
// code= field (e.g. "code=rate_limited").
func countRequestFailedCode(entries []logring.Entry, codeField string) int {
	n := 0
	for _, e := range entries {
		if e.Message != "request failed" {
			continue
		}
		for _, f := range e.Fields {
			if f == codeField {
				n++
				break
			}
		}
	}
	return n
}

// TestRequestFailedWarnDedupe pins D6: 100 identical rate_limited errors
// produce <=4 `request failed` WARNs (1st + every 50th) while the per-key
// ledger always counts every occurrence; non-rate-limit codes log every
// time. The client response is written on every call regardless.
func TestRequestFailedWarnDedupe(t *testing.T) {
	t.Run("rate_limited burst fires <=4 WARNs", func(t *testing.T) {
		ring := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 500)
		s := &Server{logger: slog.New(ring)}
		rle := &upstream.RateLimitError{Status: "", RetryAfter: time.Minute, Window: "reset", Body: "daily quota exhausted"}
		var gotStatus int
		for i := 0; i < 100; i++ {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			s.writeError(w, r, rle, "deepseek/deepseek-v4-flash", nil)
			gotStatus = w.Code
		}
		if gotStatus != http.StatusTooManyRequests {
			t.Errorf("response status = %d, want 429 even on suppressed WARNs", gotStatus)
		}
		if n := countRequestFailedCode(ring.Recent(500), "code=rate_limited"); n > 4 {
			t.Errorf("`request failed` WARNs = %d, want <= 4 for 100 identical rate_limited errors", n)
		}
		s.rateLimitDedupe.mu.Lock()
		n := s.rateLimitDedupe.m["bridge|rate_limited|reset"]
		s.rateLimitDedupe.mu.Unlock()
		if n != 100 {
			t.Errorf("dedupe ledger count = %d, want 100 (counter always increments)", n)
		}
	})

	t.Run("non-rate-limit codes log every time", func(t *testing.T) {
		ring := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 500)
		s := &Server{logger: slog.New(ring)}
		be := &upstream.BanError{ResumesAt: time.Now().Add(time.Hour), Body: `{"status":"banned"}`}
		for i := 0; i < 25; i++ {
			w := httptest.NewRecorder()
			s.writeError(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), be, "", nil)
		}
		if n := countRequestFailedCode(ring.Recent(500), "code=account_banned"); n != 25 {
			t.Errorf("`request failed` WARNs for banned = %d, want 25 (every time)", n)
		}
	})
}

// TestRequestFailedStructuredFields pins T6: the `request failed` WARN
// carries req_id, retry_after, reset_at, token and model when the caller and
// the error provide them.
func TestRequestFailedStructuredFields(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)

	t.Run("req_id token model retry_after", func(t *testing.T) {
		ring := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 100)
		s := &Server{logger: slog.New(ring)}
		rle := &upstream.RateLimitError{RetryAfter: 90 * time.Second, Window: "retry-after", Body: "quota"}
		ctx := context.WithValue(context.Background(), reqIDKey{}, "req-test-123")
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		s.writeError(w, r, rle, "deepseek/deepseek-v4-flash", &pool.Lease{Token: 0})
		fields := requestFailedFields(ring.Recent(100))
		if fields == nil {
			t.Fatal("no `request failed` WARN captured")
		}
		joined := strings.Join(fields, " ")
		for _, want := range []string{
			"req_id=req-test-123",
			"retry_after=90",
			"token=1",
			"model=deepseek/deepseek-v4-flash",
			"code=rate_limited",
			"status=429",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("`request failed` missing %q in %s", want, joined)
			}
		}
		if strings.Contains(joined, "reset_at=") {
			t.Errorf("unexpected reset_at when the error carries none: %s", joined)
		}
	})

	t.Run("reset_at when the error carries it", func(t *testing.T) {
		ring := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 100)
		s := &Server{logger: slog.New(ring)}
		rle := &upstream.RateLimitError{ResetAt: future, Window: "reset", Body: "quota"}
		w := httptest.NewRecorder()
		s.writeError(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), rle, "", &pool.Lease{Token: 0})
		fields := requestFailedFields(ring.Recent(100))
		if fields == nil {
			t.Fatal("no `request failed` WARN captured")
		}
		joined := strings.Join(fields, " ")
		if want := "reset_at=" + future.Format(time.RFC3339); !strings.Contains(joined, want) {
			t.Errorf("`request failed` missing %q in %s", want, joined)
		}
		if !strings.Contains(joined, "retry_after=") {
			t.Errorf("`request failed` missing derived retry_after in %s", joined)
		}
	})
}
