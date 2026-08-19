package upstream

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	utls "github.com/refraction-networking/utls"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/stealth"
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
	// #106: the chat POST carries NO x-freebuff-model / x-freebuff-instance-id
	// headers — the model and instance id ride only in the body metadata.
	if got := h.Get("x-freebuff-model"); got != "" {
		t.Errorf("x-freebuff-model = %q on the chat POST, want absent (#106)", got)
	}
	if got := h.Get("x-freebuff-instance-id"); got != "" {
		t.Errorf("x-freebuff-instance-id = %q on the chat POST, want absent (#106)", got)
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
	// #103: client_id is a FRESH random draw per chat call — never the
	// sess:-prefixed shape the server fingerprints as a proxy.
	clientID, _ := md["client_id"].(string)
	if !regexp.MustCompile(`^[a-z0-9]{13}$`).MatchString(clientID) || strings.HasPrefix(clientID, "sess:") {
		t.Errorf("client_id = %q, want a fresh 13-char base36 draw per chat call (#103)", clientID)
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
		{"session superseded", 400, `{"error":"session_superseded"}`, ErrSessionSuperseded},
		{"session expired", 400, `{"error":"session_expired"}`, ErrSessionInvalid},
		{"update required", 400, `{"error":"freebuff_update_required"}`, ErrSessionInvalid},
		{"auth", 401, `{"error":"unauthorized"}`, ErrAuthRejected},
		{"waiting room 503", 503, `{"error":"waiting_room_queued"}`, ErrWaitingRoom},
		{"waiting room required (428)", 428, `{"error":"waiting_room_required"}`, ErrWaitingRoomRequired},
		{"waiting room required body (any status)", 429, `{"error":"waiting_room_required"}`, ErrWaitingRoomRequired},
		{"generic", 500, `{"error":"boom"}`, &UpstreamError{Status: 500}},
		{"402 out of credits", 402, `{"error":"out of credits"}`, ErrCredits},
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
	if err := client.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestProbeAccount verifies the zero-cost token probe: a GET
// /api/v1/freebuff/session with NO instance header that claims no session
// slot, returns the live per-model quota, and classifies
// auth/ban/region/transport failures through the standard matrix.
func TestProbeAccount(t *testing.T) {
	t.Run("200 with quota", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.AccessTier = "full"

		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		st, err := client.ProbeAccount(context.Background())
		if err != nil {
			t.Fatalf("ProbeAccount: %v", err)
		}
		if st.Status != "active" || st.InstanceID != "inst-abc-123" {
			t.Fatalf("probe state = %+v", st)
		}
		if st.AccessTier != "full" {
			t.Errorf("probe AccessTier = %q, want full (captured from response JSON)", st.AccessTier)
		}
		q, ok := st.RateLimitsByModel["deepseek/deepseek-v4-flash"]
		if !ok {
			t.Fatalf("RateLimitsByModel missing flash quota: %+v", st.RateLimitsByModel)
		}
		if q.Limit != 6 || q.RecentCount != 2 {
			t.Errorf("quota limit/recentCount = %v/%v, want 6/2", q.Limit, q.RecentCount)
		}
		if q.Period != "pacific_day" {
			t.Errorf("period = %q, want pacific_day", q.Period)
		}
		if q.ResetAt.IsZero() {
			t.Error("resetAt not parsed")
		}
		// A probe must not claim a session slot (no POST).
		if got := mock.SessionCreatesSnapshot(); got != 0 {
			t.Errorf("session creates = %d, want 0 (probe is zero-cost)", got)
		}
		if got := mock.SessionProbesSnapshot(); got != 1 {
			t.Errorf("session probes = %d, want 1", got)
		}
	})

	t.Run("404 maps to ErrNoActiveSession", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":"session not found"}`)
		}

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrNoActiveSession) {
			t.Fatalf("err = %v, want ErrNoActiveSession", err)
		}
	})

	t.Run("200 ended maps to ErrNoActiveSession", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"status":"ended"}`)
		}

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrNoActiveSession) {
			t.Fatalf("err = %v, want ErrNoActiveSession", err)
		}
	})

	t.Run("401 auth rejected", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.AuthReject = true

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrAuthRejected) {
			t.Fatalf("err = %v, want ErrAuthRejected", err)
		}
	})

	t.Run("403 banned", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.Ban = true

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrBanned) {
			t.Fatalf("err = %v, want ErrBanned", err)
		}
	})

	t.Run("403 country blocked", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"status":"country_blocked","countryCode":"CN","countryBlockReason":"region_restricted","ipPrivacySignals":["vpn"]}`)
		}

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrCountryBlocked) {
			t.Fatalf("err = %v, want ErrCountryBlocked", err)
		}
		var cbe *CountryBlockedError
		if !errors.As(err, &cbe) {
			t.Fatalf("err = %T, want *CountryBlockedError", err)
		}
		if cbe.CountryCode != "CN" {
			t.Errorf("countryCode = %q, want CN", cbe.CountryCode)
		}
	})

	t.Run("transport error", func(t *testing.T) {
		mock := testutil.NewMock()
		url := mock.URL()
		mock.Close()

		client, _ := New("tok", testConfig(url, nil))
		_, err := client.ProbeAccount(context.Background())
		if err == nil {
			t.Fatal("ProbeAccount returned nil error for closed server")
		}
	})
}

// TestSessionCallParsesRateLimitsByModel verifies the live per-model quota
// map from an admission response is parsed into SessionState, including the
// nested entitlement breakdown and flex-time resetAt.
func TestSessionCallParsesRateLimitsByModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		"z-ai/glm-5.2": map[string]any{
			"model":       "z-ai/glm-5.2",
			"limit":       5,
			"recentCount": 4,
			"period":      "pacific_day",
			"resetAt":     "2026-08-16T07:00:00.000Z",
			"entitlementBreakdown": map[string]any{
				"base":     1,
				"referral": 1,
				"streak":   3,
			},
		},
	}

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	q, ok := st.RateLimitsByModel["z-ai/glm-5.2"]
	if !ok {
		t.Fatalf("RateLimitsByModel missing model z-ai/glm-5.2: %+v", st.RateLimitsByModel)
	}
	if q.Limit != 5 || q.RecentCount != 4 {
		t.Errorf("quota limit/recentCount = %v/%v, want 5/4", q.Limit, q.RecentCount)
	}
	if q.Period != "pacific_day" {
		t.Errorf("period = %q, want pacific_day", q.Period)
	}
	if q.ResetAt.IsZero() {
		t.Error("resetAt not parsed")
	} else if want := "2026-08-16T07:00:00Z"; q.ResetAt.UTC().Format(time.RFC3339) != want {
		t.Errorf("resetAt = %s, want %s", q.ResetAt.UTC().Format(time.RFC3339), want)
	}
	if q.Entitlement["base"] != 1 || q.Entitlement["referral"] != 1 || q.Entitlement["streak"] != 3 {
		t.Errorf("entitlement = %+v, want base=1 referral=1 streak=3", q.Entitlement)
	}
	if q.Model != "z-ai/glm-5.2" {
		t.Errorf("quota model = %q", q.Model)
	}
}

// TestSessionCallParsesLimitedModelOffers verifies the limited-tier per-model
// allowances from an admission response are parsed into SessionState,
// including flex-time userResetAt.
func TestSessionCallParsesLimitedModelOffers(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","accessTier":"limited","limitedModelOffers":[{"model":"deepseek/deepseek-v4-flash","remaining":3,"total":5,"userRemaining":3,"userResetAt":"2026-08-16T07:00:00.000Z"}]}`)
	}

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.LimitedModelOffers) != 1 {
		t.Fatalf("LimitedModelOffers len = %d, want 1: %+v", len(st.LimitedModelOffers), st.LimitedModelOffers)
	}
	offer := st.LimitedModelOffers[0]
	if offer.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("model = %q", offer.Model)
	}
	if offer.Remaining != 3 || offer.Total != 5 || offer.UserRemaining != 3 {
		t.Errorf("offer = %+v, want remaining=3 total=5 userRemaining=3", offer)
	}
	wantReset := time.Date(2026, 8, 16, 7, 0, 0, 0, time.UTC)
	if !offer.UserResetAt.Equal(wantReset) {
		t.Errorf("UserResetAt = %v, want %v", offer.UserResetAt, wantReset)
	}
}

// TestSessionCallIgnoresMissingLimitedModelOffers verifies a full-tier or
// compact admission without limitedModelOffers parses cleanly (nil slice).
func TestSessionCallIgnoresMissingLimitedModelOffers(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.LimitedModelOffers != nil {
		t.Errorf("LimitedModelOffers = %+v, want nil when absent", st.LimitedModelOffers)
	}
}

func TestSession404Mapping(t *testing.T) {
	// A create 404 means no session slot exists upstream → disabled.
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "404"

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "disabled" {
		t.Errorf("create 404 status = %q, want disabled", st.Status)
	}

	// A poll 404 means the session vanished upstream (expired/evicted) →
	// ended (recreate path), NOT a permanent disabled (which the session
	// manager would cache with no expiry, disabling the token forever).
	polled, err := client.GetSession(context.Background(), "inst-gone")
	if err != nil {
		t.Fatal(err)
	}
	if polled.Status != "ended" {
		t.Errorf("poll 404 status = %q, want ended", polled.Status)
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

	msg1 := "msg-1"
	steps := []RunStep{
		{ID: "step-1", StepNumber: 1, MessageID: &msg1, Status: "completed", StartTime: "2026-08-18T00:00:00.000Z"},
		{ID: "step-2", StepNumber: 2, Status: "completed", StartTime: "2026-08-18T00:00:01.000Z"},
	}
	if err := client.FinishRun(context.Background(), runID, "completed", len(steps), steps, ""); err != nil {
		t.Fatal(err)
	}
	if len(mock.FinishedRuns) != 1 {
		t.Fatalf("FINISH not recorded: %v", mock.FinishedRuns)
	}
	f := mock.FinishedRuns[0]
	if f.RunID != runID || f.Status != "completed" || f.TotalSteps != 2 {
		t.Errorf("FINISH payload = %+v", f)
	}
	// Issue #114: steps ride IN the FINISH payload (the CLI has no /steps
	// endpoint) with the CLI step shape: id, stepNumber, messageId
	// (null-able), status, startTime.
	if len(f.Steps) != 2 || f.Steps[0].StepNumber != 1 || f.Steps[0].MessageID == nil || *f.Steps[0].MessageID != "msg-1" ||
		f.Steps[1].StepNumber != 2 || f.Steps[1].MessageID != nil || f.Steps[1].StartTime == "" {
		t.Errorf("FINISH steps = %+v, want 2 CLI-shaped steps", f.Steps)
	}
}

func TestFinishRunErrorTruncation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, _ := New("tok", testConfig(mock.URL(), nil))

	// errorMessage must be truncated to 5000 runes (CLI parity:
	// truncateString(errorMessage, 5000) in database.ts) — a full Go stack
	// trace must not blow the cap.
	long := strings.Repeat("エ", 6000)
	if err := client.FinishRun(context.Background(), "run-0001", "failed", 0, nil, long); err != nil {
		t.Fatal(err)
	}
	finished := mock.FinishedRunsSnapshot()
	if len(finished) != 1 || finished[0].RunID != "run-0001" || finished[0].Status != "failed" {
		t.Fatalf("finished runs = %+v, want run-0001 failed", finished)
	}
	if got := len([]rune(finished[0].ErrorMessage)); got != 5000 {
		t.Errorf("errorMessage runes = %d, want 5000 (truncated)", got)
	}
}

func TestControlCallTimeout(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Hang the session create; the 50ms control timeout must win even when
	// the caller passes a much longer deadline (the control timeout is the
	// tighter bound and must never be defeated by the caller's context).
	mock.SessionCreateDelay = 10 * time.Second

	client, _ := New("tok", testConfig(mock.URL(), func(c *config.Config) { c.SessionCallTimeout = 50 * time.Millisecond }))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := client.CreateSession(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

// TestCrossHostRedirectStripsToken verifies a cross-host redirect does not
// carry x-codebuff-api-key (or Authorization): Go strips the latter itself
// but not the former, so the raw token used to leak to any redirect target.
// Same-host redirects keep their credentials (CDN / bare-host -> www).
func TestCrossHostRedirectStripsToken(t *testing.T) {
	const token = "tok-secret-redirect"

	t.Run("scheme downgrade strips token", func(t *testing.T) {
		// https -> http to the SAME host (plaintext) must drop both
		// credential headers; the token must never cross onto a cleartext
		// hop, even when the hostname is unchanged.
		check := func(t *testing.T, from, to string, wantStripped bool) {
			t.Helper()
			client, err := New(token, testConfig("http://127.0.0.1:1", nil))
			if err != nil {
				t.Fatal(err)
			}
			via := []*http.Request{{
				URL:    mustParseURL(t, from),
				Header: http.Header{},
			}}
			via[0].Header.Set("x-codebuff-api-key", token)
			via[0].Header.Set("Authorization", "Bearer "+token)
			req := &http.Request{URL: mustParseURL(t, to), Header: via[0].Header.Clone()}
			if err := client.http.CheckRedirect(req, via); err != nil {
				t.Fatalf("CheckRedirect: %v", err)
			}
			gotKey := req.Header.Get("x-codebuff-api-key")
			gotAuth := req.Header.Get("Authorization")
			if wantStripped {
				if gotKey != "" || gotAuth != "" {
					t.Errorf("redirect %s -> %s kept credentials (key %q auth %q), want stripped", from, to, gotKey, gotAuth)
				}
			} else {
				if gotKey != token || gotAuth != "Bearer "+token {
					t.Errorf("redirect %s -> %s stripped credentials (key %q auth %q), want kept", from, to, gotKey, gotAuth)
				}
			}
		}
		check(t, "https://www.codebuff.com", "http://www.codebuff.com", true)
		check(t, "https://www.codebuff.com", "http://www.codebuff.com:8080", true)
		check(t, "https://www.codebuff.com", "https://www.codebuff.com", false)
		check(t, "http://www.codebuff.com", "https://www.codebuff.com", false)
	})

	keySeen := make(chan string, 1)
	authSeen := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keySeen <- r.Header.Get("x-codebuff-api-key")
		authSeen <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/final", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := New(token, testConfig(origin.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	req, err := client.newRequest(context.Background(), http.MethodGet, "/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := <-keySeen; got != "" {
		t.Errorf("cross-host redirect carried x-codebuff-api-key %q, want stripped", got)
	}
	if got := <-authSeen; got != "" {
		t.Errorf("cross-host redirect carried Authorization %q, want stripped", got)
	}

	sameKey := make(chan string, 1)
	same := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
			return
		}
		sameKey <- r.Header.Get("x-codebuff-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer same.Close()

	sameClient, err := New(token, testConfig(same.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	sameReq, err := sameClient.newRequest(context.Background(), http.MethodGet, "/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	sameResp, err := sameClient.http.Do(sameReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = sameResp.Body.Close()
	if got := <-sameKey; got != "" {
		t.Errorf("same-host request carried x-codebuff-api-key %q, want absent (client no longer sends it, issue #107)", got)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

func TestClientIDFormat(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := generateClientID()
		if !regexp.MustCompile(`^[0-9a-z]{13}$`).MatchString(id) {
			t.Fatalf("client_id %q not 13-char base36", id)
		}
	}
}

// TestGenerateClientIDFallbackPads verifies the time-seeded fallback never
// panics on a short base36 value: UnixNano in base36 is 12 digits today, and
// the old [:13] slice on it panicked whenever crypto/rand failed. The shared
// padBase36 helper must always yield the SDK's 13-char id.
func TestGenerateClientIDFallbackPads(t *testing.T) {
	for i := 0; i < 10; i++ {
		fallback := padBase36(strconv.FormatInt(time.Now().UnixNano(), 36))
		if !regexp.MustCompile(`^[0-9a-z]{13}$`).MatchString(fallback) {
			t.Fatalf("time fallback client_id %q not 13-char base36", fallback)
		}
	}
	if got := padBase36("abc"); got != "0000000000abc" {
		t.Errorf("padBase36(abc) = %q, want 0000000000abc (13 chars)", got)
	}
	if got := padBase36("0123456789abc"); got != "0123456789abc" {
		t.Errorf("padBase36(13-char) = %q, want unchanged", got)
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

	// Generic 429 without explicit timestamp auto-detects upcoming Pacific midnight
	errGeneric := classifyError(429, `{"status":"rate_limited"}`, http.Header{})
	var rleGeneric *RateLimitError
	if errors.As(errGeneric, &rleGeneric) {
		if rleGeneric.ResetAt.IsZero() {
			t.Errorf("expected auto-detected ResetAt, got zero")
		}
		if !rleGeneric.ResetAt.After(time.Now()) {
			t.Errorf("expected ResetAt to be in the future, got %v", rleGeneric.ResetAt)
		}
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
		// RFC 9110 §8.4.1.3: deflate = zlib-wrapped (RFC 1950). A conforming
		// server's body must decode; the raw-flate fallback must not break
		// the existing raw case above. (Audit B1.)
		{"deflate zlib-wrapped", "deflate", func(b []byte) []byte {
			var buf bytes.Buffer
			zw := zlib.NewWriter(&buf)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		{"corrupt gzip", "gzip", func(b []byte) []byte {
			return []byte("this is not gzip data")
		}, "gzip:"},
		// Multi-value Content-Encoding is rejected with a clear error, not
		// silently mis-decoded. (Audit G1.)
		{"multi-value encoding rejected", "gzip, br", nil, "unsupported Content-Encoding"},
		{"brotli", "br", func(b []byte) []byte {
			var buf bytes.Buffer
			zw := brotli.NewWriter(&buf)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		{"zstd", "zstd", func(b []byte) []byte {
			var buf bytes.Buffer
			zw, _ := zstd.NewWriter(&buf)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		{"unsupported encoding", "lz4", nil, "unsupported Content-Encoding"},
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

// TestStealthProfileResolvedOncePerRequest verifies that for TLS_FINGERPRINT
// auto/random the concrete profile is resolved ONCE per request: newRequest
// stashes it, and the dialer reads the same stash for the ClientHello — so
// the TLS fingerprint always matches the resolved profile. The request
// carries the CLI UA and NO browser headers (#109): header application is
// inverted — only the utls ClientHello impersonates the browser.
func TestStealthProfileResolvedOncePerRequest(t *testing.T) {
	client, err := New("tok-a", testConfig("", func(c *config.Config) { c.TLSFingerprint = "auto" }))
	if err != nil {
		t.Fatal(err)
	}
	req, err := client.newRequest(context.Background(), http.MethodGet, "/api/v1/freebuff/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	stashed := stealthProfileFrom(req.Context())
	if stashed == nil {
		t.Fatal("no concrete profile stashed in the request context")
	}
	if stashed.ID == stealth.ProfileIDAuto || stashed.ID == stealth.ProfileIDRandom {
		t.Fatalf("stashed profile %s is not concrete (auto must resolve once)", stashed.ID)
	}
	// The request carries the pinned CLI UA, not the profile's browser UA.
	if got := req.Header.Get("User-Agent"); got != cliUserAgent {
		t.Errorf("request User-Agent %q != the CLI UA %q (no browser persona on API calls, #109)", got, cliUserAgent)
	}
	for _, hdr := range []string{"Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest",
		"Sec-CH-UA", "Sec-CH-UA-Mobile", "Sec-CH-UA-Platform"} {
		if got := req.Header.Get(hdr); got != "" {
			t.Errorf("%s = %q on an upstream API request, want absent (#109)", hdr, got)
		}
	}
	// The dialer must use the stashed profile for this request's dial.
	if dial := client.dialProfileFor(req.Context()); dial != stashed {
		t.Errorf("dialProfileFor(request ctx) = %p (%s), want the stashed profile %p", dial, dial.ID, stashed)
	}
	// A bare context (no stash) falls back to the unresolved profile; the
	// dialer resolves it per connection (pre-fix behavior for dials that
	// never went through newRequest).
	if dial := client.dialProfileFor(context.Background()); dial != stealth.ProfileAuto {
		t.Errorf("dialProfileFor(bare ctx) = %v, want ProfileAuto (dialer resolves per connection)", dial)
	}
	// Pinned profiles keep working unchanged.
	pinned, err := New("tok-a", testConfig("", func(c *config.Config) { c.TLSFingerprint = "chrome126" }))
	if err != nil {
		t.Fatal(err)
	}
	if dial := pinned.dialProfileFor(context.Background()); dial != stealth.ProfileChrome126 {
		t.Errorf("pinned dialProfileFor = %s, want chrome126", dial.ID)
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

	st, err := client.GetSessionWithOpts(context.Background(), "inst-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" {
		t.Errorf("status = %q, want active", st.Status)
	}
	if gotCompact != "1" || gotInstance != "inst-1" {
		t.Errorf("headers: compact=%q, instance=%q (want 1 / inst-1)", gotCompact, gotInstance)
	}
	// Gap #2: the CLI never beats — x-freebuff-heartbeat is Desktop-only
	// (reference/freebuff freebuff-models.ts:1212-1215), so a compact poll
	// must NOT carry it.
	if gotHeartbeat != "" {
		t.Errorf("x-freebuff-heartbeat = %q, want absent on compact polls", gotHeartbeat)
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

// TestDumpRedactsTokenHeaders verifies the debug dump redacts the
// Authorization header (the only credential on the wire since #107 dropped
// x-codebuff-api-key; the redaction list still covers it defensively).
// Regression: dump() only redacted Authorization, so DEBUG_DUMP=true leaked
// the plaintext token into dump/ files via x-codebuff-api-key.
func TestDumpRedactsTokenHeaders(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusUnauthorized
	mock.ChatErrorBody = `{"error":"unauthorized"}`

	client, err := New("tok-secret-1234", testConfig(mock.URL(), func(c *config.Config) {
		c.DebugDump = true
	}))
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, body); err == nil {
		t.Fatal("expected error from 401 response")
	}

	entries, err := filepath.Glob(filepath.Join("dump", "*.dump"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no dump file written")
	}
	data, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	dump := string(data)
	if strings.Contains(dump, "tok-secret-1234") {
		t.Fatalf("dump file leaks token:\n%s", dump)
	}
	if !strings.Contains(dump, "Authorization: [redacted]") {
		t.Errorf("dump file missing redacted Authorization header:\n%s", dump)
	}
	// #107: x-codebuff-api-key is no longer sent, so it must not appear in
	// the dump at all (the defensive redaction list stays for any future
	// setter).
	if strings.Contains(strings.ToLower(dump), "x-codebuff-api-key") {
		t.Errorf("dump file contains an x-codebuff-api-key header line (never sent now):\n%s", dump)
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

func TestNextStealthProfile(t *testing.T) {
	// Deterministic rotation across DISTINCT ClientHelloIDs.
	cases := []struct {
		cur  *stealth.Profile
		want *stealth.Profile
	}{
		{stealth.ProfileChrome120, stealth.ProfileSafari18},
		{stealth.ProfileChrome126, stealth.ProfileSafari18},
		{stealth.ProfileEdge126, stealth.ProfileSafari18},
		{stealth.ProfileSafari17, stealth.ProfileFirefox128},
		{stealth.ProfileSafari18, stealth.ProfileFirefox128},
		{stealth.ProfileFirefox120, stealth.ProfileChrome126},
		{stealth.ProfileFirefox128, stealth.ProfileChrome126},
	}
	for _, tc := range cases {
		if got := nextStealthProfile(tc.cur); got != tc.want {
			t.Errorf("nextStealthProfile(%s) = %s, want %s", tc.cur.ID, got.ID, tc.want.ID)
		}
	}
}

// TestNextStealthProfileUnknownFallback guards the unknown-profile fallback
// (G11): a profile outside the rotation order rotates to the first entry.
func TestNextStealthProfileUnknownFallback(t *testing.T) {
	got := nextStealthProfile(&stealth.Profile{ID: "bogus"})
	if want := retryProfileRotation[0].next; got != want {
		t.Errorf("nextStealthProfile(unknown) = %s, want %s", got.ID, want.ID)
	}
}

// TestWrapDecompressZstdDecoderClosed guards Audit B9 (fix 6): closing a
// zstd-wrapped response body must release the per-response decoder, not just
// the underlying socket (decoder buffers would otherwise linger until GC).
func TestWrapDecompressZstdDecoderClosed(t *testing.T) {
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"status":"active"}`))
	_ = zw.Close()

	resp := &http.Response{
		Header: http.Header{"Content-Encoding": []string{"zstd"}},
		Body:   io.NopCloser(bytes.NewReader(buf.Bytes())),
	}
	if err := wrapDecompress(resp); err != nil {
		t.Fatal(err)
	}
	dc, ok := resp.Body.(*decompressCloser)
	if !ok {
		t.Fatalf("body = %T, want *decompressCloser", resp.Body)
	}
	if dc.closeFn == nil {
		t.Error("zstd decompressCloser has no closeFn: decoder resources leak until GC")
	}
	if _, err := io.ReadAll(dc); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := dc.Close(); err != nil {
		t.Fatalf("close: %v", err)
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

// TestEnsureCliSystemMarkerBranches covers the system-marker merge matrix
// (G5): empty messages, already-present marker, non-string content, merge
// into the first system message, and the unshift path.
func TestEnsureCliSystemMarkerBranches(t *testing.T) {
	t.Run("missing messages gets marker-only system", func(t *testing.T) {
		p := map[string]any{}
		ensureCliSystemMarker(p)
		msgs, ok := p["messages"].([]any)
		if !ok || len(msgs) != 1 {
			t.Fatalf("messages = %v, want a single system message", p["messages"])
		}
		sys, ok := msgs[0].(map[string]any)
		if !ok || sys["role"] != "system" || sys["content"] != cliSystemMarker {
			t.Errorf("system message = %v, want role=system with the CLI marker", msgs[0])
		}
	})

	t.Run("empty messages gets marker-only system", func(t *testing.T) {
		p := map[string]any{"messages": []any{}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("messages = %v, want a single system message", msgs)
		}
		if msgs[0].(map[string]any)["content"] != cliSystemMarker {
			t.Errorf("system content = %v", msgs[0])
		}
	})

	t.Run("marker already present in string is untouched", func(t *testing.T) {
		content := cliSystemMarker + "\n\nextra instructions"
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": content},
			map[string]any{"role": "user", "content": "hi"},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want unchanged length", msgs)
		}
		if got := msgs[0].(map[string]any)["content"]; got != content {
			t.Errorf("system content changed: %v", got)
		}
	})

	t.Run("marker already present in structured parts is untouched", func(t *testing.T) {
		parts := []any{
			map[string]any{"type": "text", "text": cliSystemMarker + " customized"},
		}
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": parts},
			map[string]any{"role": "user", "content": "hi"},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want unchanged length", msgs)
		}
		gotParts, ok := msgs[0].(map[string]any)["content"].([]any)
		if !ok || len(gotParts) != 1 {
			t.Fatalf("structured parts modified: %v", gotParts)
		}
		if gotParts[0].(map[string]any)["text"] != cliSystemMarker+" customized" {
			t.Errorf("structured text modified: %v", gotParts[0])
		}
	})

	t.Run("phrase mid-string in string prepends marker", func(t *testing.T) {
		// #110: the server gate is a TRIMMED PREFIX test at position 0 — a
		// system message that merely mentions the phrase mid-string must NOT
		// suppress the canonical prefix.
		content := "Please act as " + cliSystemMarkerPhrase + " and be concise."
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": content},
			map[string]any{"role": "user", "content": "hi"},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want length 2", msgs)
		}
		got := msgs[0].(map[string]any)["content"].(string)
		if !strings.HasPrefix(got, cliSystemMarker+"\n\n") || !strings.Contains(got, content) {
			t.Errorf("system content = %q, want marker prepended to the mid-string mention", got)
		}
	})

	t.Run("phrase mid-string in structured part prepends marker", func(t *testing.T) {
		parts := []any{
			map[string]any{"type": "text", "text": "Remember: " + cliSystemMarkerPhrase + "."},
		}
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": parts},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		gotParts, ok := msgs[0].(map[string]any)["content"].([]any)
		if !ok || len(gotParts) != 2 {
			t.Fatalf("system parts = %v, want 2 with marker prepended", msgs[0])
		}
		if gotParts[0].(map[string]any)["text"] != cliSystemMarker {
			t.Errorf("marker part = %v, want the CLI marker first", gotParts[0])
		}
	})

	t.Run("structured system content array prepends marker", func(t *testing.T) {
		originalParts := []any{
			map[string]any{"type": "text", "text": "custom instructions"},
			map[string]any{"type": "text", "text": "more instructions"},
		}
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": originalParts},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		parts, ok := msgs[0].(map[string]any)["content"].([]any)
		if !ok || len(parts) != 3 {
			t.Fatalf("system parts = %v, want 3 parts with marker prepended", msgs[0])
		}
		markerPart, ok := parts[0].(map[string]any)
		if !ok || markerPart["type"] != "text" || markerPart["text"] != cliSystemMarker {
			t.Errorf("marker part = %v, want text type with CLI marker", parts[0])
		}
		if parts[1].(map[string]any)["text"] != "custom instructions" || parts[2].(map[string]any)["text"] != "more instructions" {
			t.Errorf("original parts lost: %v", parts)
		}
	})

	t.Run("non-string non-array system content replaced", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": 12345},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if got := msgs[0].(map[string]any)["content"]; got != cliSystemMarker {
			t.Errorf("system content = %v, want the CLI marker", got)
		}
	})

	t.Run("empty string system content replaced with marker", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": ""},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if got := msgs[0].(map[string]any)["content"]; got != cliSystemMarker {
			t.Errorf("system content = %v, want the CLI marker", got)
		}
	})

	t.Run("merges into first system message", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "u"},
			map[string]any{"role": "system", "content": "existing"},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want length 2", msgs)
		}
		sys := msgs[1].(map[string]any)
		if sys["role"] != "system" {
			t.Fatalf("second message = %v, want system", sys)
		}
		if got := sys["content"].(string); !strings.HasPrefix(got, cliSystemMarker) || !strings.Contains(got, "existing") {
			t.Errorf("merged content = %q, want marker + existing", got)
		}
	})

	t.Run("unshifts marker before user", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "u"},
		}}
		ensureCliSystemMarker(p)
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want length 2", msgs)
		}
		if msgs[0].(map[string]any)["role"] != "system" {
			t.Errorf("first message = %v, want system", msgs[0])
		}
	})
}

// TestInjectEnvelopeBranchMatrix covers injectEnvelope's override behavior
// (G5): stream:false is force-overridden, provider is replaced, stop is
// preserved, and a non-object body is rejected.
func TestInjectEnvelopeBranchMatrix(t *testing.T) {
	t.Run("stream false overridden to true", func(t *testing.T) {
		out, err := injectEnvelope([]byte(`{"model":"m","stream":false}`), "free", ChatOptions{RunID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["stream"] != true {
			t.Errorf("stream = %v, want true", payload["stream"])
		}
	})

	t.Run("provider replaced", func(t *testing.T) {
		out, err := injectEnvelope([]byte(`{"model":"m","provider":{"data_collection":"allow"}}`), "free", ChatOptions{RunID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatal(err)
		}
		prov, ok := payload["provider"].(map[string]any)
		if !ok || prov["data_collection"] != "deny" {
			t.Errorf("provider = %v, want data_collection=deny", payload["provider"])
		}
	})

	t.Run("client stop preserved", func(t *testing.T) {
		out, err := injectEnvelope([]byte(`{"model":"m","stop":["custom"]}`), "free", ChatOptions{RunID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatal(err)
		}
		stop, ok := payload["stop"].([]any)
		if !ok || len(stop) != 1 || stop[0] != "custom" {
			t.Errorf("stop = %v, want preserved [custom]", payload["stop"])
		}
	})

	t.Run("no stop adds cb_easp", func(t *testing.T) {
		out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatal(err)
		}
		stop, ok := payload["stop"].([]any)
		if !ok || len(stop) != 1 || stop[0] != "cb_easp" {
			t.Errorf("stop = %v, want [cb_easp]", payload["stop"])
		}
	})

	t.Run("non-object body rejected", func(t *testing.T) {
		if _, err := injectEnvelope([]byte(`[1,2,3]`), "free", ChatOptions{RunID: "r"}); err == nil {
			t.Error("injectEnvelope accepted a JSON array body")
		}
	})
}

// TestRequestJitter guards the REQUEST_JITTER gate (G6): the request is held
// before any upstream contact, and canceling during the window aborts with
// context.Canceled and no upstream hit.
func TestRequestJitter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := New("tok", testConfig(mock.URL(), func(c *config.Config) {
		c.RequestJitter = time.Hour
	}))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.ChatCompletions(ctx, ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		done <- err
	}()

	// The jitter gate must hold the request before any upstream contact.
	time.Sleep(50 * time.Millisecond)
	if n := mock.Requests; n != 0 {
		t.Fatalf("upstream hit %d times during the jitter window, want 0", n)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ChatCompletions did not abort on cancel during jitter")
	}
	if n := mock.Requests; n != 0 {
		t.Fatalf("upstream hit %d times after cancel, want 0", n)
	}

	t.Run("small jitter still completes", func(t *testing.T) {
		mock2 := testutil.NewMock()
		defer mock2.Close()
		mock2.ChatBody = testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`)
		client2, err := New("tok", testConfig(mock2.URL(), func(c *config.Config) {
			c.RequestJitter = 30 * time.Millisecond
		}))
		if err != nil {
			t.Fatal(err)
		}
		rc, err := client2.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatalf("chat with jitter failed: %v", err)
		}
		_ = rc.Close()
		if mock2.Requests != 1 {
			t.Errorf("Requests = %d, want 1", mock2.Requests)
		}
	})
}

// TestRedirectMultihop guards multi-hop redirect token semantics (G7): an
// A→B→A loop keeps the token at the origin (B never sees it, A receives its
// own token on the loop-back hop), the 3-hop limit errors out, and a
// port-differing same-host hop is treated as cross-host (token stripped).
func TestRedirectMultihop(t *testing.T) {
	const token = "tok-multihop"

	t.Run("A-B-A loop", func(t *testing.T) {
		bKeySeen := make(chan string, 1)
		aKeySeen := make(chan string, 2)

		var targetBURL string
		originA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/start":
				http.Redirect(w, r, targetBURL+"/b", http.StatusTemporaryRedirect)
			default:
				aKeySeen <- r.Header.Get("x-codebuff-api-key")
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer originA.Close()

		targetB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bKeySeen <- r.Header.Get("x-codebuff-api-key")
			// Loop back to the ORIGIN: Go re-copies headers from via[0]
			// (the origin), so the origin must receive its own token again.
			http.Redirect(w, r, originA.URL+"/loop", http.StatusTemporaryRedirect)
		}))
		defer targetB.Close()
		targetBURL = targetB.URL

		client, err := New(token, testConfig(originA.URL, nil))
		if err != nil {
			t.Fatal(err)
		}
		req, err := client.newRequest(context.Background(), http.MethodGet, "/start", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()

		if got := <-bKeySeen; got != "" {
			t.Errorf("intermediate host B received token %q, want stripped", got)
		}
		if got := <-aKeySeen; got != "" {
			t.Errorf("loop-back hop to A carried %q, want absent (client no longer sends x-codebuff-api-key, issue #107)", got)
		}
	})

	t.Run("three-hop limit", func(t *testing.T) {
		targetC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
		}))
		defer targetC.Close()
		targetB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, targetC.URL+"/c", http.StatusTemporaryRedirect)
		}))
		defer targetB.Close()
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, targetB.URL+"/b", http.StatusTemporaryRedirect)
		}))
		defer origin.Close()

		client, err := New(token, testConfig(origin.URL, nil))
		if err != nil {
			t.Fatal(err)
		}
		req, err := client.newRequest(context.Background(), http.MethodGet, "/start", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.http.Do(req)
		if err == nil {
			t.Fatal("3-redirect chain succeeded, want too-many-redirects error")
		}
		if !strings.Contains(err.Error(), "too many redirects") {
			t.Errorf("err = %v, want too many redirects", err)
		}
	})

	t.Run("port-differing same-host strips token", func(t *testing.T) {
		keySeen := make(chan string, 1)
		otherPort := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			keySeen <- r.Header.Get("x-codebuff-api-key")
			w.WriteHeader(http.StatusOK)
		}))
		defer otherPort.Close()
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, otherPort.URL+"/final", http.StatusTemporaryRedirect)
		}))
		defer origin.Close()

		client, err := New(token, testConfig(origin.URL, nil))
		if err != nil {
			t.Fatal(err)
		}
		req, err := client.newRequest(context.Background(), http.MethodGet, "/start", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		// Pin current behavior: Go's Host includes the port, so a different
		// port is treated as cross-host and the token is dropped.
		if got := <-keySeen; got != "" {
			t.Errorf("port-differing hop carried token %q, want stripped", got)
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

// TestSessionCallUnknownStatus5xx pins current sessionCall behavior (G10):
// any status code with a parseable body carrying a non-empty status field
// yields a SessionState, not an error — even a 5xx with an unknown status.
func TestSessionCallUnknownStatus5xx(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"status":"weird","message":"unknown status"}`)
	}
	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatalf("unexpected error for a parseable 5xx body: %v", err)
	}
	if st.Status != "weird" {
		t.Errorf("status = %q, want weird", st.Status)
	}
}

// TestEndSession404Tolerated guards the EndSession 404 contract (E2E flow
// 10): a 404 DELETE is "nothing to end", not an error, while a 5xx is.
func TestEndSession404Tolerated(t *testing.T) {
	t.Run("404 tolerated", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"session not found"}`)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := client.EndSession(context.Background()); err != nil {
			t.Errorf("EndSession 404 = %v, want nil", err)
		}
	})

	t.Run("5xx surfaces error", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := client.EndSession(context.Background()); err == nil {
			t.Error("EndSession 500 succeeded, want error")
		}
	})
}

// TestClassify429ChatLevel guards 429 chat-level bodies (G10): ip_capped
// classifies as the distinct IpCappedError (admission-only, NOT a quota
// reset — never ErrRateLimited), while spend_limited keeps quota-lock
// RateLimitError semantics.
func TestClassify429ChatLevel(t *testing.T) {
	t.Run("ip_capped", func(t *testing.T) {
		err := classifyError(http.StatusTooManyRequests,
			`{"status":"ip_capped","activeUsersForIp":5,"limit":4,"retryAfterMs":30000}`, http.Header{})
		if errors.Is(err, ErrRateLimited) {
			t.Fatal("ip_capped classified as ErrRateLimited, want distinct ErrIpCapped")
		}
		var ice *IpCappedError
		if !errors.As(err, &ice) {
			t.Fatalf("err = %v, want *IpCappedError", err)
		}
		if !errors.Is(err, ErrIpCapped) {
			t.Errorf("err = %v, want ErrIpCapped", err)
		}
		if ice.ActiveUsersForIP != 5 || ice.Limit != 4 {
			t.Errorf("IpCappedError = %+v, want ActiveUsersForIP 5 limit 4", ice)
		}
		if ice.RetryAfter != 30*time.Second {
			t.Errorf("RetryAfter = %v, want 30s (bounded to retryAfterMs only)", ice.RetryAfter)
		}
	})
	t.Run("spend_limited", func(t *testing.T) {
		err := classifyError(http.StatusTooManyRequests,
			`{"status":"spend_limited","message":"Daily budget reached","retryAfterMs":60000}`, http.Header{})
		var rle *RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("err = %v, want RateLimitError", err)
		}
		if !errors.Is(err, ErrRateLimited) {
			t.Errorf("err = %v, want ErrRateLimited", err)
		}
		if rle.Status != "spend_limited" {
			t.Errorf("RateLimitError.Status = %q, want spend_limited", rle.Status)
		}
	})
}

// TestChatNonObjectBodyAndGzipError guards G12: a non-object chat body is
// rejected at the envelope stage, and a gzip-compressed 4xx error body is
// drained and decompressed before classification.
func TestChatNonObjectBodyAndGzipError(t *testing.T) {
	t.Run("non-object body rejected", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, []byte(`[1,2,3]`))
		if err == nil {
			t.Fatal("array chat body accepted, want envelope error")
		}
		if !strings.Contains(err.Error(), "envelope") {
			t.Errorf("err = %v, want an envelope error", err)
		}
		if mock.Requests != 0 {
			t.Errorf("upstream hit %d times for a rejected body, want 0", mock.Requests)
		}
	})

	t.Run("gzip 4xx body decompressed before classify", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusTooManyRequests)
			zw := gzip.NewWriter(w)
			_, _ = zw.Write([]byte(`{"status":"rate_limited","retryAfterMs":60000}`))
			_ = zw.Close()
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, []byte(`{"model":"m"}`))
		if !errors.Is(err, ErrRateLimited) {
			t.Errorf("err = %v, want ErrRateLimited (gzip body must be decompressed before classification)", err)
		}
	})
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

// TestFullChatLifecycleChained is E2E flow 7: create session, start run,
// chat (with instance-id + envelope), finish run, end session — in one
// chain, asserting the instance/run ids thread through.
func TestFullChatLifecycleChained(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lifecycle"},"finish_reason":null}]}`)

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	st, err := client.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if st.Status != "active" || st.InstanceID == "" {
		t.Fatalf("session = %+v, want active with an instance id", st)
	}

	runID, err := client.StartRun(ctx, "agent-1")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if runID == "" {
		t.Fatal("StartRun returned an empty run id")
	}

	rc, err := client.ChatCompletions(ctx, ChatOptions{Model: "m", RunID: runID, SessionInstanceID: st.InstanceID},
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(data), `"content":"lifecycle"`) {
		t.Errorf("stream missing chunk: %s", data)
	}
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("recorded chat headers = %d, want 1", len(mock.RecordedChatHeaders))
	}
	// #106: the chat POST carries no instance/model headers — they ride in
	// the body metadata only.
	if got := mock.RecordedChatHeaders[0].Get("x-freebuff-instance-id"); got != "" {
		t.Errorf("chat x-freebuff-instance-id = %q, want absent (#106)", got)
	}
	if got := mock.RecordedChatHeaders[0].Get("x-freebuff-model"); got != "" {
		t.Errorf("chat x-freebuff-model = %q, want absent (#106)", got)
	}
	if !mock.BodyContains(`"freebuff_instance_id":"` + st.InstanceID + `"`) {
		t.Error("chat body missing freebuff_instance_id in codebuff_metadata")
	}
	if !mock.BodyContains(`"run_id":"` + runID + `"`) {
		t.Error("chat body missing run_id in codebuff_metadata")
	}

	if err := client.FinishRun(ctx, runID, "completed", 3, nil, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if err := client.EndSession(ctx); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	if mock.SessionCreates != 1 || mock.SessionEnds != 1 {
		t.Errorf("session creates/ends = %d/%d, want 1/1", mock.SessionCreates, mock.SessionEnds)
	}
	if got := mock.StartedRunsSnapshot(); len(got) != 1 || got[0] != "agent-1" {
		t.Errorf("started runs = %v, want [agent-1]", got)
	}
	finished := mock.FinishedRunsSnapshot()
	if len(finished) != 1 || finished[0].RunID != runID || finished[0].TotalSteps != 3 {
		t.Errorf("finished runs = %+v, want run %s with 3 steps", finished, runID)
	}
}

// TestCompactPollAbsentTolerant is E2E flow 8: a compact poll without quota/
// offer fields parses cleanly with nil maps, and carries no heartbeat header.
func TestCompactPollAbsentTolerant(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	gotCompact := make(chan string, 1)
	gotHeartbeat := make(chan string, 1)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		gotCompact <- r.Header.Get("x-freebuff-compact-session")
		gotHeartbeat <- r.Header.Get("x-freebuff-heartbeat")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2026-08-17T10:00:00.000Z"}`)
	}

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.GetSessionWithOpts(context.Background(), "inst-1", true)
	if err != nil {
		t.Fatalf("compact poll: %v", err)
	}
	if st.Status != "active" || st.InstanceID != "inst-1" {
		t.Errorf("state = %+v, want active inst-1", st)
	}
	if st.RateLimitsByModel != nil {
		t.Errorf("RateLimitsByModel = %v, want nil on a compact poll without quotas", st.RateLimitsByModel)
	}
	if st.LimitedModelOffers != nil {
		t.Errorf("LimitedModelOffers = %v, want nil on a compact poll without offers", st.LimitedModelOffers)
	}
	if got := <-gotCompact; got != "1" {
		t.Errorf("compact header = %q, want 1", got)
	}
	if got := <-gotHeartbeat; got != "" {
		t.Errorf("heartbeat header = %q, want absent (CLI never beats)", got)
	}
}

// ── Wave 1 issue tests (#75, #81, #82, #79, #80, #76) ────────────────────

// TestClassifyCapacityDeferred verifies #75: a free_mode_capacity_deferred
// response classifies as the distinct CapacityDeferredError (retryable
// same-session condition), never a token cooldown or session invalidation.
func TestClassifyCapacityDeferred(t *testing.T) {
	err := classifyError(http.StatusTooManyRequests, `{"error":{"code":"free_mode_capacity_deferred","message":"Free mode is at capacity; your request will be retried automatically"}}`, http.Header{})
	var cde *CapacityDeferredError
	if !errors.As(err, &cde) {
		t.Fatalf("err = %v, want *CapacityDeferredError", err)
	}
	if !errors.Is(err, ErrCapacityDeferred) {
		t.Errorf("err = %v, want ErrCapacityDeferred", err)
	}
	// Unwraps to a Retryable UpstreamError (errors.As finds it), but
	// writeError surfaces 429 free_mode_capacity_deferred + Retry-After
	// via its dedicated CapacityDeferredError branch (#105).
	var ue *UpstreamError
	if !errors.As(err, &ue) || !ue.Retryable {
		t.Errorf("err = %v, want unwrap to Retryable UpstreamError", err)
	}
	if cde.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want 429", cde.Status)
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

// TestClassifyWaitingRoomQueued verifies #81: a 429 waiting_room_queued body
// is a transient admission race (endsTheSession:false) — surfaced as a
// WaitingRoomError, never session-invalid (no session refresh/recreate).
func TestClassifyWaitingRoomQueued(t *testing.T) {
	err := classifyError(http.StatusTooManyRequests, `{"error":{"code":"waiting_room_queued","message":"row caught mid-admit"}}`, http.Header{})
	if errors.Is(err, ErrSessionInvalid) {
		t.Fatal("waiting_room_queued classified as session-invalid, want transient WaitingRoomError")
	}
	var wr *WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("err = %v, want *WaitingRoomError", err)
	}
}

// TestClassifySessionLimitReached verifies #82: a 409 session_limit_reached
// response is a distinct non-invalid error carrying the code — the ACCOUNT
// is over its concurrent-tab budget but the session row is fine
// (endsTheSession:false), so no session refresh/recreate may trigger.
func TestClassifySessionLimitReached(t *testing.T) {
	err := classifyError(http.StatusConflict, `{"error":{"code":"session_limit_reached","message":"Concurrent tab limit reached"}}`, http.Header{})
	if errors.Is(err, ErrSessionInvalid) {
		t.Fatal("session_limit_reached classified as session-invalid; the row is fine")
	}
	var sle *SessionLimitError
	if !errors.As(err, &sle) {
		t.Fatalf("err = %v, want *SessionLimitError", err)
	}
	if !errors.Is(err, ErrSessionLimitReached) {
		t.Errorf("err = %v, want ErrSessionLimitReached", err)
	}
	if sle.Status != http.StatusConflict {
		t.Errorf("Status = %d, want 409", sle.Status)
	}
}

// TestChatSendsActingUserID verifies #79: when ACTING_USER_ID is configured
// the client sends x-freebuff-acting-user-id on the chat path (the CLI
// sends the account's own id derived from /api/v1/me); when unset the
// header is omitted.
func TestChatSendsActingUserID(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`)

	t.Run("set", func(t *testing.T) {
		client, err := New("tok-a", testConfig(mock.URL(), func(c *config.Config) { c.ActingUserID = "user-123" }))
		if err != nil {
			t.Fatal(err)
		}
		rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = rc.Close()
		if len(mock.RecordedChatHeaders) != 1 {
			t.Fatalf("want 1 chat request, got %d", len(mock.RecordedChatHeaders))
		}
		if got := mock.RecordedChatHeaders[0].Get("x-freebuff-acting-user-id"); got != "user-123" {
			t.Errorf("x-freebuff-acting-user-id = %q, want user-123", got)
		}
	})
	t.Run("unset omits header", func(t *testing.T) {
		client, err := New("tok-a", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = rc.Close()
		if got := mock.RecordedChatHeaders[1].Get("x-freebuff-acting-user-id"); got != "" {
			t.Errorf("x-freebuff-acting-user-id = %q, want unset", got)
		}
	})
}

// TestWaitingRoomChainWireFidelity verifies #124: the pre-session ad chain
// matches the CLI wire shape — header UA Freebuff-CLI/0.0.149 (never the
// old 2.0.42 login UA), body userAgent = the Chrome-124 browser UA,
// device carries the host IANA timezone/locale, messages stays [] with no
// sessionId (fresh waiting-room), and the streak GET inherits newRequest's
// cliUserAgent (no UA override).
func TestWaitingRoomChainWireFidelity(t *testing.T) {
	var mu sync.Mutex
	var adsHeaders, streakHeaders http.Header
	var adsBody map[string]any
	adsHits, streakHits := 0, 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/ads" && r.Method == http.MethodPost:
			adsHits++
			adsHeaders = r.Header.Clone()
			_ = json.NewDecoder(r.Body).Decode(&adsBody)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ads":[],"provider":"gravity"}`)
		case r.URL.Path == "/api/v1/freebuff/streak" && r.Method == http.MethodGet:
			streakHits++
			streakHeaders = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client, err := New("tok-a", testConfig(ts.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	client.FireWaitingRoomChain(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if adsHits == 0 {
		t.Fatal("ads request not fired")
	}
	if streakHits == 0 {
		t.Fatal("streak request not fired")
	}
	// Header UA: Freebuff-CLI/<installed binary version>.
	if got := adsHeaders.Get("User-Agent"); got != freebuffCliUA {
		t.Errorf("ads header User-Agent = %q, want %q", got, freebuffCliUA)
	}
	// Body userAgent: the platform-consistent Chrome-124 browser UA (ad
	// targeting) — must agree with the device block's os.
	if got := adsBody["userAgent"]; got != adBrowserUserAgent() {
		t.Errorf("ads body userAgent = %q, want %q", got, adBrowserUserAgent())
	}
	// Device block: host-derived IANA tz/locale, not hardcoded UTC/en-US.
	device, ok := adsBody["device"].(map[string]any)
	if !ok {
		t.Fatalf("ads body device = %T, want object", adsBody["device"])
	}
	tz, _ := device["timezone"].(string)
	if tz == "" || tz == "Local" {
		t.Errorf("ads device timezone = %q, want host IANA name or UTC", tz)
	} else if _, err := time.LoadLocation(tz); err != nil {
		t.Errorf("ads device timezone %q is not a valid IANA zone", tz)
	}
	loc, _ := device["locale"].(string)
	if loc == "" || loc == "C" || loc == "POSIX" || strings.Contains(loc, "_") {
		t.Errorf("ads device locale = %q, want a BCP-47-style locale (e.g. en-US)", loc)
	}
	// The device os follows the host's wire mapping (darwin→macos) and the
	// body UA agrees with it — the CLI picks both from the same platform.
	if os, _ := device["os"].(string); os != deviceOS() {
		t.Errorf("ads device os = %q, want %q (host wire mapping)", os, deviceOS())
	}
	// Faithful details kept: empty messages and NO sessionId (the chain
	// fires before a session exists).
	if msgs, _ := adsBody["messages"].([]any); len(msgs) != 0 {
		t.Errorf("ads body messages = %v, want []", msgs)
	}
	if _, hasSession := adsBody["sessionId"]; hasSession {
		t.Error("ads body carries sessionId, want omitted (fresh waiting-room)")
	}
	// Streak GET: no UA override — it inherits newRequest's cliUserAgent.
	if got := streakHeaders.Get("User-Agent"); got != cliUserAgent {
		t.Errorf("streak User-Agent = %q, want %q (cliUserAgent, no override)", got, cliUserAgent)
	}
}

// TestInjectEnvelopeTraceSessionIDAndFreshClientID verifies #80+#103: the
// envelope injects trace_session_id when carried by ChatOptions (stable per
// run) while client_id is a FRESH random draw per call (never derived from
// the run id).
func TestInjectEnvelopeTraceSessionIDAndFreshClientID(t *testing.T) {
	out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "run-1", TraceSessionID: "trace-abc"})
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	md := sent["codebuff_metadata"].(map[string]any)
	if md["trace_session_id"] != "trace-abc" {
		t.Errorf("trace_session_id = %v, want trace-abc", md["trace_session_id"])
	}
	if id, _ := md["client_id"].(string); !regexp.MustCompile(`^[a-z0-9]{13}$`).MatchString(id) || strings.HasPrefix(id, "run:") {
		t.Errorf("client_id = %v, want a fresh unprefixed 13-char base36 draw (#103)", md["client_id"])
	}
	// Re-injecting the same run yields a DIFFERENT client_id across calls.
	out2, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	var sent2 map[string]any
	_ = json.Unmarshal(out2, &sent2)
	if md2 := sent2["codebuff_metadata"].(map[string]any); md2["client_id"] == md["client_id"] {
		t.Errorf("client_id = %v, want a fresh draw per request (same run)", md2["client_id"])
	}
	// Without a run id the SDK-faithful 13-char base36 draw is kept.
	out3, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var sent3 map[string]any
	_ = json.Unmarshal(out3, &sent3)
	md3 := sent3["codebuff_metadata"].(map[string]any)
	if id, _ := md3["client_id"].(string); !regexp.MustCompile(`^[0-9a-z]{13}$`).MatchString(id) {
		t.Errorf("client_id %q not 13-char base36 when no run id", id)
	}
}

// TestProbeAccountSendsIncludeUnusedRateLimits verifies #76: the zero-cost
// GET probe carries x-freebuff-include-unused-rate-limits: 1 so the response
// includes accessTier/glmPromo/resetAt/rateLimitsByModel for dashboard
// display, and sessionCall parses the new fields.
func TestProbeAccountSendsIncludeUnusedRateLimits(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var gotHeader string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-freebuff-include-unused-rate-limits")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","accessTier":"limited","glmPromo":{"dailySessions":2,"endsAt":"2026-08-20T07:00:00.000Z"},"rateLimitsByModel":{"deepseek/deepseek-v4-flash":{"model":"deepseek/deepseek-v4-flash","limit":6,"recentCount":2,"period":"pacific_day","resetAt":"2026-08-18T07:00:00.000Z"}}}`)
	}
	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotHeader != "1" {
		t.Errorf("x-freebuff-include-unused-rate-limits = %q, want 1", gotHeader)
	}
	if st.AccessTier != "limited" {
		t.Errorf("AccessTier = %q, want limited", st.AccessTier)
	}
	if st.GlmPromo == "" || !strings.Contains(st.GlmPromo, "dailySessions") {
		t.Errorf("GlmPromo = %q, want raw glmPromo JSON", st.GlmPromo)
	}
	if st.RateLimitsByModel == nil || st.RateLimitsByModel["deepseek/deepseek-v4-flash"].Limit != 6 {
		t.Errorf("RateLimitsByModel = %+v, want parsed per-model quota", st.RateLimitsByModel)
	}
}

func TestDeviceOSWireContract(t *testing.T) {
	tests := []struct {
		goos string
		want string
	}{
		{"darwin", "macos"}, // Go reports darwin, wire contract wants macos
		{"windows", "windows"},
		{"linux", "linux"},
		{"freebsd", "linux"}, // CLI falls back to linux for unknown platforms
		{"", "linux"},
	}
	for _, tt := range tests {
		if got := deviceOSFor(tt.goos); got != tt.want {
			t.Errorf("deviceOSFor(%q) = %q, want %q", tt.goos, got, tt.want)
		}
	}
}

// TestReqIDContextHelpers pins the D1 ctx plumbing: withReqID stores the id
// and ReqID reads it through descendant contexts (the timeout wraps in
// ChatCompletions/do derive from the wrapped ctx).
func TestReqIDContextHelpers(t *testing.T) {
	if got := ReqID(context.Background()); got != "" {
		t.Errorf("ReqID(background) = %q, want empty", got)
	}
	ctx := withReqID(context.Background(), "req-123")
	if got := ReqID(ctx); got != "req-123" {
		t.Errorf("ReqID = %q, want req-123", got)
	}
	child, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if got := ReqID(child); got != "req-123" {
		t.Errorf("ReqID(child) = %q, want req-123 (value must survive descendant wraps)", got)
	}
}

// TestDumpWriteFailureLogsWarn verifies T18: when DEBUG_DUMP is enabled but
// the dump write fails (a regular file occupies the dump/ path), the failure
// is logged as a WARN with path and err instead of being swallowed.
func TestDumpWriteFailureLogsWarn(t *testing.T) {
	orig := slog.Default()
	var sink bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	t.Chdir(t.TempDir())
	// A regular FILE named "dump": MkdirAll fails and WriteFile hits
	// ENOTDIR/EEXIST — deterministic failure injection.
	if err := os.WriteFile("dump", []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}
	client, err := New("tok", testConfig("", func(c *config.Config) { c.DebugDump = true }))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://www.codebuff.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	client.dump("chat", req, http.StatusOK, "response body")

	logs := sink.String()
	if !strings.Contains(logs, "debug dump write failed") {
		t.Fatalf("dump WARN missing: %s", logs)
	}
	if !strings.Contains(logs, "path=") || !strings.Contains(logs, "err=") {
		t.Errorf("dump WARN missing path/err attrs: %s", logs)
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

	// The daily-cap path is untouched: a plain rate_limited 429 without the
	// markers still goes through parseRateLimit's midnight default.
	err := classifyError(http.StatusTooManyRequests, `{"status":"rate_limited","message":"daily quota"}`, http.Header{})
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("plain 429 = %T %v, want *RateLimitError", err, err)
	}
	if rle.Status != "" && rle.Status == "load_shedding" {
		t.Error("plain 429 misclassified as load_shedding")
	}
	if rle.ResetAt.IsZero() {
		t.Error("plain no-timestamp 429 lost the Pacific-midnight lock")
	}
}
