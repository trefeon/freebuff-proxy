package upstream

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
)

// testConfig builds a config; baseURL "" keeps the default (only for tests
// that do not perform requests). All request-making tests pass mock.URL().
func testConfig(baseURL string, mut func(*config.Config)) *config.Config {
	cfg := &config.Config{
		ListenAddr:         ":3457",
		UpstreamBaseURL:    "https://www.codebuff.com",
		AuthTokens:         []string{"tok-a"},
		RotationInterval:   6 * time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 30 * time.Second,
		RegistryRefresh:    6 * time.Hour,
	}
	if baseURL != "" {
		cfg.UpstreamBaseURL = baseURL
	}
	if mut != nil {
		mut(cfg)
	}
	return cfg
}

// TestChatCompletionsStreamBodySurvives streams three chunks with real
// delays and asserts the whole body reads back. Regression: do() used to
// defer-cancel the request context when the response headers arrived, which
// aborted every streamed body read (observed live: "upstream stream error:
// context canceled" right after a successful upstream 200).
func TestChatCompletionsStreamBodySurvives(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	chunks := []string{
		`{"id":"c0","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"0"},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"1"},"finish_reason":null}]}`,
		`{"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"2"},"finish_reason":null}]}`,
	}
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk))
			flusher.Flush()
			time.Sleep(150 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("stream read failed (request context canceled too early?): %v", err)
	}
	text := string(data)
	for i, want := range []string{`"content":"0"`, `"content":"1"`, `"content":"2"`, "[DONE]"} {
		if !strings.Contains(text, want) {
			t.Errorf("stream missing %q (chunk %d): %s", want, i, text)
		}
	}
}

func TestChatCompletionsEnvelope(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`)

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{
		Model:             "deepseek/deepseek-v4-flash",
		RunID:             "run-abc",
		SessionInstanceID: "inst-1",
	}, body)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()

	headers, bodies := mock.RecordedChatHeaders, mock.RecordedChatBodies
	if len(headers) != 1 || len(bodies) != 1 {
		t.Fatalf("want 1 chat request, got %d / %d", len(headers), len(bodies))
	}
	h := headers[0]
	if got := h.Get("x-freebuff-model"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("x-freebuff-model = %q", got)
	}
	if got := h.Get("x-freebuff-instance-id"); got != "inst-1" {
		t.Errorf("x-freebuff-instance-id = %q", got)
	}
	if got := h.Get("Authorization"); got != "Bearer tok-a" {
		t.Errorf("Authorization = %q", got)
	}
	if got := h.Get("Accept"); got != "application/json, text/event-stream" {
		t.Errorf("Accept = %q", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
		t.Fatalf("recorded body not JSON: %v", err)
	}
	md, ok := sent["codebuff_metadata"].(map[string]any)
	if !ok {
		t.Fatalf("missing codebuff_metadata in %s", bodies[0])
	}
	if md["run_id"] != "run-abc" {
		t.Errorf("run_id = %v", md["run_id"])
	}
	if md["freebuff_instance_id"] != "inst-1" {
		t.Errorf("freebuff_instance_id = %v", md["freebuff_instance_id"])
	}
	clientID, _ := md["client_id"].(string)
	if !regexp.MustCompile(`^[0-9a-z]{13}$`).MatchString(clientID) {
		t.Errorf("client_id %q not 13-char base36", clientID)
	}
	provider, ok := sent["provider"].(map[string]any)
	if !ok || provider["data_collection"] != "deny" {
		t.Errorf("provider.data_collection not deny: %v", sent["provider"])
	}
	if sent["stream"] != true {
		t.Errorf("stream not forced: %v", sent["stream"])
	}
	stop, ok := sent["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "cb_easp" {
		t.Errorf("stop sentinel not injected: %v", sent["stop"])
	}
	if sent["temperature"] != 0.7 {
		t.Errorf("temperature lost in envelope: %v", sent["temperature"])
	}
	if sent["cost_mode"] != nil {
		// cost_mode lives inside codebuff_metadata only
		t.Errorf("cost_mode leaked to top level: %v", sent["cost_mode"])
	}
}

func TestEnvelopeCostModeAndStopPreserved(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	// cost_mode present
	withMode, err := New("tok", testConfig(mock.URL(), func(c *config.Config) { c.CostMode = "free" }))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := withMode.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r", SessionInstanceID: "i"}, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	var sent map[string]any
	_ = json.Unmarshal([]byte(mock.RecordedChatBodies[0]), &sent)
	md := sent["codebuff_metadata"].(map[string]any)
	if md["cost_mode"] != "free" {
		t.Errorf("cost_mode = %v, want free", md["cost_mode"])
	}

	// cost_mode absent
	noMode, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	rc, err = noMode.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r", SessionInstanceID: "i"}, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	_ = json.Unmarshal([]byte(mock.RecordedChatBodies[1]), &sent)
	md = sent["codebuff_metadata"].(map[string]any)
	if _, present := md["cost_mode"]; present {
		t.Errorf("cost_mode present despite empty config: %v", md)
	}

	// client-supplied stop is preserved
	rc, err = noMode.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r", SessionInstanceID: "i"},
		[]byte(`{"model":"m","stop":["my-stop"]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	_ = json.Unmarshal([]byte(mock.RecordedChatBodies[2]), &sent)
	stop := sent["stop"].([]any)
	if len(stop) != 1 || stop[0] != "my-stop" {
		t.Errorf("client stop overwritten: %v", stop)
	}
}

func TestUAIsCLIUserAgent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = rc.Close()
		if got := mock.RecordedChatHeaders[i].Get("User-Agent"); got != cliUserAgent {
			t.Errorf("request %d UA = %q, want the fixed CLI UA %q", i, got, cliUserAgent)
		}
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"run invalid", 400, `{"error":"runId not found"}`, ErrRunInvalid},
		{"run not running", 400, `{"error":"runId not running"}`, ErrRunInvalid},
		{"session superseded", 400, `{"error":"session_superseded"}`, ErrSessionInvalid},
		{"session expired", 400, `{"error":"session_expired"}`, ErrSessionInvalid},
		{"update required", 400, `{"error":"freebuff_update_required"}`, ErrSessionInvalid},
		{"auth", 401, `{"error":"unauthorized"}`, ErrAuthRejected},
		{"waiting room 503", 503, `{"error":"waiting_room_queued"}`, ErrWaitingRoom},
		{"waiting room body", 429, `{"error":"waiting_room_required"}`, ErrSessionInvalid},
		{"generic", 500, `{"error":"boom"}`, &UpstreamError{Status: 500}},
		{"402 out of credits", 402, `{"error":"out of credits"}`, &UpstreamError{Status: 402}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.ChatStatus = tc.status
			mock.ChatErrorBody = tc.body

			client, err := New("tok", testConfig(mock.URL(), nil))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
			if err == nil {
				t.Fatal("expected error")
			}
			if _, isUpstream := tc.want.(*UpstreamError); isUpstream {
				var upErr *UpstreamError
				if !errors.As(err, &upErr) {
					t.Fatalf("want UpstreamError, got %v", err)
				}
				if upErr.Status != tc.status {
					t.Fatalf("status = %d, want %d", upErr.Status, tc.status)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("errors.Is(%q) = false, want %v", err, tc.want)
			}
		})
	}
}

func TestTruncationOfLargeErrorBody(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = 500
	mock.ChatErrorBody = strings.Repeat("x", 2000)

	client, _ := New("tok", testConfig(mock.URL(), nil))
	_, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("want UpstreamError, got %v", err)
	}
	if len(upErr.Body) > 503 {
		t.Errorf("body not truncated: %d chars", len(upErr.Body))
	}
	if !strings.HasSuffix(upErr.Body, "...") {
		t.Errorf("truncation marker missing: %q", upErr.Body)
	}
}

func TestWaitingRoomRetryAfterHeader(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":"waiting_room_queued"}`)
	}

	client, _ := New("tok", testConfig(mock.URL(), nil))
	_, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
	var wrErr *WaitingRoomError
	if !errors.As(err, &wrErr) {
		t.Fatalf("want WaitingRoomError, got %v", err)
	}
	if wrErr.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %s, want 7s", wrErr.RetryAfter)
	}
	if !errors.Is(err, ErrWaitingRoom) {
		t.Error("not unwrap-able to ErrWaitingRoom")
	}
}

func TestSessionControlCalls(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, _ := New("tok", testConfig(mock.URL(), nil))

	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" || st.InstanceID != "inst-abc-123" {
		t.Fatalf("create state = %+v", st)
	}
	if st.ExpiresAt.IsZero() {
		t.Error("expiresAt not parsed")
	}

	// poll requires instance header
	polled, err := client.GetSession(context.Background(), "inst-abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if polled.Status != "active" {
		t.Errorf("poll status = %q", polled.Status)
	}

	// end + tolerated 404
	if err := client.EndSession(context.Background(), "inst-abc-123"); err != nil {
		t.Fatal(err)
	}
}

func TestGetSession404IsDisabled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "404"

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.GetSession(context.Background(), "inst-gone")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "disabled" {
		t.Errorf("status = %q, want disabled (404 mapping)", st.Status)
	}
}

func TestQueuedSessionParsing(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "queued"
	mock.QueuePosition = 4
	mock.QueueDepth = 9
	mock.EstimatedWaitMs = 0

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "queued" || st.Position != 4 || st.QueueDepth != 9 {
		t.Fatalf("queued state = %+v", st)
	}
	if st.PollAt.IsZero() {
		t.Error("pollAt not parsed")
	}
}

func TestStartAndFinishRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, _ := New("tok", testConfig(mock.URL(), nil))

	runID, err := client.StartRun(context.Background(), "base2-free-deepseek-flash")
	if err != nil {
		t.Fatal(err)
	}
	if runID != "run-0001" {
		t.Errorf("runID = %q", runID)
	}
	if len(mock.StartedRuns) != 1 || mock.StartedRuns[0] != "base2-free-deepseek-flash" {
		t.Errorf("START not recorded: %v", mock.StartedRuns)
	}

	if err := client.FinishRun(context.Background(), runID, 4); err != nil {
		t.Fatal(err)
	}
	if len(mock.FinishedRuns) != 1 {
		t.Fatalf("FINISH not recorded: %v", mock.FinishedRuns)
	}
	f := mock.FinishedRuns[0]
	if f.RunID != runID || f.Status != "completed" || f.TotalSteps != 4 {
		t.Errorf("FINISH payload = %+v", f)
	}
}

func TestControlCallTimeout(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Hang the session create; the 50ms control timeout must win.
	mock.SessionCreateDelay = 2 * time.Second

	client, _ := New("tok", testConfig(mock.URL(), func(c *config.Config) { c.SessionCallTimeout = 50 * time.Millisecond }))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := client.CreateSession(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestProxyWiring(t *testing.T) {
	cfg := testConfig("", func(c *config.Config) { c.HTTPProxy = "http://127.0.0.1:9999" })
	client, err := New("tok", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if client.http.Transport.(*http.Transport).Proxy == nil {
		t.Error("HTTP proxy not wired")
	}

	socksCfg := testConfig("", func(c *config.Config) { c.SOCKS5Proxy = "socks5://127.0.0.1:1080" })
	socksClient, err := New("tok", socksCfg)
	if err != nil {
		t.Fatal(err)
	}
	if socksClient.http.Transport.(*http.Transport).DialContext == nil {
		t.Error("SOCKS5 dialer not wired")
	}
}

func TestClientIDFormat(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := generateClientID()
		if !regexp.MustCompile(`^[0-9a-z]{13}$`).MatchString(id) {
			t.Fatalf("client_id %q not 13-char base36", id)
		}
	}
}

func TestNewTLSFingerprintInvalid(t *testing.T) {
	cfg := testConfig("", func(c *config.Config) { c.TLSFingerprint = "bogus" })
	_, err := New("tok", cfg)
	if err == nil {
		t.Fatal("New with bogus TLS_FINGERPRINT succeeded, want error")
	}
	if !strings.Contains(err.Error(), "TLS_FINGERPRINT") {
		t.Errorf("error = %q, want mention of TLS_FINGERPRINT", err)
	}
}

func TestAbortPropagation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBlocks = true

	client, _ := New("tok", testConfig(mock.URL(), nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		rc, err := client.ChatCompletions(ctx, ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		if err == nil {
			_ = rc.Close()
		}
		done <- err
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ChatCompletions error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ChatCompletions still blocked after cancel")
	}

	deadline := time.Now().Add(2 * time.Second)
	for !mock.AbortDetected.Load() {
		if time.Now().After(deadline) {
			t.Fatal("upstream request was not aborted on client cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
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

func TestWrapDecompress(t *testing.T) {
	const want = `{"status":"active","instanceId":"inst-abc-123"}`
	cases := []struct {
		name       string
		encoding   string
		compress   func([]byte) []byte
		wantErrSub string
	}{
		{"identity passthrough", "", nil, ""},
		{"gzip", "gzip", func(b []byte) []byte {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		{"deflate", "deflate", func(b []byte) []byte {
			var buf bytes.Buffer
			zw, _ := flate.NewWriter(&buf, flate.DefaultCompression)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		{"brotli", "br", func(b []byte) []byte {
			var buf bytes.Buffer
			zw := brotli.NewWriter(&buf)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		{"unsupported encoding", "zstd", nil, "unsupported Content-Encoding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(want)
			if tc.compress != nil {
				body = tc.compress([]byte(want))
			}
			resp := &http.Response{
				Header: http.Header{},
				Body:   io.NopCloser(bytes.NewReader(body)),
			}
			if tc.encoding != "" {
				resp.Header.Set("Content-Encoding", tc.encoding)
			}
			err := wrapDecompress(resp)
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("wrapDecompress err = %v, want %q", err, tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("wrapDecompress: %v", err)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			_ = resp.Body.Close()
			if string(got) != want {
				t.Errorf("body = %q, want %q", got, want)
			}
			if resp.Header.Get("Content-Encoding") != "" {
				t.Error("Content-Encoding header not stripped")
			}
		})
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
func TestCreateSessionForModelHeaders(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			model := r.Header.Get("x-freebuff-model")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","model":"`+model+`","expiresAt":"2030-01-01T00:00:00Z"}`)
			return
		}
		http.NotFound(w, r)
	}

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}

	st, err := client.CreateSessionForModel(context.Background(), "thudm/glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" || st.Model != "thudm/glm-5.2" || st.InstanceID != "inst-1" {
		t.Errorf("got %+v, want active with model thudm/glm-5.2", st)
	}
}

func TestGetSessionWithOptsHeaders(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var gotCompact, gotHeartbeat, gotInstance string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		gotCompact = r.Header.Get("x-freebuff-compact-session")
		gotHeartbeat = r.Header.Get("x-freebuff-heartbeat")
		gotInstance = r.Header.Get("x-freebuff-instance-id")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`)
	}

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}

	st, err := client.GetSessionWithOpts(context.Background(), "inst-1", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" {
		t.Errorf("status = %q, want active", st.Status)
	}
	if gotCompact != "1" || gotHeartbeat != "1" || gotInstance != "inst-1" {
		t.Errorf("headers: compact=%q, heartbeat=%q, instance=%q", gotCompact, gotHeartbeat, gotInstance)
	}
}

func TestSessionCallStructured4xx(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		wantStatus string
	}{
		{
			name:       "model_locked 409",
			statusCode: http.StatusConflict,
			body:       `{"status":"model_locked","currentModel":"deepseek/deepseek-v4-flash","requestedModel":"thudm/glm-5.2"}`,
			wantStatus: "model_locked",
		},
		{
			name:       "model_unavailable 409",
			statusCode: http.StatusConflict,
			body:       `{"status":"model_unavailable","requestedModel":"thudm/glm-5.2","availableHours":"08:00-20:00"}`,
			wantStatus: "model_unavailable",
		},
		{
			name:       "ip_capped 429",
			statusCode: http.StatusTooManyRequests,
			body:       `{"status":"ip_capped","activeUsersForIp":5,"limit":4,"retryAfterMs":30000}`,
			wantStatus: "ip_capped",
		},
		{
			name:       "spend_limited 429",
			statusCode: http.StatusTooManyRequests,
			body:       `{"status":"spend_limited","message":"Daily budget reached","retryAfterMs":60000}`,
			wantStatus: "spend_limited",
		},
		{
			name:       "country_blocked 403",
			statusCode: http.StatusForbidden,
			body:       `{"status":"country_blocked","countryCode":"CN","countryBlockReason":"country_not_allowed"}`,
			wantStatus: "country_blocked",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.statusCode)
				_, _ = io.WriteString(w, tc.body)
			}

			client, err := New("tok-a", testConfig(mock.URL(), nil))
			if err != nil {
				t.Fatal(err)
			}

			st, err := client.CreateSession(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if st.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", st.Status, tc.wantStatus)
			}
		})
	}
}
