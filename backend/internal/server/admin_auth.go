package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/upstream"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type loginFlow struct {
	ID         string // short flow id shown to the client (fingerprint prefix)
	Code       *upstream.CLILoginCode
	Started    time.Time
	Done       bool
	Completing bool // one status poll is mid-completion (guards double-add)
	Token      string
	Error      string
	Index      int // pooled token index after AddToken (0 when bridge)
}

const loginFlowTTL = 10 * time.Minute
const (
	adminCookieName = "fb_admin"
	adminCookieTTL  = 24 * time.Hour

	// csrfCookieName is the double-submit CSRF cookie: the SPA reads
	// it out of document.cookie and echoes the value as the X-CSRF-Token
	// header on every state-changing request, so it MUST NOT be HttpOnly.
	csrfCookieName = "fb_csrf"

	// maxAdminTokenLen caps the admin credential accepted at login and as a
	// new password: longer values only burn compare time and .env
	// write space, and a whitespace-padded login token must never fail a
	// locked-out account when the change form trims the same input.
	maxAdminTokenLen = 256

	// loginGlobalFailMax is the process-wide failed-login budget per
	// loginGlobalWindow: a distributed brute force spread across
	// source IPs never trips the per-IP counter but still exhausts the
	// budget here.
	loginGlobalFailMax = 20
	loginGlobalWindow  = time.Minute
	// loginGlobalLockout is the first process-wide lockout duration; each
	// budget breach doubles it (exponential backoff) up to
	// loginGlobalLockoutMax.
	loginGlobalLockout    = 30 * time.Second
	loginGlobalLockoutMax = 5 * time.Minute
	// loginConcurrencyMax bounds concurrent login requests: the handler
	// reads an attacker-supplied body and contends on the auth mutex, so a
	// flood must not stack unbounded goroutines in the login path.
	loginConcurrencyMax = 8
)

type adminAuth struct {
	key   [32]byte
	mu    sync.Mutex
	fails map[string]failEntry

	// loginSlots is a counting semaphore bounding concurrent login
	// attempts; tryLogin acquires non-blocking and releaseLogin must be
	// called when the attempt completes.
	loginSlots chan struct{}

	// Process-wide failed-login budget: globalFails counts failures inside
	// the current globalWindow; crossing loginGlobalFailMax flips
	// globalUntil (per breach doubling, capped at loginGlobalLockoutMax).
	globalFails  int
	globalWindow time.Time
	globalUntil  time.Time
	globalLevel  int
}

type failEntry struct {
	count int
	until time.Time
}

func newAdminAuth() *adminAuth {
	a := &adminAuth{
		fails:      make(map[string]failEntry),
		loginSlots: make(chan struct{}, loginConcurrencyMax),
	}
	if _, err := rand.Read(a.key[:]); err != nil {
		// The session-cookie HMAC key must be uniformly random: with a
		// zero or partially filled key any expiry can be signed and admin
		// cookies forged. A RNG failure at boot is unrecoverable.
		panic("admin auth: crypto/rand failed to generate the session cookie key: " + err.Error())
	}
	return a
}

func (a *adminAuth) cookieValue(expiry time.Time) string {
	mac := hmac.New(sha256.New, a.key[:])
	_, _ = mac.Write([]byte(strconv.FormatInt(expiry.Unix(), 10)))
	return strconv.FormatInt(expiry.Unix(), 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (a *adminAuth) valid(value string) bool {
	dot := strings.IndexByte(value, '.')
	if dot < 0 {
		return false
	}
	exp, err := strconv.ParseInt(value[:dot], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return false
	}
	mac := hmac.New(sha256.New, a.key[:])
	_, _ = mac.Write([]byte(value[:dot]))
	got, err := hex.DecodeString(value[dot+1:])
	if err != nil || !hmac.Equal(got, mac.Sum(nil)) {
		return false
	}
	return true
}

const (
	maxLoginFails = 5
	loginLockout  = time.Minute
	loginFailsCap = 1024
)

// isSecureCookie dynamically determines whether session and CSRF cookies should
// carry the Secure flag. By default, it adapts to the connection protocol:
//   - When accessed over HTTPS (direct TLS or reverse-proxy X-Forwarded-Proto: https),
//     cookies carry Secure: true to protect session credentials from sniffing.
//   - When accessed over plain HTTP (e.g. self-hosted cloud VPS without TLS),
//     cookies carry Secure: false so browsers accept them without silent drops.
//
// Setting ADMIN_FORCE_SECURE_COOKIES=true forces Secure: true unconditionally.
func isSecureCookie(r *http.Request) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ADMIN_FORCE_SECURE_COOKIES")))
	if v == "true" || v == "1" || v == "yes" {
		return true
	}
	if _, content, exists, err := config.EnvFileInfo(); err == nil && exists {
		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") {
				continue
			}
			if eq := strings.IndexByte(line, '='); eq > 0 {
				k := strings.TrimSpace(line[:eq])
				val := strings.TrimSpace(line[eq+1:])
				if strings.EqualFold(k, "ADMIN_FORCE_SECURE_COOKIES") {
					val = strings.ToLower(strings.Trim(val, `"'`))
					if val == "true" || val == "1" || val == "yes" {
						return true
					}
				}
			}
		}
	}
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	return false
}

func (a *adminAuth) setCookie(w http.ResponseWriter, r ...*http.Request) {
	var req *http.Request
	if len(r) > 0 {
		req = r[0]
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Value: a.cookieValue(time.Now().Add(adminCookieTTL)), Path: "/admin", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isSecureCookie(req), MaxAge: int(adminCookieTTL.Seconds())})
}

func (a *adminAuth) allow(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Process-wide budget first: while the global lockout is active
	// every source IP is denied, including fresh ones.
	if !a.globalUntil.IsZero() && time.Now().Before(a.globalUntil) {
		return false
	}
	e, ok := a.fails[ip]
	if !ok {
		return true
	}
	if !e.until.IsZero() {
		if time.Now().Before(e.until) {
			return false
		}
		delete(a.fails, ip)
	}
	return true
}

func (a *adminAuth) recordFail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.fails[ip]
	e.count++
	if e.count >= maxLoginFails {
		e.until = time.Now().Add(loginLockout)
		e.count = 0
	}
	if _, exists := a.fails[ip]; !exists && len(a.fails) >= loginFailsCap {
		now := time.Now()
		for k, v := range a.fails {
			if now.After(v.until) {
				delete(a.fails, k)
			}
		}
		if len(a.fails) >= loginFailsCap {
			// No expired entries to reclaim — drop one lockout (map
			// iteration order is fine; the bound is what matters).
			for k := range a.fails {
				delete(a.fails, k)
				break
			}
		}
	}
	a.fails[ip] = e

	// Process-wide budget: a distributed brute force spread across
	// source IPs never trips the per-IP counter, so the global window
	// counts every failure. Crossing the budget locks the whole login
	// surface for a duration that doubles with each breach (exponential
	// backoff, capped at loginGlobalLockoutMax).
	now := time.Now()
	if a.globalWindow.IsZero() || now.Sub(a.globalWindow) >= loginGlobalWindow {
		a.globalWindow = now
		a.globalFails = 0
	}
	a.globalFails++
	if a.globalFails >= loginGlobalFailMax && (a.globalUntil.IsZero() || !now.Before(a.globalUntil)) {
		a.globalLevel++
		dur := loginGlobalLockout << (a.globalLevel - 1)
		if dur > loginGlobalLockoutMax || dur <= 0 {
			dur = loginGlobalLockoutMax
		}
		a.globalUntil = now.Add(dur)
	}
}

func (a *adminAuth) clearFails(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fails, ip)
}

func (a *adminAuth) loginFailState(ip string) (attempts int, locked bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.fails[ip]
	if !ok {
		return 0, false
	}
	return e.count, !e.until.IsZero() && time.Now().Before(e.until)
}

// tryLogin acquires one of the bounded concurrent-login slots.
// Returns false when the login surface is saturated; call releaseLogin
// exactly once when the attempt completes.
func (a *adminAuth) tryLogin() bool {
	select {
	case a.loginSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *adminAuth) releaseLogin() {
	<-a.loginSlots
}

func (a *adminHandlers) dashboardAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := a.cfgLoad()
		// Open mode (ADMIN_TOKEN unset) must not expose the dashboard read
		// tier to non-loopback clients: per-token quota/spend/standing and
		// routing metadata would be anonymously readable. Apply the exact
		// gate adminSensitive uses (RemoteAddr + Host loopback check; no
		// trusted-proxy header handling). Behavior change: remote open-mode
		// requests now get 403 instead of pass-through; the AuthNone routes
		// (login page, logout, static assets) stay reachable, and setting
		// ADMIN_TOKEN restores remote access.
		if cfg.AdminToken == "" &&
			(!isLoopbackAddr(r.RemoteAddr) || !isLoopbackHost(r.Host)) {
			http.Error(w, "forbidden: the dashboard is unauthenticated (ADMIN_TOKEN unset) and only reachable from loopback; set ADMIN_TOKEN to enable remote access", http.StatusForbidden)
			return
		}
		// The SPA reads fb_csrf (double-submit token) out of
		// document.cookie on page load; issue it on the page response when
		// missing so the first state-changing POST already carries the pair.
		a.setCSRFCookieIfAbsent(w, r)
		if cfg.AdminToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(adminCookieName); err == nil && a.adminAuth.valid(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/admin/login", http.StatusFound)
	})
}

func (a *adminHandlers) adminSensitive(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := a.cfgLoad()
		// Sensitive routes (raw .env read/write, logs, mode switch, token
		// management) require a loopback client when the deployment is
		// effectively unauthenticated: ADMIN_TOKEN unset, or still the
		// factory default ("123456" since #188 — publicly known, so remote
		// access under it is anonymous-equivalent).
		//
		// BOOTSTRAP EXEMPTION — POST /admin/api/change-password only. The
		// route's whole purpose is to lift this restriction by replacing the
		// factory password, so gating it by the factory password itself is a
		// catch-22 for remote operators (they cannot reach the very endpoint
		// that grants access). A remote POST that SUPPLIES the factory
		// current_password proves the caller knows the deployment's effective
		// credential — for a factory-password deployment that is the same
		// bar loopback meets (the credential is public; there is nothing
		// additional to leak). The request still passes the session-cookie
		// and CSRF gates; only the loopback restriction is waived, and only
		// while the effective credential IS the factory default.
		if r.Method == http.MethodPost && r.URL.Path == "/admin/api/change-password" &&
			cfg.IsDefaultAdminToken() {
			next.ServeHTTP(w, r)
			return
		}
		if (cfg.AdminToken == "" || cfg.IsDefaultAdminToken()) &&
			(!isLoopbackAddr(r.RemoteAddr) || !isLoopbackHost(r.Host)) {
			http.Error(w, "forbidden: sensitive dashboard routes require a loopback client until a custom admin password is set", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackAddr reports whether remoteAddr's host part is a loopback
// address (127.0.0.0/8 or ::1). Both "127.0.0.1:1234" and "[::1]:1234" forms
// are accepted, as is a port-less remoteAddr; anything else — attacker
// source IPs included — is not loopback.
func isLoopbackAddr(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isLoopbackHost reports whether an HTTP Host header names a loopback
// destination: a loopback IP (loopback port form, optional port) or the
// exact DNS name "localhost" (case-insensitive, optional trailing dot).
// Port stripping is best-effort: net.SplitHostPort handles "127.0.0.1:3457"
// and "[::1]:3457"; on failure the last ":port" is stripped only when one
// is present. An unbracketed IPv6 literal is never mangled into a loopback
// name by that fallback (the residue still has to parse as a loopback IP or
// equal "localhost"), so the safe direction — reject — holds for any
// malformed or attacker-supplied Host.
func isLoopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if idx := strings.LastIndex(host, ":"); idx > 0 {
		host = host[:idx]
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

// newCSRFToken mints the double-submit CSRF value: 32 bytes of
// crypto/rand, hex-encoded.
func newCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// csrfCookie builds the double-submit CSRF cookie. It is deliberately NOT
// HttpOnly: the SPA reads it from document.cookie and echoes the value as
// the X-CSRF-Token header on state-changing requests.
func csrfCookie(value string, r ...*http.Request) *http.Cookie {
	var req *http.Request
	if len(r) > 0 {
		req = r[0]
	}
	return &http.Cookie{
		Name:     csrfCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: false,
		SameSite: http.SameSiteStrictMode,
		Secure:   isSecureCookie(req),
	}
}

// setCSRFCookieIfAbsent issues the double-submit CSRF cookie when the
// request carries none, so the SPA can pick it up and start sending the
// matching X-CSRF-Token header on its next state-changing request.
func (a *adminHandlers) setCSRFCookieIfAbsent(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie(csrfCookieName); err != nil {
		if value, err := newCSRFToken(); err == nil {
			http.SetCookie(w, csrfCookie(value, r))
		}
	}
}

func (a *adminHandlers) adminCSRF(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusForbidden)
					a.dash.RenderConfigResult(w, r, false, "Cross-origin request rejected.")
					return
				}
				if !strings.EqualFold(u.Host, r.Host) {
					originH, originP, err1 := net.SplitHostPort(u.Host)
					reqH, reqP, err2 := net.SplitHostPort(r.Host)
					if err1 == nil && err2 == nil && originP == reqP && isLoopbackHost(originH) && isLoopbackHost(reqH) {
						// Allowed: localhost:3457 vs 127.0.0.1:3457 on same port
					} else {
						w.Header().Set("Content-Type", "text/html; charset=utf-8")
						w.WriteHeader(http.StatusForbidden)
						a.dash.RenderConfigResult(w, r, false, "Cross-origin request rejected.")
						return
					}
				}
			}
			if sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); sfs != "" {
				if sfs != "same-origin" && sfs != "none" {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusForbidden)
					a.dash.RenderConfigResult(w, r, false, "Cross-origin request rejected.")
					return
				}
			}
			// Double-submit token: when the fb_csrf cookie is
			// present, the X-CSRF-Token header must match it exactly.
			// /admin/login stays origin-checked only — the very first login
			// attempt may not hold a token yet. Requests without a cookie
			// keep the checks above as the fallback (a token cannot be
			// required before it has ever been issued); for those, issue
			// one on the response so the SPA picks it up for later calls.
			if r.URL.Path != "/admin/login" {
				if c, err := r.Cookie(csrfCookieName); err == nil && c.Value != "" {
					if subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.Header.Get("X-CSRF-Token"))) != 1 {
						w.Header().Set("Content-Type", "text/html; charset=utf-8")
						w.WriteHeader(http.StatusForbidden)
						a.dash.RenderConfigResult(w, r, false, "Invalid CSRF token.")
						return
					}
				} else {
					a.setCSRFCookieIfAbsent(w, r)
				}
			}
		}
		next.ServeHTTP(w, r)
	}
}

func (a *adminHandlers) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	cfg := a.cfgLoad()
	if r.Method != http.MethodPost {
		// GET/HEAD: render the SPA login page. The Svelte form posts to this
		// same route; with ADMIN_TOKEN unset there is nothing to log in to.
		if cfg.AdminToken == "" {
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		// The SPA reads fb_csrf (double-submit token) from
		// document.cookie; issue it on the login page response so the first
		// POST already carries the cookie.
		a.setCSRFCookieIfAbsent(w, r)
		a.dash.ServeSPA(w, r)
		return
	}
	if cfg.AdminToken == "" {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	// Bound the login body: the credential fits in a few hundred bytes and
	// an attacker-controlled form must not cost a full body read per
	// attempt.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if !a.adminAuth.tryLogin() {
		// The login surface is saturated: answer busy without consuming a
		// per-IP or global budget slot.
		a.logfunc().Warn("admin login rejected", "remote", remoteHost(r), "reason", "busy")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "Login service is busy — try again shortly."})
		return
	}
	defer a.adminAuth.releaseLogin()
	ip := remoteHost(r)
	// The top-of-function method guard already returned for every non-POST
	// request, so no inner method check is needed here (issue #222).
	if !a.adminAuth.allow(ip) {
		// T15: audit the lockout rejection — attempts is the lockout
		// bound that was crossed; the submitted credential is never
		// logged.
		a.logfunc().Warn("admin login failed", "remote", ip, "attempts", maxLoginFails, "reason", "locked_out")
		a.dash.RenderLogin(w, r, "Too many failed attempts — try again in a minute.")
		return
	}
	token := strings.TrimSpace(r.FormValue("token"))
	// Trim exactly like the change-password form: surrounding
	// whitespace must not silently burn a login attempt whose value the
	// password form would accept.
	if len(token) > maxAdminTokenLen {
		// Over-long credentials are invalid by construction; count them so
		// the budget cannot be probed for free, but never compare them.
		a.adminAuth.recordFail(ip)
		attempts, locked := a.adminAuth.loginFailState(ip)
		if locked {
			attempts = maxLoginFails
		}
		a.logfunc().Warn("admin login failed", "remote", ip, "attempts", attempts, "reason", "invalid_token")
		a.dash.RenderLogin(w, r, "Invalid admin token.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.AdminToken)) == 1 {
		a.adminAuth.clearFails(ip)
		a.adminAuth.setCookie(w, r)
		// Double-submit CSRF cookie: the SPA re-reads it from
		// document.cookie after the login response and echoes it on every
		// later state-changing request.
		a.setCSRFCookieIfAbsent(w, r)
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	a.adminAuth.recordFail(ip)
	attempts, locked := a.adminAuth.loginFailState(ip)
	if locked {
		attempts = maxLoginFails
	}
	// T15: audit a failed login — remote, running attempt count, and
	// reason only; the credential itself is never logged.
	a.logfunc().Warn("admin login failed", "remote", ip, "attempts", attempts, "reason", "invalid_token")
	a.dash.RenderLogin(w, r, "Invalid admin token.")
}

// handleAdminLogout clears the fb_admin session cookie (MaxAge=-1, same
// name/Path/SameSite as the login cookie) and answers: 302 → /admin/login
// on GET, JSON {"ok":true} on POST. It does NOT require a valid cookie —
// logging out an already-expired session must work — and it is not wrapped
// in adminSensitive because it exposes nothing.
func (a *adminHandlers) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	// The clearing cookie carries Secure matching the session cookie:
	// a non-Secure cookie cannot overwrite a Secure one.
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Value: "", Path: "/admin", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isSecureCookie(r), MaxAge: -1})
	if r.Method == http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		return
	}
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (a *adminHandlers) handleAdminAuthStatus(w http.ResponseWriter, r *http.Request) {
	cfg := a.cfgLoad()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"authenticated":          true,
		"is_default_admin_token": cfg.IsDefaultAdminToken(),
		"require_login":          cfg.RequireLogin(),
		"has_password":           cfg.AdminToken != "",
	})
}

func (a *adminHandlers) handleAdminChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req changePasswordRequest
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			a.dash.RenderResult(w, http.StatusBadRequest, false, "failed to read request body", "invalid_request")
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			a.dash.RenderResult(w, http.StatusBadRequest, false, "invalid request JSON", "invalid_json")
			return
		}
	} else {
		// Form path: cap the body before FormValue — ParseForm would
		// otherwise slurp the entire request into memory. Passwords fit in
		// a few hundred bytes; the JSON branch above keeps its own 64KB cap.
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		req.CurrentPassword = r.FormValue("current_password")
		req.NewPassword = r.FormValue("new_password")
	}

	req.CurrentPassword = strings.TrimSpace(req.CurrentPassword)
	req.NewPassword = strings.TrimSpace(req.NewPassword)

	cfg := a.cfgLoad()

	// Verify current password with constant-time comparison if a password is currently configured
	if cfg.AdminToken != "" {
		if subtle.ConstantTimeCompare([]byte(req.CurrentPassword), []byte(cfg.AdminToken)) != 1 {
			a.dash.RenderResult(w, http.StatusBadRequest, false, "Current password is incorrect.", "invalid_credentials")
			return
		}
	}

	if len(req.NewPassword) < 6 {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "New password must be at least 6 characters.", "password_too_short")
		return
	}
	if len(req.NewPassword) > maxAdminTokenLen {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "New password is too long (max "+strconv.Itoa(maxAdminTokenLen)+" characters).", "password_too_long")
		return
	}

	if req.NewPassword == config.DefaultAdminToken {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "New password cannot be the factory default password ('123456').", "password_insecure")
		return
	}

	// parseDotenv trims unquoted values at the first '#' and strips a
	// leading quote pair, while updateEnvKeys writes ADMIN_TOKEN raw. A
	// password outside this charset would write fine but reload mangled,
	// tripping the divergence guard below with a misleading "overridden by
	// the environment" error on every future attempt. Reject it before any
	// filesystem mutation instead.
	if strings.ContainsAny(req.NewPassword, "#\r\n,") || req.NewPassword[0] == '"' || req.NewPassword[0] == '\'' {
		a.dash.RenderResult(w, http.StatusBadRequest, false,
			"New password must not contain '#', ',', a newline, or start with a quote character: it could not be stored losslessly in .env.",
			"password_unsafe_for_env")
		return
	}

	a.adminSaveMu.Lock()
	defer a.adminSaveMu.Unlock()

	oldBytes, oldErr := os.ReadFile(config.EnvFileForWrite())
	_, err := updateEnvKeys([]config.EnvUpdate{
		{Key: "ADMIN_TOKEN", Value: req.NewPassword},
		{Key: "DASHBOARD_REQUIRE_LOGIN", Value: "true"},
	})
	if err != nil {
		a.dash.RenderResult(w, http.StatusInternalServerError, false, "Failed to update .env: "+err.Error(), "env_write_failed")
		return
	}

	newCfg, err := config.Load(a.configPath)
	if err != nil {
		restoreEnvFile(oldBytes, oldErr)
		a.logfunc().Warn("admin change password reload failed; restored .env", "err", err)
		a.dash.RenderResult(w, http.StatusInternalServerError, false, "Failed to reload configuration: "+err.Error(), "reload_failed")
		return
	}

	// Divergence guard (mirrors syncTokensAfterMutation): a real process
	// environment variable or -config JSON outranks ./.env, so the .env
	// write above may not move the effective credential. Answering ok:true
	// then would leave the old token (possibly the factory default) live
	// while telling the operator rotation succeeded.
	if newCfg.AdminToken != req.NewPassword {
		restoreEnvFile(oldBytes, oldErr)
		a.logfunc().Warn("admin change password shadowed by environment; restored .env")
		a.dash.RenderResult(w, http.StatusConflict, false,
			"ADMIN_TOKEN is overridden by the process environment or -config JSON — the .env write was rolled back and the running credential is unchanged; change ADMIN_TOKEN where it is actually set",
			"admin_token_overridden")
		return
	}

	a.applyReloadedConfig(&newCfg)

	// Set updated session cookie.
	a.adminAuth.setCookie(w, r)

	a.logfunc().Info("admin password changed successfully", "remote", remoteHost(r))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"message": "Admin password updated successfully.",
	})
}

func (a *adminHandlers) handleAdminRequireLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RequireLogin *bool `json:"require_login"`
		Enabled      *bool `json:"enabled"`
	}
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
		if err != nil {
			a.dash.RenderResult(w, http.StatusBadRequest, false, "failed to read request body", "invalid_request")
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			a.dash.RenderResult(w, http.StatusBadRequest, false, "invalid request JSON", "invalid_json")
			return
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		val := r.FormValue("require_login")
		if val == "" {
			val = r.FormValue("enabled")
		}
		if val != "" {
			b := val == "true" || val == "1" || val == "on" || val == "yes"
			req.RequireLogin = &b
		}
	}

	target := true
	if req.RequireLogin != nil {
		target = *req.RequireLogin
	} else if req.Enabled != nil {
		target = *req.Enabled
	}

	// Security restriction: open mode is loopback-only. Disabling login from
	// a remote client would immediately lock out remote access (403).
	if !target && (!isLoopbackAddr(r.RemoteAddr) || !isLoopbackHost(r.Host)) {
		a.dash.RenderResult(w, http.StatusBadRequest, false,
			"Cannot disable login requirement from a remote client: open mode is restricted to loopback clients only, which would immediately lock out remote access.",
			"remote_open_mode_forbidden")
		return
	}

	valStr := "true"
	if !target {
		valStr = "false"
	}

	a.adminSaveMu.Lock()
	defer a.adminSaveMu.Unlock()

	oldBytes, oldErr := os.ReadFile(config.EnvFileForWrite())
	_, err := updateEnvKeys([]config.EnvUpdate{
		{Key: "DASHBOARD_REQUIRE_LOGIN", Value: valStr},
	})
	if err != nil {
		a.dash.RenderResult(w, http.StatusInternalServerError, false, "Failed to update .env: "+err.Error(), "env_write_failed")
		return
	}

	newCfg, err := config.Load(a.configPath)
	if err != nil {
		restoreEnvFile(oldBytes, oldErr)
		a.logfunc().Warn("admin require login reload failed; restored .env", "err", err)
		a.dash.RenderResult(w, http.StatusInternalServerError, false, "Failed to reload configuration: "+err.Error(), "reload_failed")
		return
	}

	if newCfg.RequireLogin() != target {
		restoreEnvFile(oldBytes, oldErr)
		a.logfunc().Warn("admin require login shadowed by environment; restored .env")
		a.dash.RenderResult(w, http.StatusConflict, false,
			"DASHBOARD_REQUIRE_LOGIN is overridden by the process environment or -config JSON — the .env write was rolled back and the running credential is unchanged",
			"require_login_overridden")
		return
	}

	a.applyReloadedConfig(&newCfg)

	msg := "Dashboard login requirement enabled."
	if !target {
		msg = "Dashboard login requirement disabled (open mode on loopback)."
	}

	a.logfunc().Info("admin require login updated", "require_login", newCfg.RequireLogin(), "remote", remoteHost(r))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":            true,
		"require_login": newCfg.RequireLogin(),
		"message":       msg,
	})
}
