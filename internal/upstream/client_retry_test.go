package upstream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/stealth"
	"freebuff-proxy/internal/testutil"
)

// flakyRT is a RoundTripper that fails the first failN calls with a fixed
// transport error, then serves a canned 200 SSE response. It records every
// request body and header so tests can assert GetBody replay and fingerprint
// rotation.
type flakyRT struct {
	failN       int
	calls       atomic.Int32
	err         error
	body        []byte
	seen        [][]byte
	header      http.Header
	seenHeaders []http.Header
}

func (f *flakyRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls.Add(1)
	var b []byte
	if req.Body != nil {
		b, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	f.seen = append(f.seen, b)
	f.seenHeaders = append(f.seenHeaders, req.Header.Clone())
	if int(f.calls.Load()) <= f.failN {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     f.header,
		Body:       io.NopCloser(bytes.NewReader(f.body)),
		Request:    req,
	}, nil
}

// newRetryClient builds a client with TRANSIENT_RETRIES enabled and a pinned
// TLS fingerprint (optional), with the retry backoff pinned to 1ms.
func newRetryClient(t *testing.T, baseURL string, retries int, fingerprint string) (*Client, *flakyRT) {
	t.Helper()
	client, err := New("tok-a", testConfig(baseURL, func(c *config.Config) {
		c.TransientRetries = retries
		c.TLSFingerprint = fingerprint
	}))
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = func() time.Duration { return time.Millisecond }
	rt := &flakyRT{}
	client.http.Transport = rt
	return client, rt
}

func TestClassifyRateLimit(t *testing.T) {
	body := `{"model":"deepseek/deepseek-v4-flash","entitlementBreakdown":{"base":6},"limit":6,"period":"pacific_day","resetTimeZone":"America/Los_Angeles","resetAt":"2026-08-12T07:00:00.000Z","windowHours":24,"recentCount":6.6,"status":"rate_limited","accessTier":"limited","retryAfterMs":48549499}`
	err := classifyError(429, body, http.Header{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("errors.Is(ErrRateLimited) = false, got %v", err)
	}
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want *RateLimitError, got %v", err)
	}
	if rle.RetryAfter != 48549499*time.Millisecond {
		t.Errorf("RetryAfter = %s, want 48549499ms", rle.RetryAfter)
	}
	if rle.Limit != 6 {
		t.Errorf("Limit = %v, want 6", rle.Limit)
	}
	if rle.RecentCount != 6.6 {
		t.Errorf("RecentCount = %v, want 6.6", rle.RecentCount)
	}
	wantReset, _ := time.Parse(time.RFC3339Nano, "2026-08-12T07:00:00.000Z")
	if !rle.ResetAt.Equal(wantReset) {
		t.Errorf("ResetAt = %v, want %v", rle.ResetAt, wantReset)
	}

	// Nested error payload with snake_case fields
	bodyNested := `{"error":{"status":"rate_limited","reset_at":"2026-08-15T07:00:00Z","retry_after_ms":120000}}`
	errNested := classifyError(429, bodyNested, http.Header{})
	if !errors.Is(errNested, ErrRateLimited) {
		t.Fatalf("nested: errors.Is(ErrRateLimited) = false, got %v", errNested)
	}
	var rleNested *RateLimitError
	if !errors.As(errNested, &rleNested) {
		t.Fatalf("nested: want *RateLimitError, got %v", errNested)
	}
	if rleNested.RetryAfter != 120*time.Second {
		t.Errorf("RetryAfter = %s, want 120s", rleNested.RetryAfter)
	}

	// Generic 429 with no timestamp, no period, and no Retry-After header is
	// an opaque refusal: bounded backoff (#140 P2), never a Pacific-midnight
	// lock.
	errGeneric := classifyError(429, `{"status":"rate_limited"}`, http.Header{})
	var rleGeneric *RateLimitError
	if !errors.As(errGeneric, &rleGeneric) {
		t.Fatalf("want *RateLimitError, got %v", errGeneric)
	}
	if !rleGeneric.ResetAt.IsZero() {
		t.Errorf("opaque 429 ResetAt = %v, want zero (no Pacific-midnight lock)", rleGeneric.ResetAt)
	}
	if rleGeneric.RetryAfter <= 0 || rleGeneric.RetryAfter > 5*time.Minute {
		t.Errorf("opaque 429 RetryAfter = %s, want bounded >0 and <= 5m", rleGeneric.RetryAfter)
	}
	if rleGeneric.RetryAfter != opaqueRateLimitBackoff {
		t.Errorf("opaque 429 RetryAfter = %s, want %s", rleGeneric.RetryAfter, opaqueRateLimitBackoff)
	}

	// Header fallback when body has no JSON quota fields.
	err2 := classifyError(429, "opaque body", http.Header{"Retry-After": {"300"}})
	if !errors.Is(err2, ErrRateLimited) {
		t.Fatalf("header fallback: errors.Is(ErrRateLimited) = false, got %v", err2)
	}
	var rle2 *RateLimitError
	if !errors.As(err2, &rle2) {
		t.Fatalf("header fallback: want *RateLimitError, got %v", err2)
	}
	if rle2.RetryAfter != 300*time.Second {
		t.Errorf("RetryAfter = %s, want 300s (header fallback)", rle2.RetryAfter)
	}
}

func TestNextPacificMidnight(t *testing.T) {
	next := NextPacificMidnight()
	if !next.After(time.Now()) {
		t.Fatalf("NextPacificMidnight %v is not after now %v", next, time.Now())
	}
	if next.Location() != time.UTC {
		t.Errorf("expected UTC location, got %v", next.Location())
	}
}

func TestClassifyBan(t *testing.T) {
	resumesAt := "2026-07-21T09:18:07+00:00"
	body := `{"status":"banned","resumes_at":"` + resumesAt + `"}`
	err := classifyError(403, body, http.Header{})
	if !errors.Is(err, ErrBanned) {
		t.Fatalf("errors.Is(ErrBanned) = false, got %v", err)
	}
	var be *BanError
	if !errors.As(err, &be) {
		t.Fatalf("want *BanError, got %v", err)
	}
	wantTime, _ := time.Parse(time.RFC3339, resumesAt)
	if !be.ResumesAt.Equal(wantTime) {
		t.Errorf("ResumesAt = %v, want %v", be.ResumesAt, wantTime)
	}

	// 403 banned without resumes_at.
	bodyNoTime := `{"status":"banned"}`
	err2 := classifyError(403, bodyNoTime, http.Header{})
	if !errors.Is(err2, ErrBanned) {
		t.Fatalf("errors.Is(ErrBanned) = false for no-resumes_at, got %v", err2)
	}
	var be2 *BanError
	if !errors.As(err2, &be2) {
		t.Fatalf("want *BanError, got %v", err2)
	}
	if !be2.ResumesAt.IsZero() {
		t.Errorf("ResumesAt = %v, want zero for missing resumes_at", be2.ResumesAt)
	}

	// 403 WITHOUT "status":"banned" must NOT be ErrBanned.
	bodyOther := `{"error":"forbidden"}`
	err3 := classifyError(403, bodyOther, http.Header{})
	if errors.Is(err3, ErrBanned) {
		t.Fatalf("403 without banned status must not be ErrBanned, got %v", err3)
	}
	var ue *UpstreamError
	if !errors.As(err3, &ue) {
		t.Fatalf("want UpstreamError, got %v", err3)
	}
}

// TestClassifyAccountSuspended verifies the G3 ban-class shape: the newest
// CLI's hard ban is 403 {"error":"account_suspended","message":"...suspended
// due to billing issues."} (reference/freebuff sdk run-cancellation.test.ts
// :314-359). It must route into the same parseBan path as "status":"banned"
// with NO resumes_at (24h default cooldown), while near-misses stay generic.
func TestClassifyAccountSuspended(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantBan   bool
		wantUAErr bool // generic UpstreamError
	}{
		{
			name:    "exact shape classifies ErrBanned without ResumesAt",
			status:  http.StatusForbidden,
			body:    `{"error":"account_suspended","message":"Your account has been suspended due to billing issues."}`,
			wantBan: true,
		},
		{
			name:      "hyphenated near-miss stays generic",
			status:    http.StatusForbidden,
			body:      `{"error":"account-suspended","message":"nope"}`,
			wantUAErr: true,
		},
		{
			name:      "message word without the error key stays generic",
			status:    http.StatusForbidden,
			body:      `{"error":"forbidden","message":"your friend was account_suspended once"}`,
			wantUAErr: true,
		},
		{
			name:      "other 403 body stays generic",
			status:    http.StatusForbidden,
			body:      `{"error":"forbidden"}`,
			wantUAErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyError(tc.status, tc.body, http.Header{})
			var be *BanError
			if tc.wantBan {
				if !errors.Is(err, ErrBanned) {
					t.Fatalf("errors.Is(ErrBanned) = false, got %v", err)
				}
				if !errors.As(err, &be) {
					t.Fatalf("want *BanError, got %v", err)
				}
				if !be.ResumesAt.IsZero() {
					t.Errorf("ResumesAt = %v, want zero (body carries no resumes_at)", be.ResumesAt)
				}
				return
			}
			if errors.Is(err, ErrBanned) {
				t.Fatalf("near-miss classified as ErrBanned: %v", err)
			}
			if tc.wantUAErr {
				var ue *UpstreamError
				if !errors.As(err, &ue) {
					t.Fatalf("want generic *UpstreamError, got %v", err)
				}
			}
		})
	}
}

// TestClassifyBanUnixMsResumesAt verifies parseBan decodes a unix-ms
// resumes_at (not just RFC3339): flex-time parsing must recover the unban
// time so the cooldown ends when the ban actually lifts.
func TestClassifyBanUnixMsResumesAt(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Time
	}{
		{"unix milliseconds", `{"status":"banned","resumes_at":1753075087000}`, time.UnixMilli(1753075087000)},
		{"unix seconds", `{"status":"banned","resumes_at":1753075087}`, time.Unix(1753075087, 0)},
		{"rfc3339", `{"status":"banned","resumes_at":"2026-07-21T09:18:07+00:00"}`, time.Date(2026, 7, 21, 9, 18, 7, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyError(403, tc.body, http.Header{})
			var be *BanError
			if !errors.As(err, &be) {
				t.Fatalf("want *BanError, got %v", err)
			}
			if !be.ResumesAt.Equal(tc.want) {
				t.Errorf("ResumesAt = %v, want %v", be.ResumesAt, tc.want)
			}
		})
	}
}

// TestClassifyCredits verifies a 402 payment-required response maps to a
// CreditsError unwrapping to ErrCredits (fresh free accounts hit this before
// the free tier kicks in, so it must NOT fall through to a generic
// UpstreamError).
func TestClassifyCredits(t *testing.T) {
	err := classifyError(402, `{"error":"insufficient credits"}`, http.Header{})
	var credErr *CreditsError
	if !errors.As(err, &credErr) {
		t.Fatalf("want CreditsError, got %v", err)
	}
	if credErr.Status != 402 {
		t.Errorf("status = %d, want 402", credErr.Status)
	}
	if !errors.Is(err, ErrCredits) {
		t.Error("not unwrap-able to ErrCredits")
	}
}

// TestClassifyFreeModeCLIRequired verifies the free-tier gate refusal is
// typed, so the gateway can distinguish "envelope missing" from a hard 403.
func TestClassifyFreeModeCLIRequired(t *testing.T) {
	body := `{"error":{"status":"free_mode_cli_required","message":"CLI fingerprint required for free tier"}}`
	err := classifyError(403, body, http.Header{})
	if !errors.Is(err, ErrFreeModeCLIRequired) {
		t.Fatalf("errors.Is(ErrFreeModeCLIRequired) = false, got %v", err)
	}
}

// TestClassifyCountryBlocked verifies a 403 country_blocked response maps to
// a CountryBlockedError carrying the parsed region fields.
func TestClassifyCountryBlocked(t *testing.T) {
	body := `{"status":"country_blocked","countryCode":"US","countryBlockReason":"Free mode is not available in your country","ipPrivacySignals":["vpn","proxy"]}`
	err := classifyError(403, body, http.Header{})
	var cbe *CountryBlockedError
	if !errors.As(err, &cbe) {
		t.Fatalf("want CountryBlockedError, got %v", err)
	}
	if cbe.CountryCode != "US" {
		t.Errorf("countryCode = %q, want US", cbe.CountryCode)
	}
	if cbe.CountryBlockReason != "Free mode is not available in your country" {
		t.Errorf("countryBlockReason = %q", cbe.CountryBlockReason)
	}
	if len(cbe.IpPrivacySignals) != 2 || cbe.IpPrivacySignals[0] != "vpn" || cbe.IpPrivacySignals[1] != "proxy" {
		t.Errorf("ipPrivacySignals = %v", cbe.IpPrivacySignals)
	}
	if !errors.Is(err, ErrCountryBlocked) {
		t.Error("not unwrap-able to ErrCountryBlocked")
	}
}

// TestClassifyCountryBlockedToleratesAbsentFields verifies a bare
// country_blocked body (compact poll) still classifies without panicking and
// leaves the optional fields zero.
func TestClassifyCountryBlockedToleratesAbsentFields(t *testing.T) {
	err := classifyError(403, `{"status":"country_blocked"}`, http.Header{})
	var cbe *CountryBlockedError
	if !errors.As(err, &cbe) {
		t.Fatalf("want CountryBlockedError, got %v", err)
	}
	if cbe.CountryCode != "" || cbe.CountryBlockReason != "" || len(cbe.IpPrivacySignals) != 0 {
		t.Errorf("expected zero optional fields, got %+v", cbe)
	}
	if !errors.Is(err, ErrCountryBlocked) {
		t.Error("not unwrap-able to ErrCountryBlocked")
	}
}

// TestClassifyDeploymentOutsideHoursRetryable verifies a
// deployment_outside_hours body (when no other classifier claims it) maps to
// an UpstreamError marked Retryable, not a hard failure.
func TestClassifyDeploymentOutsideHoursRetryable(t *testing.T) {
	err := classifyError(500, `{"status":"deployment_outside_hours","message":"Free mode is only available during operating hours"}`, http.Header{})
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("want UpstreamError, got %v", err)
	}
	if !upErr.Retryable {
		t.Error("Retryable = false, want true")
	}
	if upErr.Status != 500 {
		t.Errorf("status = %d, want 500", upErr.Status)
	}

	// Ordinary 500s stay non-retryable.
	errPlain := classifyError(500, `{"error":"boom"}`, http.Header{})
	var plain *UpstreamError
	if !errors.As(errPlain, &plain) {
		t.Fatalf("want UpstreamError, got %v", errPlain)
	}
	if plain.Retryable {
		t.Error("plain UpstreamError must not be Retryable")
	}
}

// TestTransientRetriesNotCountedWhenRetryCannotFire verifies the transient
// retry counter only counts retries that actually fire: no GetBody (GET) and
// a failed body replay must both leave the counter at 0.
func TestTransientRetriesNotCountedWhenRetryCannotFire(t *testing.T) {
	t.Run("nil GetBody never counts", func(t *testing.T) {
		client, rt := newRetryClient(t, "", 1, "")
		rt.failN = 1
		rt.err = errors.New("tls handshake failed")

		req, err := client.newRequest(context.Background(), http.MethodGet, "/api/v1/freebuff/session", nil)
		if err != nil {
			t.Fatal(err)
		}
		if req.GetBody != nil {
			t.Fatal("GET request should have nil GetBody")
		}
		req.Body = http.NoBody // GETs carry no body; the transport needs a non-nil reader
		resp, cancel, err := client.do(req, time.Second)
		if err == nil {
			_ = resp.Body.Close()
			releaseCancel(cancel)
			t.Fatal("want error (no retry possible for nil GetBody)")
		}
		if rt.calls.Load() != 1 {
			t.Errorf("upstream attempts = %d, want 1 (no retry for nil GetBody)", rt.calls.Load())
		}
		if got := client.TransientRetries(); got != 0 {
			t.Errorf("TransientRetries = %d, want 0", got)
		}
	})

	t.Run("failed body replay never counts", func(t *testing.T) {
		client, rt := newRetryClient(t, "", 1, "")
		rt.failN = 1
		rt.err = errors.New("tls handshake failed")

		req, err := client.newRequest(context.Background(), http.MethodPost, "/api/v1/freebuff/session", []byte("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("replay unavailable") }
		resp, cancel, err := client.do(req, time.Second)
		if err == nil {
			_ = resp.Body.Close()
			releaseCancel(cancel)
			t.Fatal("want error when replay fails and no retry fires")
		}
		if got := client.TransientRetries(); got != 0 {
			t.Errorf("TransientRetries = %d, want 0 (counted only after successful replay)", got)
		}
	})
}

// TestPacificMidnightFallback pins the tzdata-less fallback: Pacific is
// UTC-7 (07:00 UTC midnight) March-November and UTC-8 (08:00 UTC) otherwise.
func TestPacificMidnightFallback(t *testing.T) {
	jan := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	if got := pacificMidnightFallback(jan); got.Hour() != 8 {
		t.Errorf("January fallback hour = %d, want 8 (PST)", got.Hour())
	}
	jul := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	if got := pacificMidnightFallback(jul); got.Hour() != 7 {
		t.Errorf("July fallback hour = %d, want 7 (PDT)", got.Hour())
	}
	if !pacificMidnightFallback(jan).After(jan) || !pacificMidnightFallback(jul).After(jul) {
		t.Error("fallback must return a time after the reference now")
	}
}

func TestChatCompletionsRetriesTransientFailure(t *testing.T) {
	client, rt := newRetryClient(t, "", 1, "")
	rt.failN = 1
	rt.err = errors.New("tls handshake failed")
	rt.body = []byte(testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`) + "data: [DONE]\n\n")

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if err != nil {
		t.Fatalf("ChatCompletions failed after transient retry: %v", err)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()

	if rt.calls.Load() != 2 {
		t.Errorf("upstream attempts = %d, want 2 (1 failure + 1 retry)", rt.calls.Load())
	}
	if got := client.TransientRetries(); got != 1 {
		t.Errorf("TransientRetries = %d, want 1", got)
	}
	// GetBody replay must re-send an identical payload.
	if len(rt.seen) != 2 || string(rt.seen[0]) != string(rt.seen[1]) {
		t.Errorf("replayed body differs: %q vs %q", rt.seen[0], rt.seen[1])
	}
	if !strings.Contains(string(rt.seen[0]), `"run_id":"r"`) {
		t.Errorf("first attempt body missing envelope: %q", rt.seen[0])
	}
}

func TestChatCompletionsRetriesTwiceWhenAllowed(t *testing.T) {
	client, rt := newRetryClient(t, "", 2, "")
	rt.failN = 2
	rt.err = io.EOF
	rt.body = []byte(testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`) + "data: [DONE]\n\n")

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if err != nil {
		t.Fatalf("ChatCompletions failed after 2 retries: %v", err)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()

	if rt.calls.Load() != 3 {
		t.Errorf("upstream attempts = %d, want 3 (2 failures + 2 retries)", rt.calls.Load())
	}
	if got := client.TransientRetries(); got != 2 {
		t.Errorf("TransientRetries = %d, want 2", got)
	}
	for i := 1; i < len(rt.seen); i++ {
		if string(rt.seen[i]) != string(rt.seen[0]) {
			t.Errorf("attempt %d body differs from attempt 0: %q vs %q", i, rt.seen[i], rt.seen[0])
		}
	}
}

func TestCreateSessionRetriesConnectionReset(t *testing.T) {
	// A real abrupt connection close surfaces as context.Canceled on some
	// platforms (Go cancels the request context when the server tears the
	// connection down mid-request), which MUST NOT be retried. Inject the
	// transport-level reset at the RoundTripper boundary instead: this is
	// the same code path a live dial/TLS failure takes.
	rt := &flakyRT{
		failN:  1,
		err:    errors.New("read tcp 127.0.0.1:443: connection reset by peer"),
		header: http.Header{"Content-Type": []string{"application/json"}},
		body:   []byte(`{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`),
	}
	client, err := New("tok-a", testConfig("", func(c *config.Config) { c.TransientRetries = 1 }))
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = func() time.Duration { return time.Millisecond }
	client.SetTransport(rt)

	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatalf("CreateSession failed after retry: %v", err)
	}
	if st.Status != "active" || st.InstanceID != "inst-1" {
		t.Errorf("session = %+v, want active inst-1", st)
	}
	if rt.calls.Load() != 2 {
		t.Errorf("upstream attempts = %d, want 2 (1 failure + 1 retry)", rt.calls.Load())
	}
	if got := client.TransientRetries(); got != 1 {
		t.Errorf("TransientRetries = %d, want 1", got)
	}
	// The session POST body was replayed identically.
	if len(rt.seen) != 2 || string(rt.seen[0]) != string(rt.seen[1]) {
		t.Errorf("replayed session body differs: %q vs %q", rt.seen[0], rt.seen[1])
	}
}

func TestRateLimitNeverRetried(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimit = true

	client, err := New("tok-a", testConfig(mock.URL(), func(c *config.Config) { c.TransientRetries = 3 }))
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = func() time.Duration { return time.Millisecond }

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if mock.Requests != 1 {
		t.Errorf("upstream requests = %d, want exactly 1 (429 must never be retried)", mock.Requests)
	}
	if got := client.TransientRetries(); got != 0 {
		t.Errorf("TransientRetries = %d, want 0", got)
	}
}

func TestBanNeverRetried(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true

	client, err := New("tok-a", testConfig(mock.URL(), func(c *config.Config) { c.TransientRetries = 3 }))
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = func() time.Duration { return time.Millisecond }

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if !errors.Is(err, ErrBanned) {
		t.Fatalf("err = %v, want ErrBanned", err)
	}
	if mock.Requests != 1 {
		t.Errorf("upstream requests = %d, want exactly 1 (403 banned must never be retried)", mock.Requests)
	}
	if got := client.TransientRetries(); got != 0 {
		t.Errorf("TransientRetries = %d, want 0", got)
	}
}

func TestTransientRetriesDisabledSingleAttempt(t *testing.T) {
	client, rt := newRetryClient(t, "", 0, "")
	rt.failN = 100
	rt.err = errors.New("connection reset by peer")
	rt.body = []byte(testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`))

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	_, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if err == nil {
		t.Fatal("want error when every attempt fails")
	}
	if rt.calls.Load() != 1 {
		t.Errorf("upstream attempts = %d, want exactly 1 (TRANSIENT_RETRIES=0)", rt.calls.Load())
	}
	if got := client.TransientRetries(); got != 0 {
		t.Errorf("TransientRetries = %d, want 0", got)
	}
}

func TestRetryRotatesPinnedFingerprint(t *testing.T) {
	client, rt := newRetryClient(t, "", 1, "chrome126")
	rt.failN = 1
	rt.err = errors.New("tls handshake failed")
	rt.body = []byte(testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`))

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if err != nil {
		t.Fatalf("ChatCompletions failed after retry: %v", err)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()

	if got := client.FingerprintRotations(); got != 1 {
		t.Errorf("FingerprintRotations = %d, want 1", got)
	}
	client.profileMu.Lock()
	id := client.stealthProfile.ID
	client.profileMu.Unlock()
	if id != stealth.ProfileIDSafari18 {
		t.Errorf("stealthProfile = %s, want safari18 (chrome126 rotated to a distinct JA3)", id)
	}
	// Headers stay CLI-shaped across the retry (#109): the fingerprint
	// rotates at the TLS layer only; no browser persona ever touches the API
	// request, so there are no Sec-CH-UA/Sec-Fetch-* headers to carry over.
	if rt.calls.Load() != 2 {
		t.Fatalf("upstream attempts = %d, want 2", rt.calls.Load())
	}
	for i, wantAttempt := range []string{"attempt 1", "attempt 2 (rotated)"} {
		if got := rt.seenHeaders[i].Get("User-Agent"); got != cliUserAgent {
			t.Errorf("%s User-Agent = %q, want the CLI UA %q", wantAttempt, got, cliUserAgent)
		}
	}
	for _, hdr := range []string{"Sec-CH-UA", "Sec-CH-UA-Mobile", "Sec-CH-UA-Platform",
		"Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest"} {
		for i := 0; i < 2; i++ {
			if got := rt.seenHeaders[i].Get(hdr); got != "" {
				t.Errorf("attempt %d %s = %q, want absent (no browser headers on API calls, #109)", i+1, hdr, got)
			}
		}
	}
}

func TestRetryDoesNotRotateAutoProfile(t *testing.T) {
	client, rt := newRetryClient(t, "", 1, "auto")
	rt.failN = 1
	rt.err = errors.New("tls handshake failed")
	rt.body = []byte(testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`))

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if err != nil {
		t.Fatalf("ChatCompletions failed after retry: %v", err)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()

	if got := client.FingerprintRotations(); got != 0 {
		t.Errorf("FingerprintRotations = %d, want 0 (auto rotates per connection already)", got)
	}
	client.profileMu.Lock()
	id := client.stealthProfile.ID
	client.profileMu.Unlock()
	if id != stealth.ProfileIDAuto {
		t.Errorf("stealthProfile = %s, want auto (unchanged)", id)
	}
}

func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"tls handshake failed (wrapper)", errors.New("tls handshake failed: EOF"), true},
		{"tls handshake failure (Go alert)", errors.New("remote error: tls: handshake failure"), true},
		{"tls internal error", errors.New("tls: internal error"), true},
		{"connection refused", errors.New(`dial tcp 127.0.0.1:443: connect: connection refused`), true},
		{"connection reset by peer", errors.New("read tcp 1.2.3.4:443: connection reset by peer"), true},
		{"EOF", io.EOF, true},
		{"unexpected EOF", errors.New("unexpected EOF"), true},
		{"eof substring not retried", errors.New("peer closed with eof marker"), false},
		{"eof substring wrapped", fmt.Errorf("stealth: tcp dial failed: %w", errors.New("read tcp 1.2.3.4:443: eof reached")), false},
		{"network unreachable", errors.New(`dial tcp 1.2.3.4:443: connect: network is unreachable`), true},
		{"no route to host", errors.New(`dial tcp 1.2.3.4:443: connect: no route to host`), true},
		{"dial i/o timeout", errors.New(`dial tcp 1.2.3.4:443: i/o timeout`), true},
		{"stealth-wrapped connection reset", fmt.Errorf("stealth: tcp dial failed: %w", errors.New("connection reset by peer")), true},
		{"url-wrapped EOF", &url.Error{Op: "Post", URL: "https://www.codebuff.com/api/v1/chat/completions", Err: io.EOF}, true},
		{"context canceled", context.Canceled, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"tls bad certificate", errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"), false},
		{"rate limit body", errors.New("upstream rate limited"), false},
		{"arbitrary error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransient(tc.err); got != tc.want {
				t.Errorf("isTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestParseRetryAfterAndFlexTime guards the time parsers' edge branches
// (G2): HTTP-date Retry-After, zero/negative/garbage values, and
// numeric-string unix seconds for flex times.
func TestParseRetryAfterAndFlexTime(t *testing.T) {
	t.Run("http-date", func(t *testing.T) {
		future := time.Now().Add(90 * time.Second).UTC()
		hdr := http.Header{}
		hdr.Set("Retry-After", future.Format(http.TimeFormat))
		got := parseRetryAfter(hdr)
		if got <= 0 || got > 3*time.Minute {
			t.Errorf("HTTP-date Retry-After = %v, want ~90s", got)
		}
	})
	t.Run("seconds, zero, negative, garbage", func(t *testing.T) {
		cases := []struct {
			raw  string
			want time.Duration
		}{
			{"30", 30 * time.Second},
			{"0", 0},
			{"-5", 0},
			{"garbage", 0},
			{"", 0},
		}
		for _, tc := range cases {
			hdr := http.Header{}
			hdr.Set("Retry-After", tc.raw)
			if got := parseRetryAfter(hdr); got != tc.want {
				t.Errorf("Retry-After %q = %v, want %v", tc.raw, got, tc.want)
			}
		}
	})
	t.Run("flex time numeric string seconds", func(t *testing.T) {
		got, err := parseFlexTime("1753075087")
		if err != nil {
			t.Fatalf("parseFlexTime(string seconds): %v", err)
		}
		if want := time.Unix(1753075087, 0); !got.Equal(want) {
			t.Errorf("parseFlexTime = %v, want %v", got, want)
		}
	})
	t.Run("flex time nil and empty error", func(t *testing.T) {
		if _, err := parseFlexTime(nil); err == nil {
			t.Error("parseFlexTime(nil) succeeded")
		}
		if _, err := parseFlexTime(""); err == nil {
			t.Error("parseFlexTime(\"\") succeeded")
		}
	})
}

// TestDoBackoffCancelAndDeadline guards the do() retry-loop branches (G3):
// ctx cancellation during the backoff aborts without a second attempt; a
// pre-existing deadline skips the internal timeout entirely; and after the
// retry budget is exhausted the error surfaces as a non-transient wrap.
func TestDoBackoffCancelAndDeadline(t *testing.T) {
	t.Run("ctx cancel during backoff aborts", func(t *testing.T) {
		client, rt := newRetryClient(t, "", 1, "")
		// Block the backoff so cancellation has a window to land in.
		client.retryBackoff = func() time.Duration { return time.Hour }
		rt.failN = 1
		rt.err = errors.New("connection reset by peer")

		ctx, cancel := context.WithCancel(context.Background())
		req, err := client.newRequest(ctx, http.MethodPost, "/api/v1/chat/completions", []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := client.do(req, 0)
			done <- err
		}()
		// Wait for the first (failed) attempt, then cancel mid-backoff.
		deadline := time.Now().Add(5 * time.Second)
		for rt.calls.Load() < 1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if rt.calls.Load() != 1 {
			t.Fatalf("first attempt never ran (calls=%d)", rt.calls.Load())
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("do() did not return after cancel during backoff")
		}
		if rt.calls.Load() != 1 {
			t.Errorf("calls = %d after cancel, want 1 (no retry fired)", rt.calls.Load())
		}
	})

	t.Run("pre-existing deadline keeps control timeout when tighter", func(t *testing.T) {
		client, _ := newRetryClient(t, "", 0, "")
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		req, err := client.newRequest(ctx, http.MethodPost, "/api/v1/chat/completions", []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, cfn, err := client.do(req, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		// The 30s control timeout is tighter than the caller's 1h deadline,
		// so it must be applied (cancel non-nil) — a long caller deadline
		// must not silently defeat SessionCallTimeout on control calls.
		if cfn == nil {
			t.Error("control timeout not applied despite being tighter than the caller deadline (cancel must be non-nil)")
		}
	})

	t.Run("pre-existing deadline skips looser timeout", func(t *testing.T) {
		client, _ := newRetryClient(t, "", 0, "")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := client.newRequest(ctx, http.MethodPost, "/api/v1/chat/completions", []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		// The control timeout (1h) is LOOSER than the caller deadline; it
		// must not override the tighter caller bound.
		resp, cfn, err := client.do(req, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if cfn != nil {
			t.Error("looser timeout applied over a tighter caller deadline (cancel must be nil)")
		}
	})

	t.Run("exhausted budget returns non-transient wrap", func(t *testing.T) {
		client, rt := newRetryClient(t, "", 1, "")
		rt.failN = 2
		rt.err = errors.New("connection reset by peer")
		req, err := client.newRequest(context.Background(), http.MethodPost, "/api/v1/chat/completions", []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = client.do(req, 0)
		if err == nil {
			t.Fatal("expected an error after exhausting the retry budget")
		}
		if !strings.Contains(err.Error(), "upstream:") {
			t.Errorf("err = %v, want an upstream-wrapped error", err)
		}
		if rt.calls.Load() != 2 {
			t.Errorf("calls = %d, want 2 (initial + 1 retry)", rt.calls.Load())
		}
		if got := client.TransientRetries(); got != 1 {
			t.Errorf("TransientRetries = %d, want 1", got)
		}
	})
}

// TestClassifyErrorMatrix guards the 403 classification matrix (G4): the
// narrowed banned marker, deployment_outside_hours precedence across
// statuses, chat-level session markers, 500+rate_limited bodies, and
// ban-before-rate discrimination (E2E flow 3).
func TestClassifyErrorMatrix(t *testing.T) {
	t.Run("banned substring does not over-match", func(t *testing.T) {
		// Regression for Audit B5: a 403 body merely mentioning "banned"
		// (not the {"status":"banned"} marker) must stay a generic 403.
		err := classifyError(403, `{"error":"model temporarily banned from free tier"}`, http.Header{})
		if errors.Is(err, ErrBanned) {
			t.Fatalf("403 with the word banned but no status marker classified as ErrBanned: %v", err)
		}
		var upErr *UpstreamError
		if !errors.As(err, &upErr) || upErr.Status != 403 {
			t.Errorf("err = %v, want a generic 403 UpstreamError", err)
		}
	})

	t.Run("status banned marker still classifies", func(t *testing.T) {
		err := classifyError(403, `{"status":"banned","resumes_at":"2026-07-21T09:18:07+00:00"}`, http.Header{})
		if !errors.Is(err, ErrBanned) {
			t.Errorf("err = %v, want ErrBanned", err)
		}
	})

	t.Run("ban beats rate_limited text", func(t *testing.T) {
		// E2E flow 3: a banned body that also mentions rate_limited text
		// still classifies as a ban (first case wins).
		err := classifyError(403, `{"status":"banned","error":"rate_limited","resumes_at":"2026-07-21T09:18:07+00:00"}`, http.Header{})
		if !errors.Is(err, ErrBanned) {
			t.Errorf("err = %v, want ErrBanned (ban must beat rate text)", err)
		}
	})

	t.Run("deployment_outside_hours preempts status cases", func(t *testing.T) {
		// Pin current behavior (Audit B6, NOT fixed): the marker wins over
		// 401/403/429 classification and yields a retryable UpstreamError.
		cases := []struct {
			name   string
			status int
			body   string
			notErr error
		}{
			{"401", 401, `{"status":"deployment_outside_hours"}`, ErrAuthRejected},
			{"403", 403, `{"status":"deployment_outside_hours"}`, ErrFreeModeCLIRequired},
			{"429", 429, `{"status":"deployment_outside_hours"}`, ErrRateLimited},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := classifyError(tc.status, tc.body, http.Header{})
				var upErr *UpstreamError
				if !errors.As(err, &upErr) {
					t.Fatalf("err = %v, want UpstreamError", err)
				}
				if !upErr.Retryable {
					t.Errorf("deployment_outside_hours not marked Retryable: %v", err)
				}
				if errors.Is(err, tc.notErr) {
					t.Errorf("err = %v, must not classify as %v", err, tc.notErr)
				}
			})
		}
	})

	t.Run("chat-level session markers", func(t *testing.T) {
		err := classifyError(409, `{"status":"model_locked","currentModel":"a","requestedModel":"b"}`, http.Header{})
		if !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("model_locked at chat level = %v, want ErrSessionInvalid", err)
		}
		err = classifyError(400, `{"status":"session_model_mismatch"}`, http.Header{})
		if !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("session_model_mismatch at chat level = %v, want ErrSessionInvalid", err)
		}
	})

	t.Run("session_model_mismatch limited on egress IP", func(t *testing.T) {
		cases := []struct {
			name      string
			body      string
			hdr       http.Header
			wantRetry time.Duration
		}{
			{
				name: "limited marker",
				body: `{"status":"session_model_mismatch","message":"model kimi/kimi-k2-0725 is limited on this IP"}`,
			},
			{
				name:      "retry-after header honored",
				body:      `{"status":"session_model_mismatch","message":"model kimi/kimi-k2-0725 is limited on this IP"}`,
				hdr:       http.Header{"Retry-After": {"120"}},
				wantRetry: 120 * time.Second,
			},
			{
				name: "production limited free access message",
				body: `{"error":"session_model_mismatch","message":"Limited free access is only available with DeepSeek V4 Flash or MiMo 2.5."}`,
			},
			{
				name: "status variant with limited free access",
				body: `{"status":"session_model_mismatch","message":"Limited free access is only available with DeepSeek V4 Flash."}`,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := classifyError(409, tc.body, tc.hdr)
				var lie *LimitedIpError
				if !errors.As(err, &lie) {
					t.Fatalf("err = %v, want *LimitedIpError", err)
				}
				if !errors.Is(err, ErrModelIPLimited) {
					t.Errorf("errors.Is(ErrModelIPLimited) = false, got %v", err)
				}
				if tc.wantRetry > 0 && lie.RetryAfter != tc.wantRetry {
					t.Errorf("RetryAfter = %s, want %s", lie.RetryAfter, tc.wantRetry)
				}
			})
		}
	})

	t.Run("500 with rate_limited body", func(t *testing.T) {
		// Pin current behavior: the rate_limited body marker wins even on a
		// 500, producing a RateLimitError.
		err := classifyError(500, `{"status":"rate_limited"}`, http.Header{})
		if !errors.Is(err, ErrRateLimited) {
			t.Errorf("err = %v, want ErrRateLimited", err)
		}
	})

	t.Run("chat-level country blocked", func(t *testing.T) {
		// E2E flow 2: a chat 403 country_blocked body surfaces the typed
		// CountryBlockedError with parsed fields, not a generic 403.
		err := classifyError(403, `{"status":"country_blocked","countryCode":"CN","countryBlockReason":"region_restricted","ipPrivacySignals":["vpn"]}`, http.Header{})
		var cbe *CountryBlockedError
		if !errors.As(err, &cbe) {
			t.Fatalf("err = %v, want CountryBlockedError", err)
		}
		if cbe.CountryCode != "CN" || cbe.CountryBlockReason != "region_restricted" {
			t.Errorf("country fields = %q/%q, want CN/region_restricted", cbe.CountryCode, cbe.CountryBlockReason)
		}
		if len(cbe.IpPrivacySignals) != 1 || cbe.IpPrivacySignals[0] != "vpn" {
			t.Errorf("ipPrivacySignals = %v, want [vpn]", cbe.IpPrivacySignals)
		}
	})
}

// TestFailedReplayMetricsDisagreement pins the current metrics behavior on a
// failed body replay (Audit B3, NOT fixed): the pinned fingerprint is rotated
// and counted BEFORE the replay, so a failed replay leaves FingerprintRotations
// incremented with no TransientRetries and the profile permanently swapped.
func TestFailedReplayMetricsDisagreement(t *testing.T) {
	client, rt := newRetryClient(t, "", 1, "chrome126")
	rt.failN = 1
	rt.err = errors.New("tls handshake failed")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://example.com/api/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("replay body unavailable") }

	// With the replay failing, do() surfaces the transient error directly
	// (no second attempt fires): the rotation already happened, the retry
	// did not.
	_, cfn, err := client.do(req, 0)
	if err == nil {
		t.Fatal("expected the transient error to surface when the body cannot be replayed")
	}
	if cfn != nil {
		defer cfn()
	}

	if got := client.FingerprintRotations(); got != 1 {
		t.Errorf("FingerprintRotations = %d, want 1 (rotation precedes the replay)", got)
	}
	if got := client.TransientRetries(); got != 0 {
		t.Errorf("TransientRetries = %d, want 0 (no retry fired)", got)
	}
	if got := client.currentStealthProfile().ID; got != stealth.ProfileIDSafari18 {
		t.Errorf("profile after failed replay = %s, want %s (permanently rotated)", got, stealth.ProfileIDSafari18)
	}
}

// TestChatRetriesRealDialFailure is E2E flow 1: a real transport failure
// (the listener accepts then hangs up mid-request) is retried over a fresh
// connection with a byte-identical replayed body, and the SSE stream reads
// back cleanly.
func TestChatRetriesRealDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	bodies := make(chan []byte, 2)
	sse := testutil.SSEEvent(`{"id":"c0","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`) +
		"data: [DONE]\n\n"

	go func() {
		for i := 0; i < 2; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			br := bufio.NewReader(conn)
			req, err := http.ReadRequest(br)
			if err != nil {
				// Keep accepting: the retry still needs a second connection.
				_ = conn.Close()
				continue
			}
			body, _ := io.ReadAll(req.Body)
			_ = req.Body.Close()
			bodies <- body
			if i == 0 {
				// Swallow the first request, then hang up: the client sees
				// a transport-level EOF and must retry.
				_ = conn.Close()
				continue
			}
			resp := "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: " +
				strconv.Itoa(len(sse)) + "\r\nConnection: close\r\n\r\n" + sse
			_, _ = conn.Write([]byte(resp))
			_ = conn.Close()
		}
	}()

	client, err := New("tok", testConfig("http://"+ln.Addr().String(), func(c *config.Config) {
		c.TransientRetries = 1
	}))
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = func() time.Duration { return time.Millisecond }

	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"},
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("chat failed after retry: %v", err)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("stream read: %v", err)
	}
	_ = rc.Close()
	if !strings.Contains(string(data), `"content":"hi"`) {
		t.Errorf("stream missing expected chunk: %s", data)
	}
	if got := client.TransientRetries(); got != 1 {
		t.Errorf("TransientRetries = %d, want 1", got)
	}

	first := <-bodies
	second := <-bodies
	if !bytes.Equal(first, second) {
		t.Errorf("replayed body differs:\n first: %s\nsecond: %s", first, second)
	}
}

// TestRetryRotatesFingerprintAtDialLayer is E2E flow 5: after a transient
// retry, the transport dials with the rotated profile's ClientHello — the
// dial-layer profile capture must show chrome126 then safari18.
func TestRetryRotatesFingerprintAtDialLayer(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`))
	}))
	defer tlsSrv.Close()

	client, err := New("tok", testConfig(tlsSrv.URL, func(c *config.Config) {
		c.TLSFingerprint = "chrome126"
		c.TransientRetries = 1
	}))
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = func() time.Duration { return time.Millisecond }

	tr := client.http.Transport.(*http.Transport)
	var mu sync.Mutex
	var dials []stealth.ProfileID
	var dialCount int
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		prof := client.dialProfileFor(ctx)
		mu.Lock()
		dialCount++
		dials = append(dials, prof.ID)
		mu.Unlock()
		if dialCount == 1 {
			return nil, errors.New("tls handshake failed: injected first-dial failure")
		}
		if prof.ID == stealth.ProfileIDSafari18 {
			// The package-level Safari profile shares one mutable
			// utls.ClientHelloSpec that the first utls handshake corrupts
			// (KeyShareExtension data is written in place), so a second
			// real dial through the shared spec is flaky (pre-existing
			// stealth bug, reported separately). Dial with utls's own
			// built-in Safari preset instead — still a Safari-family
			// fingerprint — while the rotation DECISION (chrome126 ->
			// safari18) is pinned by the captured profile IDs.
			p := *prof
			p.ClientHelloID = utls.HelloSafari_16_0
			p.CustomSpec = nil
			prof = &p
		}
		// Production hard-codes InsecureSkipVerify=false; the local test
		// server's self-signed cert requires true.
		return stealth.Dialer(prof, nil, true, nil)(ctx, network, addr)
	}

	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(dials) != 2 || dials[0] != stealth.ProfileIDChrome126 || dials[1] != stealth.ProfileIDSafari18 {
		t.Errorf("dialed profiles = %v, want [chrome126 safari18]", dials)
	}
	if got := client.TransientRetries(); got != 1 {
		t.Errorf("TransientRetries = %d, want 1", got)
	}
	if got := client.FingerprintRotations(); got != 1 {
		t.Errorf("FingerprintRotations = %d, want 1", got)
	}
}

// TestChatCompletionsRetriesCapacityDeferredSameSession verifies #75: a
// free_mode_capacity_deferred 429 is retried IN PLACE against the same
// lease/session (byte-identical body, same instance id) up to the
// TRANSIENT_RETRIES budget, and surfaces the typed retryable error once the
// budget is exhausted.
func TestChatCompletionsRetriesCapacityDeferredSameSession(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	deferred := `{"error":{"code":"free_mode_capacity_deferred","message":"Free mode is at capacity; your request will be retried automatically"}}`

	t.Run("retries same session then succeeds", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		var calls atomic.Int32
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				// #105: a short retry-after — the client must sleep it (floor
				// 10s) before re-POSTing, not retry immediately.
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, deferred)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`))
		}
		client, err := New("tok-a", testConfig(mock.URL(), func(c *config.Config) { c.TransientRetries = 1 }))
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r", SessionInstanceID: "inst-1"}, body)
		if err != nil {
			t.Fatalf("ChatCompletions after capacity-deferred retry: %v", err)
		}
		_ = rc.Close()
		// The retry-after (1s) must have been honored before the retry POST.
		if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
			t.Errorf("capacity-deferred retry elapsed %v, want >= the 1s Retry-After sleep (#105)", elapsed)
		}

		if got := calls.Load(); got != 2 {
			t.Errorf("upstream chat calls = %d, want 2 (original + same-session retry)", got)
		}
		if got := client.CapacityDeferredRetries(); got != 1 {
			t.Errorf("CapacityDeferredRetries = %d, want 1", got)
		}
		if len(mock.RecordedChatHeaders) != 2 {
			t.Fatalf("recorded %d chat requests, want 2", len(mock.RecordedChatHeaders))
		}
		// Same session on the retry: the instance id rides in the body
		// metadata, not the chat headers (#106).
		if got := mock.RecordedChatHeaders[1].Get("x-freebuff-instance-id"); got != "" {
			t.Errorf("retry x-freebuff-instance-id = %q, want absent (chat headers carry no instance id)", got)
		}
		if !strings.Contains(mock.RecordedChatBodies[0], `"freebuff_instance_id":"inst-1"`) {
			t.Error("chat body missing freebuff_instance_id in codebuff_metadata")
		}
		if mock.RecordedChatBodies[0] != mock.RecordedChatBodies[1] {
			t.Error("retried body differs from original (must be byte-identical)")
		}
	})

	t.Run("budget exhausted surfaces typed retryable error", func(t *testing.T) {
		mock2 := testutil.NewMock()
		defer mock2.Close()
		var calls2 atomic.Int32
		mock2.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			calls2.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, deferred)
		}
		client2, err := New("tok-b", testConfig(mock2.URL(), func(c *config.Config) { c.TransientRetries = 1 }))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client2.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
		if !errors.Is(err, ErrCapacityDeferred) {
			t.Fatalf("err = %v, want ErrCapacityDeferred", err)
		}
		var cde *CapacityDeferredError
		if !errors.As(err, &cde) {
			t.Fatalf("err = %v, want *CapacityDeferredError", err)
		}
		var ue *UpstreamError
		if !errors.As(err, &ue) || !ue.Retryable {
			t.Errorf("err = %v, want unwrap to Retryable UpstreamError", err)
		}
		if got := calls2.Load(); got != 2 {
			t.Errorf("upstream chat calls = %d, want 2 (original + 1 budgeted retry)", got)
		}
		if got := client2.CapacityDeferredRetries(); got != 1 {
			t.Errorf("CapacityDeferredRetries = %d, want 1", got)
		}
	})

	t.Run("zero budget never retries", func(t *testing.T) {
		mock3 := testutil.NewMock()
		defer mock3.Close()
		var calls3 atomic.Int32
		mock3.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			calls3.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, deferred)
		}
		client3, err := New("tok-c", testConfig(mock3.URL(), func(c *config.Config) { c.TransientRetries = 0 }))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client3.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
		if !errors.Is(err, ErrCapacityDeferred) {
			t.Fatalf("err = %v, want ErrCapacityDeferred", err)
		}
		if got := calls3.Load(); got != 1 {
			t.Errorf("upstream chat calls = %d, want 1 (retries disabled)", got)
		}
	})

	t.Run("budget resets per request", func(t *testing.T) {
		// Regression (review P1): the capacity-deferred budget must be
		// per-request, not client-lifetime. Two sequential requests on the
		// SAME client must each get their own TRANSIENT_RETRIES budget.
		mock4 := testutil.NewMock()
		defer mock4.Close()
		var calls4 atomic.Int32
		mock4.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			if calls4.Add(1)%2 == 1 { // first call of each request: deferred
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, deferred)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`))
		}
		client4, err := New("tok-d", testConfig(mock4.URL(), func(c *config.Config) { c.TransientRetries = 1 }))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			rc, err := client4.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
			if err != nil {
				t.Fatalf("request %d after capacity-deferred retry: %v", i+1, err)
			}
			_ = rc.Close()
		}
		if got := calls4.Load(); got != 4 {
			t.Errorf("upstream chat calls = %d, want 4 (2 requests x 2 calls: original + retry each)", got)
		}
		if got := client4.CapacityDeferredRetries(); got != 2 {
			t.Errorf("CapacityDeferredRetries = %d, want 2 (one retry per request)", got)
		}
	})
}

// TestClassifyIpCappedNoPacificMidnight verifies #81: a 429 ip_capped body
// with no explicit retryAfterMs gets a bounded 1m default — never the
// Pacific-midnight quota lock.
func TestClassifyIpCappedNoPacificMidnight(t *testing.T) {
	err := classifyError(http.StatusTooManyRequests, `{"status":"ip_capped"}`, http.Header{})
	if errors.Is(err, ErrRateLimited) {
		t.Fatal("ip_capped classified as ErrRateLimited, want distinct ErrIpCapped")
	}
	var ice *IpCappedError
	if !errors.As(err, &ice) {
		t.Fatalf("err = %v, want *IpCappedError", err)
	}
	if ice.RetryAfter != time.Minute {
		t.Errorf("RetryAfter = %v, want 1m bounded default (no Pacific midnight)", ice.RetryAfter)
	}
	if errors.Is(err, ErrIpCapped) == false {
		t.Errorf("err = %v, want ErrIpCapped", err)
	}
}

// TestClassifyLoadSheddingAndPeakHours pins issue #133: 429 bodies with the
// load-saturation and peak-hours markers classify as bounded cooldowns with
// distinct statuses — never the Pacific-midnight lock parseRateLimit would
// apply to a no-timestamp 429.
func TestClassifyLoadSheddingAndPeakHours(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantStatus  string
		wantCooldwn time.Duration
	}{
		{"load saturation", `{"status":"insufficient_quota","message":"The current group's upstream load is saturated, please try again later (request id: 42)"}`, "load_shedding", LoadShedCooldown},
		{"limit burst rate", `{"status":"limit_burst_rate","message":"upstream load saturated, try again later"}`, "load_shedding", LoadShedCooldown},
		{"peak hours", `{"status":"rate_limited","message":"Usage is temporarily limited during peak hours, when upstream model prices double"}`, "peak_hours", PeakHoursCooldown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyError(http.StatusTooManyRequests, tt.body, http.Header{})
			var rle *RateLimitError
			if !errors.As(err, &rle) {
				t.Fatalf("classifyError = %T %v, want *RateLimitError", err, err)
			}
			if rle.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", rle.Status, tt.wantStatus)
			}
			if rle.RetryAfter != tt.wantCooldwn {
				t.Errorf("RetryAfter = %v, want %v (bounded, not midnight)", rle.RetryAfter, tt.wantCooldwn)
			}
			if !rle.ResetAt.IsZero() {
				t.Errorf("ResetAt = %v, want zero (no Pacific-midnight lock)", rle.ResetAt)
			}
		})
	}

	// A truly opaque no-timestamp 429 (no resetAt, no period, no Retry-After
	// header) gets the bounded backoff — never the Pacific-midnight lock
	// (#140 P2) and never the load-shedding mislabel.
	err := classifyError(http.StatusTooManyRequests, `{"status":"rate_limited","message":"daily quota"}`, http.Header{})
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("plain 429 = %T %v, want *RateLimitError", err, err)
	}
	if rle.Status == "load_shedding" {
		t.Error("plain 429 misclassified as load_shedding")
	}
	if !rle.ResetAt.IsZero() {
		t.Errorf("plain 429 ResetAt = %v, want zero (bounded backoff, not midnight)", rle.ResetAt)
	}
	if rle.RetryAfter != opaqueRateLimitBackoff {
		t.Errorf("plain 429 RetryAfter = %s, want %s (bounded, not midnight)", rle.RetryAfter, opaqueRateLimitBackoff)
	}

	// A body that DOES signal a genuine daily reset — a pacific_day quota
	// period with the counter at/over the limit — still resolves to the
	// Pacific-midnight lock (#140 pins the daily-cap path).
	errDaily := classifyError(http.StatusTooManyRequests, `{"status":"rate_limited","period":"pacific_day","limit":6,"recentCount":6}`, http.Header{})
	var rleDaily *RateLimitError
	if !errors.As(errDaily, &rleDaily) {
		t.Fatalf("daily-cap 429 = %T %v, want *RateLimitError", errDaily, errDaily)
	}
	next := NextPacificMidnight()
	if rleDaily.ResetAt.IsZero() {
		t.Error("daily-cap 429 lost the Pacific-midnight lock")
	} else if d := rleDaily.ResetAt.Sub(next); d < -time.Second || d > time.Second {
		t.Errorf("daily-cap 429 ResetAt = %v, want near %v", rleDaily.ResetAt, next)
	}
	if rleDaily.RetryAfter <= 0 {
		t.Error("daily-cap 429 RetryAfter <= 0")
	}
}

// TestClassifyOpaqueRateLimitedBoundedBackoff pins #140 P2: a fully opaque
// rate_limited 429 body with empty headers (no timestamp, no period, no
// Retry-After) must yield a bounded RetryAfter — a minutes-scale transient
// is never locked to Pacific midnight.
func TestClassifyOpaqueRateLimitedBoundedBackoff(t *testing.T) {
	err := classifyError(http.StatusTooManyRequests, `{"error":{"code":"rate_limited"}}`, http.Header{})
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("classifyError = %T %v, want *RateLimitError", err, err)
	}
	if rle.RetryAfter <= 0 || rle.RetryAfter > 5*time.Minute {
		t.Errorf("RetryAfter = %s, want bounded >0 and <= 5m", rle.RetryAfter)
	}
	if rle.RetryAfter != opaqueRateLimitBackoff {
		t.Errorf("RetryAfter = %s, want %s", rle.RetryAfter, opaqueRateLimitBackoff)
	}
	if !rle.ResetAt.IsZero() {
		t.Errorf("ResetAt = %v, want zero (opaque body: no midnight lock)", rle.ResetAt)
	}
}
