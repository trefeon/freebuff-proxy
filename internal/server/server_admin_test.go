package server_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
)

func TestAdminReload(t *testing.T) {
	// Isolate cwd: handleReload runs config.Load("") which reads ./.env.
	// Without a temp dir the test would silently pick up a developer's .env
	// dropped into internal/server.
	t.Chdir(t.TempDir())
	mock0 := testutil.NewMock()
	defer mock0.Close()
	ts, _ := newTestServer(t, nil, mock0)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil, map[string]string{"Authorization": "Bearer " + config.DefaultAdminToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), `"status":"ok"`) {
		t.Errorf("reload response missing ok status: %s", data)
	}
}

// TestAdminReloadToken verifies ADMIN_TOKEN guards POST /admin/reload: 401
// without the bearer token, 200 with it; unset keeps the legacy open
// behavior.
func TestAdminReloadToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.AdminToken = "admin-secret" }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reload without token status = %d, want 401: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil, map[string]string{"Authorization": "Bearer wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reload with wrong token status = %d, want 401: %s", resp.StatusCode, data)
	}

	// The successful reload executes LAST: it swaps s.cfg for a fresh
	// config.Load("") (no ADMIN_TOKEN in the test environment), so nothing
	// after it may rely on the old gate.
	resp, data = doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil, map[string]string{"Authorization": "Bearer admin-secret"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reload with token status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), `"status":"ok"`) {
		t.Errorf("reload response missing ok status: %s", data)
	}

	// Unset: legacy behavior (open), still works.
	mockLegacy := testutil.NewMock()
	defer mockLegacy.Close()
	tsLegacy, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.AdminToken = "" }, mockLegacy)
	resp, data = doJSON(t, http.MethodPost, tsLegacy.URL+"/admin/reload", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reload without ADMIN_TOKEN status = %d, want 200 (legacy): %s", resp.StatusCode, data)
	}
}

func TestConcurrentReloadAndChat(t *testing.T) {
	// Isolate cwd: the reload workers run config.Load("") which reads
	// ./.env (see TestAdminReload).
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-c1", 1, `"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Hammer chat and models while handleReload swaps s.cfg. The local run
	// exercises the concurrent paths without panicking; the -race build in CI
	// is the real data-race gate.
	worker := func(method, url string, body []byte) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var reader io.Reader
				if body != nil {
					reader = bytes.NewReader(body)
				}
				req, err := http.NewRequest(method, url, reader)
				if err != nil {
					t.Errorf("worker request build: %v", err)
					return
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Errorf("worker request: %v", err)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}
	for i := 0; i < 8; i++ {
		worker(http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA))
	}
	for i := 0; i < 4; i++ {
		worker(http.MethodGet, ts.URL+"/v1/models", nil)
	}
	for i := 0; i < 20; i++ {
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil, map[string]string{"Authorization": "Bearer " + config.DefaultAdminToken})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("reload %d status = %d, want 200: %s", i, resp.StatusCode, data)
		}
	}
	close(stop)
	wg.Wait()
}

// TestAdminSensitiveOpenMode verifies that in open mode (ADMIN_TOKEN unset / optional),
// admin routes are accessible without 403.
func TestAdminSensitiveOpenMode(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "" }, mock)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/config", nil)
	req.Host = "127.0.0.1:3457"
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestAuth(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, []string{"sk-test"}, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	// No key.
	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key status = %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(string(data), "invalid_api_key") {
		t.Errorf("body missing invalid_api_key: %s", data)
	}

	// Wrong keys, both schemes.
	for _, h := range []map[string]string{
		{"Authorization": "Bearer wrong"},
		{"x-api-key": "wrong"},
	} {
		resp, _ := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), h)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("wrong key %v status = %d, want 401", h, resp.StatusCode)
		}
	}

	// x-api-key ok.
	resp, data = doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"x-api-key": "sk-test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("x-api-key status = %d, want 200: %s", resp.StatusCode, data)
	}

	// Bearer ok.
	resp, data = doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer sk-test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Bearer status = %d, want 200: %s", resp.StatusCode, data)
	}

	// healthz is exempt from auth.
	resp, _ = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200 (exempt)", resp.StatusCode)
	}

	// models requires auth too.
	resp, _ = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("models without key status = %d, want 401", resp.StatusCode)
	}

	// The rejected requests must never have reached the pool/upstream; the
	// two accepted chats share one session and one run (same model).
	if mock.SessionCreates != 1 || len(mock.StartedRuns) != 1 {
		t.Errorf("upstream contact = %d session creates / %d started runs, want 1/1 (auth gates before pool)",
			mock.SessionCreates, len(mock.StartedRuns))
	}
}

func TestAllTokensDead502(t *testing.T) {
	bad0 := testutil.NewMock()
	defer bad0.Close()
	bad0.AuthReject = true
	bad1 := testutil.NewMock()
	defer bad1.Close()
	bad1.AuthReject = true
	ts, _ := newTestServer(t, nil, bad0, bad1)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "upstream_unavailable") {
		t.Errorf("body missing upstream_unavailable: %s", data)
	}
}

// TestBearerCaseInsensitiveVariants verifies lowercase bearer and mixed-case BEARER
// work for API authentication, admin endpoints, and bridge token extraction.
func TestBearerCaseInsensitiveVariants(t *testing.T) {
	t.Run("API auth accepts case variations", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		ts, _ := newTestServer(t, []string{"sk-test"}, mock)
		chatURL := ts.URL + "/v1/chat/completions"

		for _, auth := range []string{
			"Bearer sk-test",
			"bearer sk-test",
			"BEARER sk-test",
			"bEaReR sk-test",
		} {
			resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": auth})
			if resp.StatusCode != http.StatusOK {
				t.Errorf("auth %q status = %d, want 200: %s", auth, resp.StatusCode, data)
			}
		}
	})

	t.Run("admin reload accepts case variations", func(t *testing.T) {
		for _, auth := range []string{
			"bearer admin-secret",
			"BEARER admin-secret",
		} {
			mock := testutil.NewMock()
			ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.AdminToken = "admin-secret" }, mock)
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil, map[string]string{"Authorization": auth})
			mock.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("admin auth %q status = %d, want 200: %s", auth, resp.StatusCode, data)
			}
		}
	})

	t.Run("bridge mode token extraction accepts case variations", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-b3", 1, `"choices":[{"index":0,"delta":{"content":"bridged"},"finish_reason":null}]`))
		ts, _ := newBridgeTestServer(t, mock)
		chatURL := ts.URL + "/v1/chat/completions"

		for _, auth := range []string{
			"bearer client-tok-lower",
			"BEARER client-tok-upper",
		} {
			resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": auth})
			if resp.StatusCode != http.StatusOK {
				t.Errorf("bridge auth %q status = %d, want 200: %s", auth, resp.StatusCode, data)
			}
		}
	})
}

// TestTokenLockUnlock drives the lock/unlock lifecycle: locking a token
// excludes it from Acquire (chat fails with no available tokens), and
// unlocking restores availability.
func TestTokenLockUnlock(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) {
		c.AdminToken = "secret"
	}, mock)

	// Login to get a session cookie for dashboard endpoints.
	cookie := loginCookie(t, ts, "secret")

	// Sanity: chat works before lock.
	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-lock chat status = %d, want 200", resp.StatusCode)
	}

	// Lock token 0.
	lockResp, lockBody := doDashboardPost(t, ts.URL+"/admin/tokens/0/lock", cookie)
	if lockResp.StatusCode != http.StatusOK {
		t.Fatalf("lock status = %d, want 200: %s", lockResp.StatusCode, lockBody)
	}
	if !strings.Contains(lockBody, "locked") {
		t.Errorf("lock response missing 'locked': %s", lockBody)
	}

	// Verify snapshot reports locked.
	snap := p.Snapshot()
	if len(snap) == 0 || !snap[0].Locked {
		t.Fatal("Snapshot().Locked = false after LockToken")
	}

	// Chat should fail: the only token is locked.
	resp, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("post-lock chat status = 200, want non-200")
	}

	// Unlock.
	unlockResp, unlockBody := doDashboardPost(t, ts.URL+"/admin/tokens/0/unlock-lock", cookie)
	if unlockResp.StatusCode != http.StatusOK {
		t.Fatalf("unlock status = %d, want 200: %s", unlockResp.StatusCode, unlockBody)
	}
	if !strings.Contains(unlockBody, "unlocked") {
		t.Errorf("unlock response missing 'unlocked': %s", unlockBody)
	}

	// Verify snapshot reports unlocked.
	snap = p.Snapshot()
	if len(snap) == 0 || snap[0].Locked {
		t.Fatal("Snapshot().Locked = true after UnlockLockToken")
	}

	// Chat should succeed again.
	resp, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-unlock chat status = %d, want 200", resp.StatusCode)
	}
}

// loginCookie logs into the dashboard and returns the session cookie string.
func loginCookie(t *testing.T, ts *httptest.Server, token string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/login", strings.NewReader("token="+token))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login issued %d cookies, want 1", len(cookies))
	}
	return cookies[0].Name + "=" + cookies[0].Value
}

// doDashboardPost issues a POST with the given cookie and returns status + body.
func doDashboardPost(t *testing.T, url, cookie string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(data)
}
