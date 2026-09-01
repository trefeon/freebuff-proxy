package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/updatecheck"
	"freebuff-proxy/backend/internal/upstream"
)

// --- Open Dashboard Auth Optional -------------------------------------------

// TestDashboardAuthOptional verifies open-mode dashboard access when
// ADMIN_TOKEN is unset: loopback clients are served, remote clients are
// refused with 403 by the open-mode gate in dashboardAuth (setting
// ADMIN_TOKEN restores remote access).
func TestDashboardAuthOptional(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	srv := newServer(t, mock, nil)
	for _, host := range []string{"127.0.0.1:3457", "localhost:3457"} {
		req := httptest.NewRequest(http.MethodGet, "/admin", nil)
		req.RemoteAddr = "127.0.0.1:5555"
		req.Host = host
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("loopback host %s: status = %d, want 200", host, rec.Code)
		}
	}
	// Open mode is loopback-only: a remote peer is refused, since the
	// dashboard read tier must not be anonymously readable from off-box.
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Host = "192.168.1.50:3457"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("remote host 192.168.1.50:3457: status = %d, want 403", rec.Code)
	}
}

// TestDashboardBannerHiddenWithAdminToken verifies the banner never shows
// when ADMIN_TOKEN is set.
func TestDashboardBannerHiddenWithAdminToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	srv := newServerCfg(t, mock, func(c *config.Config) { c.AdminToken = "secret" })
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Host = "192.168.1.50:3457"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	// Without the cookie the request redirects to login (still no banner on
	// the login page).
	if strings.Contains(rec.Body.String(), "Dashboard is open") {
		t.Error("banner shown with ADMIN_TOKEN set")
	}
}

// --- #45: playground ---------------------------------------------------------

func TestPlaygroundPageRenders(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	srv := newServer(t, mock, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/playground", nil)
	// Loopback peer + host: the open-mode dashboard gate requires both.
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	page := rec.Body.String()
	if !strings.Contains(page, "freebuff-proxy") && !strings.Contains(page, "admin") {
		t.Error("playground page missing SPA content")
	}
}

// TestPlaygroundChatStreams verifies the playground chat endpoint streams
// the real chat pipeline (SSE body) without an API key.
func TestPlaygroundChatStreams(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	srv := newServer(t, mock, nil)
	body := `{"model":"deepseek/deepseek-v4-flash","prompt":"ping","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/admin/playground/chat", strings.NewReader(body))
	req.Host = "127.0.0.1:3457"
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "data:") {
		t.Error("no SSE data in playground response")
	}
}

// --- #62: login wizard endpoints ---------------------------------------------

func TestLoginWizardFlow(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Wire a real auth client through the option so the wizard works.
	srv := newServerCfg(t, mock, nil, func(s *Server) {
		auth, err := upstream.NewForAuth(&config.Config{UpstreamBaseURL: mock.URL()})
		if err != nil {
			t.Fatal(err)
		}
		s.authClient = auth
	})

	// Start.
	req := httptest.NewRequest(http.MethodPost, "/admin/login/start", nil)
	req.Host = "127.0.0.1:3457"
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", rec.Code, rec.Body.String())
	}
	var startResp struct {
		FlowID   string `json:"flow_id"`
		LoginURL string `json:"login_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &startResp); err != nil {
		t.Fatal(err)
	}
	if startResp.FlowID == "" || startResp.LoginURL == "" {
		t.Fatalf("start response missing flow_id/login_url: %s", rec.Body.String())
	}

	// Poll: pending (mock serves 401 until AuthCLIStatusBody is set).
	poll := func() string {
		req := httptest.NewRequest(http.MethodGet, "/admin/login/status?fingerprint=enhanced-test", nil)
		req.Host = "127.0.0.1:3457"
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status code = %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Status
	}
	if got := poll(); got != "pending" {
		t.Errorf("first poll = %q, want pending", got)
	}

	// Complete the login upstream; the next poll must add the token.
	mock.AuthCLIStatusBody = `{"authToken":"cb_wizard","user":{"id":"gh-9","name":"Wiz","email":"wiz@example.com"}}`
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := poll(); got == "completed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if srv.pool.TokenCount() != 2 {
		t.Errorf("pool tokens = %d, want 2 (1 fixed + wizard token)", srv.pool.TokenCount())
	}
}

// --- #100: queue-time model fallback -----------------------------------------

// TestChatFallbackAfterWaitingRoom verifies the acquire-time fallback:
// with FALLBACK_AFTER_MS + FALLBACK_MODEL configured and the waiting room
// RetryAfter >= the threshold, the request is re-routed to the fallback
// model and the X-FreeBuff-Fallback-Model header surfaces the switch.
func TestChatFallbackAfterWaitingRoom(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "queued"
	mock.SessionSequence = []string{"queued", "active"} // first create queues; the fallback model's create succeeds
	mock.EstimatedWaitMs = 20000                        // > FALLBACK_AFTER_MS (10s)
	srv := newServerCfg(t, mock, func(c *config.Config) {
		c.FallbackAfter = 10 * time.Second
		c.FallbackModels = map[string]string{"openai/gpt-5.6-luna": "deepseek/deepseek-v4-flash"}
	})
	body := `{"model":"openai/gpt-5.6-luna","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fallback served): %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-FreeBuff-Fallback-Model"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("X-FreeBuff-Fallback-Model = %q, want deepseek/deepseek-v4-flash", got)
	}
}

// TestChatNoFallbackBelowThreshold verifies a short waiting room (below
// FALLBACK_AFTER_MS) surfaces 503 waiting_room_queued as usual.
func TestChatNoFallbackBelowThreshold(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "queued"
	mock.EstimatedWaitMs = 1000 // < FALLBACK_AFTER_MS
	srv := newServerCfg(t, mock, func(c *config.Config) {
		c.FallbackAfter = 10 * time.Second
		c.FallbackModels = map[string]string{"openai/gpt-5.6-luna": "deepseek/deepseek-v4-flash"}
	})
	body := `{"model":"openai/gpt-5.6-luna","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (wait below threshold)", rec.Code)
	}
	if got := rec.Header().Get("X-FreeBuff-Fallback-Model"); got != "" {
		t.Errorf("fallback header set without fallback: %q", got)
	}
}

// --- #50b: update badge ------------------------------------------------------

func TestUpdateBadgeRendered(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var hits atomic.Int64
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer gh.Close()
	tr := &rewriteTransport{target: gh.URL}
	checker := updatecheck.New(updatecheck.DefaultRepo, &http.Client{Transport: tr})
	srv := newServerCfg(t, mock, nil, func(s *Server) {
		s.version = "v0.9.3"
		s.updates = checker
	})
	req := httptest.NewRequest(http.MethodGet, "/admin/api/version", nil)
	// Loopback peer: the open-mode dashboard gate requires a loopback
	// RemoteAddr in addition to the loopback Host.
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	t.Logf("hits=%d body=%s", hits.Load(), body)
	if !strings.Contains(body, `"has_update":true`) {
		t.Errorf("version api missing has_update:true in: %s", body)
	}
	if !strings.Contains(body, `"latest_version":"v9.9.9"`) {
		t.Errorf("version api missing latest_version v9.9.9 in: %s", body)
	}
	if hits.Load() == 0 {
		t.Error("update checker never queried")
	}
}

// rewriteTransport sends every request to target (tests must not contact
// api.github.com).
type rewriteTransport struct {
	target string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(t.target, "http://")
	return http.DefaultTransport.RoundTrip(clone)
}

// newServer builds a test server over one mock token with a config mutation
// and optional server options; returns the raw *Server for pool/option
// assertions.
func newServer(t *testing.T, mock *testutil.MockUpstream, mut func(*config.Config)) *Server {
	t.Helper()
	return newServerOpts(t, mock, mut)
}

func newServerCfg(t *testing.T, mock *testutil.MockUpstream, mut func(*config.Config), opts ...func(*Server)) *Server {
	t.Helper()
	return newServerOpts(t, mock, mut, opts...)
}

func newServerOpts(t *testing.T, mock *testutil.MockUpstream, mut func(*config.Config), opts ...func(*Server)) *Server {
	t.Helper()
	srv, _ := newTestServerStack(t, nil, []*testutil.MockUpstream{mock}, mut, nil, nil, opts...)
	return srv
}
