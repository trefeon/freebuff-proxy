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
	"runtime"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/upstream"
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

func TestAdminCookieSecureFlag(t *testing.T) {
	a := newAdminAuth()
	rec := httptest.NewRecorder()
	a.setCookie(rec, true)
	c := rec.Result().Cookies()[0]
	if !c.Secure {
		t.Error("cookie Secure flag not set when requested")
	}
	rec = httptest.NewRecorder()
	a.setCookie(rec, false)
	c = rec.Result().Cookies()[0]
	if c.Secure {
		t.Error("cookie Secure flag set for plain-HTTP loopback")
	}
}

// TestAdminCookieExpiredRedirects pins the expired-but-valid-HMAC cookie
// path: a correctly signed cookie whose expiry is in the past must fail
// validation and redirect to login, while the same signing with a future
// expiry is accepted — proving the expiry check, not the HMAC, is the gate.
func TestAdminCookieExpiredRedirects(t *testing.T) {
	s := &Server{adminAuth: newAdminAuth(), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.cfg.Store(&config.Config{AdminToken: "secret"})
	h := s.dashboardAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
func errorResponse(t *testing.T, err error) (status int, body struct {
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
	s.writeError(w, r, err)
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("writeError response is not JSON: %v: %s", err, w.Body.Bytes())
	}
	return w.Code, body
}

// TestWriteErrorNewMappings pins the self-healing error matrix additions:
// country block → 403, free-mode CLI gate → 403, credits → 402 (upstream body
// passed through verbatim), upstream deadline → 504.
func TestWriteErrorNewMappings(t *testing.T) {
	t.Run("country blocked 403", func(t *testing.T) {
		err := &upstream.CountryBlockedError{CountryCode: "CN", CountryBlockReason: "region_restricted", IpPrivacySignals: []string{"vpn"}}
		status, body := errorResponse(t, err)
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
		status, body := errorResponse(t, err)
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
		status, body := errorResponse(t, err)
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

	t.Run("upstream deadline 504", func(t *testing.T) {
		err := fmt.Errorf("chat: %w", context.DeadlineExceeded)
		status, body := errorResponse(t, err)
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
	status, body := errorResponse(t, &upstream.BanError{ResumesAt: time.Now().Add(time.Hour), Body: `{"status":"banned"}`})
	if status != http.StatusForbidden || body.Error.Code != "account_banned" {
		t.Errorf("ban: status=%d code=%q, want 403 account_banned", status, body.Error.Code)
	}

	status, body = errorResponse(t, &upstream.RateLimitError{RetryAfter: time.Minute})
	if status != http.StatusTooManyRequests || body.Error.Code != "rate_limited" {
		t.Errorf("rate limit: status=%d code=%q, want 429 rate_limited", status, body.Error.Code)
	}

	status, body = errorResponse(t, &upstream.WaitingRoomError{RetryAfter: time.Minute})
	if status != http.StatusServiceUnavailable || body.Error.Code != "waiting_room_queued" {
		t.Errorf("waiting room: status=%d code=%q, want 503 waiting_room_queued", status, body.Error.Code)
	}
}

// TestRestoreEnvFileUnreadable pins the mode-switch rollback guard: when the
// previous .env existed but was unreadable (oldErr not os.ErrNotExist), the
// rollback must NOT delete the file — removing it would destroy an operator's
// present-but-unreadable .env (regression for the P3 finding). POSIX-only:
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
	if err := os.WriteFile(".env", []byte("SAFE_MODE=true\r\nAUTH_TOKENS=tok-a\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := updateEnvKeys([]envUpdate{
		{Key: "AUTH_TOKENS", Value: ""},
		{Key: "HYBRID_MODE", Value: "false"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	want := "SAFE_MODE=true\r\nAUTH_TOKENS=\r\nHYBRID_MODE=false\r\n"
	if string(got) != want {
		t.Errorf(".env after update = %q, want %q", got, want)
	}
	assertNoTmpFiles(t, ".", ".env")

	// Flip HYBRID_MODE back to true: in-place replace, no duplicate line.
	if _, err := updateEnvKeys([]envUpdate{{Key: "HYBRID_MODE", Value: "true"}}); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got), "HYBRID_MODE=") != 1 || !strings.Contains(string(got), "HYBRID_MODE=true") {
		t.Errorf(".env after flip = %q, want single HYBRID_MODE=true line", got)
	}
}

// writeFileAtomic must atomically replace an existing file and clean up its
// temp file, on every platform (no unconditional pre-remove on Windows).
func TestWriteFileAtomicReplacesAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("NEW\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW\n" {
		t.Errorf("content after write = %q, want %q", got, "NEW\n")
	}
	assertNoTmpFiles(t, dir, ".env")
}

// On failure the target must be left exactly as it was and the temp file
// cleaned up. A non-empty directory cannot be replaced by a rename on any
// platform, so it doubles as a deterministic failure injection.
func TestWriteFileAtomicFailurePreservesTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(path, "keep.txt")
	if err := os.WriteFile(kept, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("NEW\n")); err == nil {
		t.Fatal("writeFileAtomic over a non-empty directory succeeded, want error")
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		t.Errorf("target dir missing or not a dir after failed write: %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("target content lost after failed write: %v", err)
	}
	assertNoTmpFiles(t, dir, ".env")
}

// ── Wave 1 issue tests (#81, #82, #76) ───────────────────────────────────

// TestWriteErrorIpCappedAndSessionLimit verifies the writeError mappings for
// the wave-1 typed errors: ip_capped surfaces 429 with code "ip_capped"
// (never the quota "rate_limited"), and session_limit_reached surfaces 409
// with its code (never session-invalid).
func TestWriteErrorIpCappedAndSessionLimit(t *testing.T) {
	t.Run("ip capped 429", func(t *testing.T) {
		err := &upstream.IpCappedError{ActiveUsersForIP: 5, Limit: 4, RetryAfter: 45 * time.Second, Body: `{"status":"ip_capped"}`}
		status, body := errorResponse(t, err)
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
		status, body := errorResponse(t, err)
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

// TestQuotaSummaryTierAndGlmPromo verifies #76: quotaSummary surfaces the
// account tier and glmPromo from a probe response carrying the unused rate
// limits, and still returns "" when the response has no quota data.
func TestQuotaSummaryTierAndGlmPromo(t *testing.T) {
	if got := quotaSummary(nil); got != "" {
		t.Errorf("quotaSummary(nil) = %q, want empty", got)
	}
	if got := quotaSummary(&upstream.SessionState{}); got != "" {
		t.Errorf("quotaSummary(no data) = %q, want empty", got)
	}
	st := &upstream.SessionState{
		AccessTier: "limited",
		GlmPromo:   `{"dailySessions":2,"endsAt":"2026-08-20T07:00:00.000Z"}`,
		RateLimitsByModel: map[string]upstream.ModelQuota{
			"deepseek/deepseek-v4-flash": {Model: "deepseek/deepseek-v4-flash", Limit: 6, RecentCount: 2, Period: "pacific_day", ResetAt: time.Date(2026, 8, 18, 7, 0, 0, 0, time.UTC)},
		},
	}
	got := quotaSummary(st)
	for _, want := range []string{"tier limited", "deepseek/deepseek-v4-flash 6/2 pacific_day", "glmPromo", "dailySessions"} {
		if !strings.Contains(got, want) {
			t.Errorf("quotaSummary = %q, missing %q", got, want)
		}
	}
}

// TestWriteErrorModelIPLimited pins the Issue #74 P2 writeError mapping:
// *upstream.LimitedIpError → 409, code model_ip_limited, Retry-After = ceil
// seconds of lie.RetryAfter (only when > 0 — the body window is surfaced but
// never sets the unfit registry TTL).
func TestWriteErrorModelIPLimited(t *testing.T) {
	err := &upstream.LimitedIpError{
		RetryAfter: 5 * time.Minute,
		Body:       `{"status":"session_model_mismatch","message":"model z-ai/glm-5.2 is limited on this IP"}`,
	}
	status, body := errorResponse(t, err)
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
	s.writeError(w, r, err)
	if got := w.Header().Get("Retry-After"); got != "300" {
		t.Errorf("Retry-After = %q, want 300", got)
	}

	// A zero RetryAfter must not emit the header.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.writeError(w2, r2, &upstream.LimitedIpError{Body: "no window"})
	if got := w2.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After with zero RetryAfter = %q, want empty", got)
	}
}
