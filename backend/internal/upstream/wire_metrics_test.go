package upstream

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/testutil"
)

// entryFields returns the Fields of the newest entry whose message matches,
// or nil when absent.
func entryFields(entries []logring.Entry, msg string) []string {
	for _, e := range entries {
		if e.Message == msg {
			return e.Fields
		}
	}
	return nil
}

// TestDoUpstreamResponseLogsAndPreservesBody pins the upstream response log: a
// response is logged as `upstream response` (redacted body, error class,
// req_id when present) and the body is re-wrapped so the caller's
// classification still parses it (retryAfterMs survives the round-trip).
func TestDoUpstreamResponseLogsAndPreservesBody(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	const upstreamBody = `{"error":"free_mode_rate_limited","message":"wait 30 minutes before retrying cb_token_abc","retryAfterMs":1800000}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer srv.Close()

	client, err := New("tok-0", &config.Config{UpstreamBaseURL: srv.URL, CostMode: "free"})
	if err != nil {
		t.Fatal(err)
	}

	ring := logring.NewHandler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}), 200)
	orig := slog.Default()
	slog.SetDefault(slog.New(ring))
	t.Cleanup(func() { slog.SetDefault(orig) })

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, cancel, cerr := client.do(req, 5*time.Second)
	if cerr == nil {
		t.Fatal("expected a classified error from do() on >=400 (issue #305)")
	}
	defer cancel()
	_ = resp.Body.Close()

	// do() classified once and carried the body forward: retryAfterMs
	// survived, and the redacted body never leaks the token downstream.
	var rle *RateLimitError
	if !errors.As(cerr, &rle) {
		t.Fatalf("do() classification = %T, want *RateLimitError", cerr)
	}
	if rle.RetryAfter != 30*time.Minute {
		t.Errorf("RetryAfter = %v, want 30m (body must survive the re-wrap)", rle.RetryAfter)
	}
	if strings.Contains(rle.Body, "cb_token_abc") {
		t.Errorf("rle.Body leaked the redacted token: %q", rle.Body)
	}

	entries := ring.Recent(200)
	fields := entryFields(entries, "upstream response")
	if fields == nil {
		t.Fatalf("no `upstream response` line captured")
	}
	joined := strings.Join(fields, " ")
	for _, want := range []string{
		"method=POST",
		"path=/api/v1/chat/completions",
		"status=429",
		"class=RateLimitError",
		"wait 30 minutes before retrying",
		"[redacted]",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("`upstream response` missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "cb_token_abc") {
		t.Errorf("`upstream response` body not redacted: %s", joined)
	}

	// Rate-limit classification line: FULL redacted body, correct code + window.
	fields = entryFields(entries, "upstream rate limit classified")
	if fields == nil {
		t.Fatalf("no `upstream rate limit classified` line captured")
	}
	joined = strings.Join(fields, " ")
	for _, want := range []string{
		"status=429",
		"code=free_mode_rate_limited",
		"window=\"30 minutes\"",
		"retry_after=1800",
		"wait 30 minutes before retrying", // full body, not 200-rune truncated
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("`upstream rate limit classified` missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "cb_token_abc") {
		t.Errorf("classification body not redacted: %s", joined)
	}

	// Ledger counter incremented exactly once.
	events := client.RateLimitEvents()
	if events["free_mode_rate_limited"] != 1 {
		t.Errorf("RateLimitEvents[free_mode_rate_limited] = %d, want 1 (all: %v)", events["free_mode_rate_limited"], events)
	}
}

// TestDoKeepsUpstreamOkForSuccess pins the success split: a <400 response still logs
// `upstream ok` and is returned untouched (no re-wrap, no class/body).
func TestDoKeepsUpstreamOkForSuccess(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: ok\n\n"))
	}))
	defer srv.Close()

	client, err := New("tok-0", &config.Config{UpstreamBaseURL: srv.URL, CostMode: "free"})
	if err != nil {
		t.Fatal(err)
	}
	ring := logring.NewHandler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}), 100)
	orig := slog.Default()
	slog.SetDefault(slog.New(ring))
	t.Cleanup(func() { slog.SetDefault(orig) })

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, cancel, err := client.do(req, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	got := drainBody(resp.Body)
	if got != "data: ok\n\n" {
		t.Errorf("body = %q, want untouched 200 body", got)
	}
	entries := ring.Recent(100)
	if entryFields(entries, "upstream ok") == nil {
		t.Errorf("missing `upstream ok` for 200 response")
	}
	if entryFields(entries, "upstream response") != nil {
		t.Errorf("`upstream response` must not fire for 200 responses")
	}
	startFields := entryFields(entries, "upstream request")
	if startFields == nil {
		t.Errorf("missing `upstream request` start line for 200 response")
	} else if joined := strings.Join(startFields, " "); !strings.Contains(joined, "path=/api/v1/chat/completions") {
		t.Errorf("`upstream request` missing method path: %s", joined)
	}
}

// TestRateLimitClassificationLedger pins the ledger counters: distinct upstream
// body codes are counted independently, and non-rate-limit classifications
// (403 bans) never touch the ledger.
func TestRateLimitClassificationLedger(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	client, err := New("tok-0", &config.Config{UpstreamBaseURL: "http://127.0.0.1:1", CostMode: "free"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_ = client.classify(http.StatusTooManyRequests, `{"error":"free_mode_rate_limited","message":"wait 1 minute"}`, http.Header{})
	}
	_ = client.classify(http.StatusTooManyRequests, `{"error":"insufficient_quota","message":"load is saturated"}`, http.Header{})
	_ = client.classify(http.StatusTooManyRequests, `{"error":"limit_burst_rate","message":"slow down"}`, http.Header{})
	_ = client.classify(http.StatusTooManyRequests, `{"status":"rate_limited","retryAfterMs":48549499}`, http.Header{})
	_ = client.classify(http.StatusForbidden, `{"status":"banned"}`, http.Header{}) // NOT a rate-limit event

	events := client.RateLimitEvents()
	want := map[string]int64{
		"free_mode_rate_limited": 3,
		"insufficient_quota":     1,
		"limit_burst_rate":       1,
		"rate_limited":           1,
	}
	for code, n := range want {
		if events[code] != n {
			t.Errorf("events[%q] = %d, want %d (all: %v)", code, events[code], n, events)
		}
	}
	for code, n := range events {
		if want[code] != n {
			t.Errorf("unexpected ledger entry %q=%d", code, n)
		}
	}
}

// TestRateLimitWindowTable pins the shared window derivation table.
func TestRateLimitWindowTable(t *testing.T) {
	future := time.Now().Add(time.Hour)
	cases := []struct {
		name string
		body string
		err  error
		want string
	}{
		{"body 1 minute text", `{"error":"free_mode_rate_limited","message":"wait 1 minute"}`, &RateLimitError{RetryAfter: time.Minute}, "1 minute"},
		{"body 30 minutes text", `{"error":"free_mode_rate_limited","message":"wait 30 minutes"}`, &RateLimitError{RetryAfter: 30 * time.Minute}, "30 minutes"},
		{"reset wins over retry-after", `{}`, &RateLimitError{RetryAfter: time.Minute, ResetAt: future}, "reset"},
		{"retry-after", `{}`, &RateLimitError{RetryAfter: time.Minute}, "retry-after"},
		{"none", `{}`, &RateLimitError{}, "none"},
	}
	for _, tc := range cases {
		if got := rateLimitWindow(tc.body, tc.err); got != tc.want {
			t.Errorf("%s: rateLimitWindow = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRateLimitInfoExcludesNonRateLimit pins that the ledger code is empty
// for classifications outside the rate-limit family (no counter, no log).
func TestRateLimitInfoExcludesNonRateLimit(t *testing.T) {
	for _, err := range []error{
		&BanError{ResumesAt: time.Now().Add(time.Hour)},
		&WaitingRoomError{RetryAfter: time.Minute},
		&SessionSupersededError{Status: http.StatusConflict},
		&UpstreamError{Status: http.StatusBadGateway},
	} {
		if code, _ := rateLimitInfo(`{"error":"x"}`, err); code != "" {
			t.Errorf("rateLimitInfo(%T) = %q, want empty", err, code)
		}
	}
}
