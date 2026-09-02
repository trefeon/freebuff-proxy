package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// Internal (package server) auth tests: these exercise adminAuth with the
// real constants, so the lockout bound and map cap cannot drift from the
// public behavior the dashboard_test.go rate-limit test depends on.

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		input     string
		wantToken string
		wantOK    bool
	}{
		{"Bearer test-tok", "test-tok", true},
		{"bearer test-tok", "test-tok", true},
		{"BEARER test-tok", "test-tok", true},
		{"bEaReR test-tok", "test-tok", true},
		{"  bearer  test-tok  ", "test-tok", true},
		{"Bearer ", "", false},
		{"bearer   ", "", false},
		{"Bearer", "", false},
		{"bearer", "", false},
		{"Basic test-tok", "", false},
		{"", "", false},
		{"x-api-key test-tok", "", false},
	}
	for _, tt := range tests {
		gotToken, gotOK := extractBearerToken(tt.input)
		if gotToken != tt.wantToken || gotOK != tt.wantOK {
			t.Errorf("extractBearerToken(%q) = (%q, %v), want (%q, %v)",
				tt.input, gotToken, gotOK, tt.wantToken, tt.wantOK)
		}
	}
}

func TestAdminAuthLockoutBound(t *testing.T) {
	a := newAdminAuth()
	// maxLoginFails wrong attempts fill the counter; the next allow() must
	// deny while the lockout window is active.
	for range maxLoginFails {
		a.recordFail("10.0.0.1")
	}
	if a.allow("10.0.0.1") {
		t.Fatal("allow() = true after maxLoginFails failures, want locked out")
	}
	// A successful login from the same IP clears the lockout.
	a.clearFails("10.0.0.1")
	if !a.allow("10.0.0.1") {
		t.Fatal("allow() = false after clearFails, want allowed")
	}
}

func TestAdminAuthExpiredLockoutEvicts(t *testing.T) {
	a := newAdminAuth()
	a.fails["10.0.0.9"] = failEntry{count: 0, until: time.Now().Add(-time.Second)}
	if !a.allow("10.0.0.9") {
		t.Fatal("allow() = false after lockout expiry, want allowed")
	}
	if _, ok := a.fails["10.0.0.9"]; ok {
		t.Fatal("expired fail entry not evicted")
	}
}

func TestAdminAuthFailsMapCapped(t *testing.T) {
	a := newAdminAuth()
	// More distinct fresh-lockout IPs than the cap: the map must stay
	// bounded even though no entry has expired.
	for i := range loginFailsCap + 100 {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		a.recordFail(ip)
	}
	if got := len(a.fails); got > loginFailsCap {
		t.Fatalf("fails map = %d entries, want <= %d", got, loginFailsCap)
	}
}

func TestAdminCookieDynamicProtocol(t *testing.T) {
	a := newAdminAuth()
	reqHTTP := httptest.NewRequest(http.MethodGet, "http://192.168.1.100:3457/admin", nil)
	reqHTTPS := httptest.NewRequest(http.MethodGet, "http://192.168.1.100:3457/admin", nil)
	reqHTTPS.Header.Set("X-Forwarded-Proto", "https")

	// 1. Plain HTTP -> Secure is false (zero friction for self-hosted cloud VPS users).
	rec1 := httptest.NewRecorder()
	a.setCookie(rec1, reqHTTP)
	if rec1.Result().Cookies()[0].Secure {
		t.Error("plain-HTTP cookie must have Secure: false by default")
	}
	if csrfCookie("csrf1", reqHTTP).Secure {
		t.Error("plain-HTTP csrfCookie must have Secure: false by default")
	}

	// 2. HTTPS (via X-Forwarded-Proto or TLS) -> Secure is true.
	rec2 := httptest.NewRecorder()
	a.setCookie(rec2, reqHTTPS)
	if !rec2.Result().Cookies()[0].Secure {
		t.Error("HTTPS cookie must have Secure: true")
	}
	if !csrfCookie("csrf2", reqHTTPS).Secure {
		t.Error("HTTPS csrfCookie must have Secure: true")
	}

	// 3. Explicit force via ADMIN_FORCE_SECURE_COOKIES=true -> Secure is true even on plain HTTP.
	t.Setenv("ADMIN_FORCE_SECURE_COOKIES", "true")
	rec3 := httptest.NewRecorder()
	a.setCookie(rec3, reqHTTP)
	if !rec3.Result().Cookies()[0].Secure {
		t.Error("ADMIN_FORCE_SECURE_COOKIES=true must enforce Secure: true even on plain HTTP")
	}
}

// TestAdminCookieExpiredRedirects pins the expired-but-valid-HMAC cookie
// path: a correctly signed cookie whose expiry is in the past must fail
// validation and redirect to login, while the same signing with a future
// expiry is accepted — proving the expiry check, not the HMAC, is the gate.
func TestAdminCookieExpiredRedirects(t *testing.T) {
	s := &Server{adminAuth: newAdminAuth(), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.cfg.Store(&config.Config{AdminToken: "secret"})
	s.admin = &adminHandlers{
		adminAuth: s.adminAuth,
		cfgLoad:   s.cfg.Load,
		logfunc:   func() *slog.Logger { return s.logger },
	}
	h := s.admin.dashboardAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	expired := s.adminAuth.cookieValue(time.Now().Add(-time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: expired})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expired-valid cookie status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/login" {
		t.Errorf("redirect location = %q, want /admin/login", loc)
	}

	// Same signature machinery, future expiry → accepted.
	future := s.adminAuth.cookieValue(time.Now().Add(time.Hour))
	req2 := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req2.AddCookie(&http.Cookie{Name: adminCookieName, Value: future})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("future-valid cookie status = %d, want 200 (HMAC + expiry both valid)", rec2.Code)
	}
}

// assertNoTmpFiles fails if writeFileAtomic left its temp file behind.
func assertNoTmpFiles(t *testing.T, dir, base string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "."+base+".tmp*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

// errorResponse decodes the OpenAI error shape writeError produces.
func errorResponse(t *testing.T, err error) (status int, hdr http.Header, body struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
		Hint    string `json:"hint"`
	} `json:"error"`
}) {
	t.Helper()
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.writeError(w, r, err, "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("writeError response is not JSON: %v: %s", err, w.Body.Bytes())
	}
	return w.Code, w.Header(), body
}

// TestWriteErrorNewMappings pins the self-healing error matrix additions:
// country block → 403, free-mode CLI gate → 403, credits → 402 (upstream body
// passed through verbatim), upstream deadline → 504.
func TestWriteErrorNewMappings(t *testing.T) {
	t.Run("country blocked 403", func(t *testing.T) {
		err := &upstream.CountryBlockedError{CountryCode: "CN", CountryBlockReason: "region_restricted", IpPrivacySignals: []string{"vpn"}}
		status, _, body := errorResponse(t, err)
		if status != http.StatusForbidden {
			t.Errorf("status = %d, want 403", status)
		}
		if body.Error.Code != "country_blocked" {
			t.Errorf("code = %q, want country_blocked", body.Error.Code)
		}
		if body.Error.Hint == "" || !strings.Contains(body.Error.Hint, "Route traffic through an allowed country") {
			t.Errorf("hint = %q, want actionable egress hint", body.Error.Hint)
		}
	})

	t.Run("free mode cli required 403", func(t *testing.T) {
		err := fmt.Errorf("free tier gate: %w", upstream.ErrFreeModeCLIRequired)
		status, _, body := errorResponse(t, err)
		if status != http.StatusForbidden {
			t.Errorf("status = %d, want 403", status)
		}
		if body.Error.Code != "free_mode_cli_required" {
			t.Errorf("code = %q, want free_mode_cli_required", body.Error.Code)
		}
		if !strings.Contains(errors.Unwrap(err).Error(), "CLI") && !strings.Contains(body.Error.Hint, "CLI") {
			t.Errorf("hint = %q, want CLI envelope hint", body.Error.Hint)
		}
	})

	t.Run("credits 402 with body passthrough", func(t *testing.T) {
		const upstreamBody = `{"error":"out of credits","model":"deepseek/deepseek-v4-flash"}`
		err := &upstream.CreditsError{Status: http.StatusPaymentRequired, Body: upstreamBody}
		status, _, body := errorResponse(t, err)
		if status != http.StatusPaymentRequired {
			t.Errorf("status = %d, want 402", status)
		}
		if body.Error.Code != "out_of_credits" {
			t.Errorf("code = %q, want out_of_credits", body.Error.Code)
		}
		if body.Error.Message != upstreamBody {
			t.Errorf("message = %q, want upstream body verbatim (no passthrough loss)", body.Error.Message)
		}
		if body.Error.Hint == "" || !strings.Contains(body.Error.Hint, "COST_MODE") {
			t.Errorf("hint = %q, want COST_MODE hint", body.Error.Hint)
		}
	})

	t.Run("waiting room required 503 with retry-after", func(t *testing.T) {
		// #116: 428 waiting_room_required surfaces as 503 waiting_room_required
		// + Retry-After — NEVER a bare 502.
		err := &upstream.WaitingRoomRequiredError{RetryAfter: 45 * time.Second, Detail: `{"error":"waiting_room_required"}`}
		status, hdr, body := errorResponse(t, err)
		if status != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", status)
		}
		if body.Error.Code != "waiting_room_required" {
			t.Errorf("code = %q, want waiting_room_required", body.Error.Code)
		}
		if got := hdr.Get("Retry-After"); got != "45" {
			t.Errorf("Retry-After = %q, want 45 (the refusal's retryAfter, ceil seconds)", got)
		}
	})

	t.Run("session superseded 503", func(t *testing.T) {
		// #119: 503 session_superseded — returns 503 + Retry-After so 9router
		// retries immediately instead of locking the model for 30s.
		const upstreamBody = `{"error":"session_superseded","message":"another CLI took over"}`
		err := &upstream.SessionSupersededError{Status: http.StatusConflict, Body: upstreamBody}
		status, hdr, body := errorResponse(t, err)
		if status != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", status)
		}
		if body.Error.Code != "session_superseded" {
			t.Errorf("code = %q, want session_superseded", body.Error.Code)
		}
		if body.Error.Message != upstreamBody {
			t.Errorf("message = %q, want upstream body verbatim", body.Error.Message)
		}
		if got := hdr.Get("Retry-After"); got != "1" {
			t.Errorf("Retry-After = %q, want 1", got)
		}
	})

	t.Run("upstream deadline 504", func(t *testing.T) {
		err := fmt.Errorf("chat: %w", context.DeadlineExceeded)
		status, _, body := errorResponse(t, err)
		if status != http.StatusGatewayTimeout {
			t.Errorf("status = %d, want 504", status)
		}
		if body.Error.Code != "upstream_timeout" {
			t.Errorf("code = %q, want upstream_timeout", body.Error.Code)
		}
		if body.Error.Hint == "" {
			t.Error("hint empty, want retry/REQUEST_TIMEOUT hint")
		}
	})
}

// TestWriteErrorExistingMappingsUnchanged guards the PRD §6 matrix: ban stays
// 403 account_banned, rate limit 429, waiting room 503 — the new mappings
// must not shadow them.
func TestWriteErrorExistingMappingsUnchanged(t *testing.T) {
	status, _, body := errorResponse(t, &upstream.BanError{ResumesAt: time.Now().Add(time.Hour), Body: `{"status":"banned"}`})
	if status != http.StatusForbidden || body.Error.Code != "account_banned" {
		t.Errorf("ban: status=%d code=%q, want 403 account_banned", status, body.Error.Code)
	}

	status, _, body = errorResponse(t, &upstream.RateLimitError{RetryAfter: time.Minute})
	if status != http.StatusTooManyRequests || body.Error.Code != "rate_limited" {
		t.Errorf("rate limit: status=%d code=%q, want 429 rate_limited", status, body.Error.Code)
	}

	status, _, body = errorResponse(t, &upstream.WaitingRoomError{RetryAfter: time.Minute})
	if status != http.StatusServiceUnavailable || body.Error.Code != "waiting_room_queued" {
		t.Errorf("waiting room: status=%d code=%q, want 503 waiting_room_queued", status, body.Error.Code)
	}
}

// TestRestoreEnvFileUnreadable pins the mode-switch rollback guard: when the
// previous .env existed but was unreadable (oldErr not os.ErrNotExist), the
// rollback must NOT delete the file — removing it would destroy an operator's
// present-but-unreadable .env (regression). POSIX-only:
// chmod 000 does not block reads on Windows.
func TestRestoreEnvFileUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 000 does not make a file unreadable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits")
	}
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("SAFE_MODE=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(".env", 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(".env", 0o644); err != nil {
			t.Errorf("restoring .env perms: %v", err)
		}
	}()
	_, readErr := os.ReadFile(".env")
	if readErr == nil || errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("setup: ReadFile = %v, want a non-NotExist error", readErr)
	}
	restoreEnvFile(nil, readErr)
	if _, statErr := os.Stat(".env"); statErr != nil {
		t.Errorf("restoreEnvFile removed a present-but-unreadable .env: %v", statErr)
	}
}

// TestUpdateEnvKeys pins the multi-key .env writer behind the mode switches:
// replaces existing keys, appends missing ones, and preserves CRLF line
// endings (a Windows-edited .env must never be rewritten mixed-EOL).
func TestUpdateEnvKeys(t *testing.T) {
	t.Chdir(t.TempDir())
	// Windows: Defender may hold a scan handle on a just-written .env,
	// leaving a locked .bak/.tmp stray that breaks TempDir's RemoveAll.
	// Registered after Chdir, so the drain runs first (LIFO) while the
	// working directory still points at the temp dir.
	testutil.DrainStrayTempFiles(t, ".")
	if err := os.WriteFile(".env", []byte("SAFE_MODE=true\r\nAUTH_TOKENS=tok-a\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := updateEnvKeys([]config.EnvUpdate{
		{Key: "AUTH_TOKENS", Value: ""},
		{Key: "SAFE_MODE", Value: "false"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	want := "SAFE_MODE=false\r\nAUTH_TOKENS=\r\n"
	if string(got) != want {
		t.Errorf(".env after update = %q, want %q", got, want)
	}
	assertNoTmpFiles(t, ".", ".env")

	// Flip SAFE_MODE back to true: in-place replace, no duplicate line.
	if _, err := updateEnvKeys([]config.EnvUpdate{{Key: "SAFE_MODE", Value: "true"}}); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got), "SAFE_MODE=") != 1 || !strings.Contains(string(got), "SAFE_MODE=true") {
		t.Errorf(".env after flip = %q, want single SAFE_MODE=true line", got)
	}
}

// ── Wave 1 issue tests (#81, #82, #76) ───────────────────────────────────

// TestWriteErrorIpCappedAndSessionLimit verifies the writeError mappings for
// the wave-1 typed errors: ip_capped surfaces 429 with code "ip_capped"
// (never the quota "rate_limited"), and session_limit_reached surfaces 409
// with its code (never session-invalid).
func TestWriteErrorIpCappedAndSessionLimit(t *testing.T) {
	t.Run("ip capped 429", func(t *testing.T) {
		err := &upstream.IpCappedError{ActiveUsersForIP: 5, Limit: 4, RetryAfter: 45 * time.Second, Body: `{"status":"ip_capped"}`}
		status, _, body := errorResponse(t, err)
		if status != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429", status)
		}
		if body.Error.Code != "ip_capped" {
			t.Errorf("code = %q, want ip_capped (not rate_limited)", body.Error.Code)
		}
		if !strings.Contains(body.Error.Message, "retry after 45s") {
			t.Errorf("message = %q, want bounded Retry-After detail", body.Error.Message)
		}
	})
	t.Run("session limit reached 409", func(t *testing.T) {
		err := &upstream.SessionLimitError{Status: http.StatusConflict, Body: `{"error":{"code":"session_limit_reached"}}`}
		status, _, body := errorResponse(t, err)
		if status != http.StatusConflict {
			t.Errorf("status = %d, want 409", status)
		}
		if body.Error.Code != "session_limit_reached" {
			t.Errorf("code = %q, want session_limit_reached", body.Error.Code)
		}
		if body.Error.Message != `{"error":{"code":"session_limit_reached"}}` {
			t.Errorf("message = %q, want upstream body verbatim", body.Error.Message)
		}
	})
}

// TestQuotaSummaryGlmPromo verifies #76: quotaSummary surfaces glmPromo from
// a probe response carrying the unused rate limits, and still returns ""
// when the response has no quota data.
func TestQuotaSummaryGlmPromo(t *testing.T) {
	if got := quotaSummary(nil); got != "" {
		t.Errorf("quotaSummary(nil) = %q, want empty", got)
	}
	if got := quotaSummary(&upstream.SessionState{}); got != "" {
		t.Errorf("quotaSummary(no data) = %q, want empty", got)
	}
	st := &upstream.SessionState{
		GlmPromo: `{"dailySessions":2,"endsAt":"2026-08-20T07:00:00.000Z"}`,
		RateLimitsByModel: map[string]upstream.ModelQuota{
			"deepseek/deepseek-v4-flash": {Model: "deepseek/deepseek-v4-flash", Limit: 6, RecentCount: 2, Period: "pacific_day", ResetAt: time.Date(2026, 8, 18, 7, 0, 0, 0, time.UTC)},
		},
	}
	got := quotaSummary(st)
	for _, want := range []string{"deepseek/deepseek-v4-flash 6/2 pacific_day", "glmPromo", "dailySessions"} {
		if !strings.Contains(got, want) {
			t.Errorf("quotaSummary = %q, missing %q", got, want)
		}
	}
}

// TestWriteErrorModelIPLimited pins the Issue #74 writeError mapping:
// *upstream.LimitedIpError → 409, code model_ip_limited, Retry-After = ceil
// seconds of lie.RetryAfter (only when > 0 — the body window is surfaced but
// never sets the unfit registry TTL).
func TestWriteErrorModelIPLimited(t *testing.T) {
	err := &upstream.LimitedIpError{
		RetryAfter: 5 * time.Minute,
		Body:       `{"status":"session_model_mismatch","message":"model z-ai/glm-5.2 is limited on this IP"}`,
	}
	status, _, body := errorResponse(t, err)
	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409", status)
	}
	if body.Error.Code != "model_ip_limited" {
		t.Errorf("code = %q, want model_ip_limited", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "model limited on this egress IP") {
		t.Errorf("message = %q, want limited-egress phrasing", body.Error.Message)
	}

	// Retry-After header: ceil seconds of RetryAfter, only when > 0.
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.writeError(w, r, err, "", nil)
	if got := w.Header().Get("Retry-After"); got != "300" {
		t.Errorf("Retry-After = %q, want 300", got)
	}

	// A zero RetryAfter must not emit the header.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.writeError(w2, r2, &upstream.LimitedIpError{Body: "no window"}, "", nil)
	if got := w2.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After with zero RetryAfter = %q, want empty", got)
	}
}

// TestWriteErrorBareModelIPLimitedSentinel pins the #74 contract for the
// bare sentinel (a registry entry stored without refusal detail): 409 +
// code model_ip_limited, no Retry-After header.
func TestWriteErrorBareModelIPLimitedSentinel(t *testing.T) {
	status, _, body := errorResponse(t, upstream.ErrModelIPLimited)
	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409", status)
	}
	if body.Error.Code != "model_ip_limited" {
		t.Errorf("code = %q, want model_ip_limited", body.Error.Code)
	}
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.writeError(w, r, upstream.ErrModelIPLimited, "", nil)
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want none for bare sentinel", got)
	}
}

// TestNewReqIDUUIDv4 pins the correlation-id mint (D1): RFC 4122 §4.4
// shape — version nibble 4, variant bits 10 — and a fresh value per mint.
func TestNewReqIDUUIDv4(t *testing.T) {
	id := newReqID()
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !re.MatchString(id) {
		t.Errorf("newReqID() = %q, want UUIDv4 shape", id)
	}
	if id2 := newReqID(); id2 == id {
		t.Error("two mints produced the same id")
	}
}

// TestClientRequestIDSanitize pins the X-Request-Id sanitizer (D1): trimmed,
// printable ASCII only, max 64 runes, else dropped ("").
func TestClientRequestIDSanitize(t *testing.T) {
	cases := []struct {
		hdr  string
		want string
	}{
		{"", ""},
		{"abc", "abc"},
		{"  abc  ", "abc"}, // trimmed
		{"a b", "a b"},     // inner spaces kept
		{strings.Repeat("x", 64), strings.Repeat("x", 64)},
		{strings.Repeat("x", 65), ""}, // >64 runes dropped
		{"héllo", ""},                 // non-ASCII dropped
		{"line\nbreak", ""},           // control character dropped
		{"tab\there", ""},             // control character dropped
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.hdr != "" {
			r.Header.Set("X-Request-Id", c.hdr)
		}
		if got := clientRequestID(r); got != c.want {
			t.Errorf("clientRequestID(%q) = %q, want %q", c.hdr, got, c.want)
		}
	}
}

// TestWriteErrorLoadSheddingAndPeakHours pins issue #133: load-saturation
// and peak-hours 429s surface with honest codes and bounded Retry-After, so
// routers show the right hint instead of "daily message cap or rate limit
// reached".
func TestWriteErrorLoadSheddingAndPeakHours(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantCode     string
		wantRetryMin time.Duration
		wantRetryMax time.Duration
	}{
		{"load_shedding", &upstream.RateLimitError{Status: "load_shedding", RetryAfter: upstream.LoadShedCooldown, Body: "load saturated"}, "load_shedding", 60 * time.Second, 120 * time.Second},
		{"peak_hours", &upstream.RateLimitError{Status: "peak_hours", RetryAfter: upstream.PeakHoursCooldown, Body: "peak hours"}, "peak_hours", 25 * time.Minute, 35 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _, body := errorResponse(t, tt.err)
			if status != http.StatusTooManyRequests {
				t.Errorf("status = %d, want 429", status)
			}
			if body.Error.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.wantCode)
			}
			s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			s.writeError(w, r, tt.err, "", nil)
			got, _ := time.ParseDuration(w.Header().Get("Retry-After") + "s")
			if got < tt.wantRetryMin || got > tt.wantRetryMax {
				t.Errorf("Retry-After = %v, want within [%v, %v] (bounded)", got, tt.wantRetryMin, tt.wantRetryMax)
			}
		})
	}
}

// TestAdminReloadAdminSensitiveGate pins the auth boundary on POST
// /admin/reload: reload is a state-changing admin action (re-reads .env +
// JSON, atomically swaps config, resets pool/registry/rate-limiter), so with
// ADMIN_TOKEN unset it must require a loopback client (adminSensitive),
// mirroring the /admin/config wiring. With ADMIN_TOKEN set the bearer-token
// gate still applies and keeps working from non-loopback clients.
func TestAdminReloadAdminSensitiveGate(t *testing.T) {
	// Isolate cwd: handleReload runs config.Load("") which reads ./.env
	// (see TestAdminReload).
	t.Chdir(t.TempDir())

	reload := func(t *testing.T, srv *Server, remote, host, auth string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
		req.RemoteAddr = remote
		req.Host = host
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	t.Run("open mode non-loopback forbidden", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		srv := newServerOpts(t, mock, nil) // no ADMIN_TOKEN, no API keys
		if got := reload(t, srv, "198.51.100.7:1234", "proxy.example.com", ""); got != http.StatusForbidden {
			t.Errorf("reload from non-loopback client without ADMIN_TOKEN = %d, want 403", got)
		}
	})

	t.Run("open mode loopback allowed", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		srv := newServerOpts(t, mock, nil)
		if got := reload(t, srv, "127.0.0.1:1234", "127.0.0.1:3457", ""); got != http.StatusOK {
			t.Errorf("reload from loopback client = %d, want 200", got)
		}
	})

	t.Run("admin token bearer gate", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		srv := newServerOpts(t, mock, func(c *config.Config) { c.AdminToken = "admin-secret" })
		if got := reload(t, srv, "198.51.100.7:1234", "proxy.example.com", ""); got != http.StatusUnauthorized {
			t.Errorf("reload without bearer = %d, want 401", got)
		}
		if got := reload(t, srv, "198.51.100.7:1234", "proxy.example.com", "admin-secret"); got != http.StatusOK {
			t.Errorf("reload with bearer = %d, want 200", got)
		}
	})

	t.Run("factory default token remote forbidden", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		srv := newServerOpts(t, mock, func(c *config.Config) { c.AdminToken = config.DefaultAdminToken })
		if got := reload(t, srv, "198.51.100.7:1234", "proxy.example.com", config.DefaultAdminToken); got != http.StatusForbidden {
			t.Errorf("reload from non-loopback under factory-default token = %d, want 403 (publicly-known password is anonymous-equivalent)", got)
		}
	})

	t.Run("factory default token loopback allowed", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		srv := newServerOpts(t, mock, func(c *config.Config) { c.AdminToken = config.DefaultAdminToken })
		if got := reload(t, srv, "127.0.0.1:1234", "127.0.0.1:3457", config.DefaultAdminToken); got != http.StatusOK {
			t.Errorf("reload from loopback under factory-default token = %d, want 200", got)
		}
	})

	t.Run("admin bearer works even with API_KEYS configured", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		srv := newServerOpts(t, mock, func(c *config.Config) {
			c.AdminToken = "admin-secret"
			c.APIKeys = []string{"sk-client"}
		})
		// DASH-AUTH-001 regression: the old chain stacked requireAuth above
		// adminSensitive, demanding the admin bearer ALSO be a client API
		// key — no single credential satisfied both and the documented
		// ADMIN_TOKEN-only workflow 401'd.
		if got := reload(t, srv, "198.51.100.7:1234", "proxy.example.com", "admin-secret"); got != http.StatusOK {
			t.Errorf("reload with admin bearer while API_KEYS set = %d, want 200", got)
		}
	})
}

// TestWriteErrorLegacyLunaAgent verifies that free_mode_legacy_luna_agent
// surfaces as 502 with the dedicated code (not generic session_invalid).
func TestWriteErrorLegacyLunaAgent(t *testing.T) {
	body := `{"error":"free_mode_legacy_luna_agent","message":"This conversation uses a retired Luna agent."}`
	err := fmt.Errorf("%w: %s", upstream.ErrSessionInvalid, body)
	status, _, writeBody := errorResponse(t, err)
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
	if writeBody.Error.Code != "free_mode_legacy_luna_agent" {
		t.Errorf("code = %q, want free_mode_legacy_luna_agent", writeBody.Error.Code)
	}
	if !strings.Contains(string(writeBody.Error.Message), "Retired Luna agent") {
		t.Errorf("message = %q, want Retired Luna agent hint", writeBody.Error.Message)
	}
}

// TestWriteErrorSessionInvalid verifies that generic session-invalid errors
// surface as 502 with code "session_invalid".
func TestWriteErrorSessionInvalid(t *testing.T) {
	err := fmt.Errorf("%w: session expired", upstream.ErrSessionInvalid)
	status, _, writeBody := errorResponse(t, err)
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
	if writeBody.Error.Code != "session_invalid" {
		t.Errorf("code = %q, want session_invalid", writeBody.Error.Code)
	}
	if !strings.Contains(writeBody.Error.Message, "retry immediately") {
		t.Errorf("message = %q, want retry-immediately hint", writeBody.Error.Message)
	}
}
