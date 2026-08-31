package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAdminChangePasswordRemoteLiftsFactoryGate pins the remote bootstrap
// exemption: under the factory password a remote operator is 403'd on every
// sensitive route, but POST /admin/api/change-password must stay reachable —
// otherwise the route that lifts the gate is itself gated (catch-22) and a
// fresh VPS deployment is permanently write-locked from afar. Session cookie,
// CSRF pair and the CURRENT credential are still required; only the loopback
// restriction is waived, and only while the effective credential IS the
// factory default.
func TestAdminChangePasswordRemoteLiftsFactoryGate(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=123456\n", nil)
	h := s.Handler()

	remote := func(method, path, body, contentType, cookies, csrf string) *http.Request {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		req.RemoteAddr = "198.51.100.7:1234"
		req.Host = "proxy.example.com"
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if cookies != "" {
			req.Header.Set("Cookie", cookies)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		return req
	}
	cookiesOf := func(rec *httptest.ResponseRecorder, name string) string {
		for _, c := range rec.Result().Cookies() {
			if c.Name == name {
				return c.Value
			}
		}
		return ""
	}

	// 1. dashboardAuth issues the fb_csrf cookie on the first page hit.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, remote(http.MethodGet, "/admin/", "", "", "", ""))
	csrf := cookiesOf(rec, csrfCookieName)
	if csrf == "" {
		t.Fatalf("no %s cookie issued on dashboard hit (status %d)", csrfCookieName, rec.Code)
	}

	// 2. Remote login with the factory password.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, remote(http.MethodPost, "/admin/login", "token=123456",
		"application/x-www-form-urlencoded", "", ""))
	admin := cookiesOf(rec, adminCookieName)
	if admin == "" {
		t.Fatalf("remote login under factory password failed: status %d", rec.Code)
	}
	session := csrfCookieName + "=" + csrf + "; " + adminCookieName + "=" + admin

	// 3. Other sensitive routes stay gated remotely.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, remote(http.MethodGet, "/admin/api/config", "", "", session, csrf))
	if rec.Code != http.StatusForbidden {
		t.Errorf("remote GET /admin/api/config = %d, want 403 (gate must hold)", rec.Code)
	}

	// 4. The bootstrap exemption: change-password IS reachable remotely.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, remote(http.MethodPost, "/admin/api/change-password",
		`{"current_password":"123456","new_password":"RemoteLift2026"}`,
		"application/json", session, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("remote change-password = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// 5. A fresh remote session with the NEW password unlocks sensitive routes.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, remote(http.MethodPost, "/admin/login", "token=RemoteLift2026",
		"application/x-www-form-urlencoded", "", ""))
	admin2 := cookiesOf(rec, adminCookieName)
	if admin2 == "" {
		t.Fatalf("remote login with the new password failed: status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, remote(http.MethodGet, "/admin/api/config", "", "",
		csrfCookieName+"="+csrf+"; "+adminCookieName+"="+admin2, csrf))
	if rec.Code != http.StatusOK {
		t.Errorf("remote GET /admin/api/config after password change = %d, want 200: %s",
			rec.Code, rec.Body.String())
	}
}

// TestAdminChangePasswordRemoteStillValidates pins that the exemption does not
// weaken the handler: a remote change-password with a WRONG current credential
// must fail, and the loopback gate must still 403 remote sensitive reads under
// the factory password afterwards (no drift into open mode).
func TestAdminChangePasswordRemoteStillValidates(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=123456\n", nil)
	h := s.Handler()

	remote := func(method, path, body, contentType, cookies, csrf string) *http.Request {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "198.51.100.7:1234"
		req.Host = "proxy.example.com"
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if cookies != "" {
			req.Header.Set("Cookie", cookies)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		return req
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, remote(http.MethodGet, "/admin/", "", "", "", ""))
	csrf := ""
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookieName {
			csrf = c.Value
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, remote(http.MethodPost, "/admin/login", "token=123456",
		"application/x-www-form-urlencoded", "", ""))
	admin := ""
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminCookieName {
			admin = c.Value
		}
	}
	session := csrfCookieName + "=" + csrf + "; " + adminCookieName + "=" + admin

	// Wrong current credential → rejected, password unchanged.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, remote(http.MethodPost, "/admin/api/change-password",
		`{"current_password":"wrong","new_password":"ShouldNotLand1"}`,
		"application/json", session, csrf))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("remote change-password with wrong credential = %d, want 400", rec.Code)
	}

	// Factory password still live: sensitive routes remain gated remotely.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, remote(http.MethodGet, "/admin/api/config", "", "", session, csrf))
	if rec.Code != http.StatusForbidden {
		t.Errorf("remote GET /admin/api/config after failed change = %d, want 403", rec.Code)
	}
}
