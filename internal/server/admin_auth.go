package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/upstream"
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
)

type adminAuth struct {
	key   [32]byte
	mu    sync.Mutex
	fails map[string]failEntry
}

type failEntry struct {
	count int
	until time.Time
}

func newAdminAuth() *adminAuth {
	a := &adminAuth{fails: make(map[string]failEntry)}
	_, _ = rand.Read(a.key[:])
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

func (a *adminAuth) setCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    a.cookieValue(time.Now().Add(adminCookieTTL)),
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   int(adminCookieTTL.Seconds()),
	})
}

func (a *adminAuth) allow(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
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

func (s *Server) dashboardAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		if cfg.AdminToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(adminCookieName); err == nil && s.adminAuth.valid(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/admin/login", http.StatusFound)
	})
}

func (s *Server) adminSensitive(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		// Sensitive routes (raw .env read/write, logs, mode switch, token
		// management) require a loopback client when the deployment is
		// effectively unauthenticated: ADMIN_TOKEN unset, or still the
		// factory default ("123456" since #188 — publicly known, so remote
		// access under it is anonymous-equivalent). Changing the password
		// (/admin/api/change-password requires the current credential) lifts
		// the restriction for remote operators.
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

func (s *Server) adminCSRF(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusForbidden)
					s.dash.RenderConfigResult(w, r, false, "Cross-origin request rejected.")
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
						s.dash.RenderConfigResult(w, r, false, "Cross-origin request rejected.")
						return
					}
				}
			}
			if sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); sfs != "" {
				if sfs != "same-origin" && sfs != "none" {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusForbidden)
					s.dash.RenderConfigResult(w, r, false, "Cross-origin request rejected.")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	}
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	if r.Method != http.MethodPost {
		// GET/HEAD: render the SPA login page. The Svelte form posts to this
		// same route; with ADMIN_TOKEN unset there is nothing to log in to.
		if cfg.AdminToken == "" {
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		s.dash.ServeSPA(w, r)
		return
	}
	if cfg.AdminToken == "" {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	ip := remoteHost(r)
	if r.Method == http.MethodPost {
		if !s.adminAuth.allow(ip) {
			// T15: audit the lockout rejection — attempts is the lockout
			// bound that was crossed; the submitted credential is never
			// logged.
			s.logger.Warn("admin login failed", "remote", ip, "attempts", maxLoginFails, "reason", "locked_out")
			s.dash.RenderLogin(w, r, "Too many failed attempts — try again in a minute.")
			return
		}
		token := r.FormValue("token")
		if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.AdminToken)) == 1 {
			s.adminAuth.clearFails(ip)
			// Secure only when the login arrived over an actual TLS
			// connection (direct HTTPS or a TLS-terminating reverse proxy
			// setting X-Forwarded-Proto). A Secure cookie over plain HTTP is
			// rejected by browsers, silently breaking remote login.
			s.adminAuth.setCookie(w, r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		s.adminAuth.recordFail(ip)
		attempts, locked := s.adminAuth.loginFailState(ip)
		if locked {
			attempts = maxLoginFails
		}
		// T15: audit a failed login — remote, running attempt count, and
		// reason only; the credential itself is never logged.
		s.logger.Warn("admin login failed", "remote", ip, "attempts", attempts, "reason", "invalid_token")
		s.dash.RenderLogin(w, r, "Invalid admin token.")
		return
	}
	s.dash.RenderLogin(w, r, "")
}

// handleAdminLogout clears the fb_admin session cookie (MaxAge=-1, same
// name/Path/SameSite as the login cookie) and answers: 302 → /admin/login
// on GET, JSON {"ok":true} on POST. It does NOT require a valid cookie —
// logging out an already-expired session must work — and it is not wrapped
// in adminSensitive because it exposes nothing.
func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	// Match the Secure flag with the current transport, same as login.
	// A clearing cookie without Secure cannot overwrite a Secure cookie
	// set during an HTTPS login, leaving the session alive.
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   -1,
	})
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

func (s *Server) handleAdminAuthStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"authenticated":          true,
		"is_default_admin_token": cfg.IsDefaultAdminToken(),
	})
}

func (s *Server) handleAdminChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req changePasswordRequest
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "invalid_request", 0)
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid request JSON", "invalid_request_error", "invalid_json", 0)
			return
		}
	} else {
		req.CurrentPassword = r.FormValue("current_password")
		req.NewPassword = r.FormValue("new_password")
	}

	req.CurrentPassword = strings.TrimSpace(req.CurrentPassword)
	req.NewPassword = strings.TrimSpace(req.NewPassword)

	cfg := s.cfg.Load()

	// Verify current password with constant-time comparison
	if subtle.ConstantTimeCompare([]byte(req.CurrentPassword), []byte(cfg.AdminToken)) != 1 {
		s.writeJSONError(w, http.StatusBadRequest, "Current password is incorrect.", "invalid_request_error", "invalid_credentials", 0)
		return
	}

	if len(req.NewPassword) < 6 {
		s.writeJSONError(w, http.StatusBadRequest, "New password must be at least 6 characters.", "invalid_request_error", "password_too_short", 0)
		return
	}

	if req.NewPassword == config.DefaultAdminToken {
		s.writeJSONError(w, http.StatusBadRequest, "New password cannot be the factory default password ('123456').", "invalid_request_error", "password_insecure", 0)
		return
	}

	// parseDotenv trims unquoted values at the first '#' and strips a
	// leading quote pair, while updateEnvKeys writes ADMIN_TOKEN raw. A
	// password outside this charset would write fine but reload mangled,
	// tripping the divergence guard below with a misleading "overridden by
	// the environment" error on every future attempt. Reject it before any
	// filesystem mutation instead.
	if strings.ContainsAny(req.NewPassword, "#\r\n") || req.NewPassword[0] == '"' || req.NewPassword[0] == '\'' {
		s.writeJSONError(w, http.StatusBadRequest,
			"New password must not contain '#', newline, or start with a quote character: it could not be stored losslessly in .env.",
			"invalid_request_error", "password_unsafe_for_env", 0)
		return
	}

	s.adminSaveMu.Lock()
	defer s.adminSaveMu.Unlock()

	oldBytes, oldErr := os.ReadFile(".env")
	_, err := updateEnvKeys([]envUpdate{{Key: "ADMIN_TOKEN", Value: req.NewPassword}})
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "Failed to update .env: "+err.Error(), "internal_error", "env_write_failed", 0)
		return
	}

	newCfg, err := config.Load(s.configPath)
	if err != nil {
		restoreEnvFile(oldBytes, oldErr)
		s.logger.Warn("admin change password reload failed; restored .env", "err", err)
		s.writeJSONError(w, http.StatusInternalServerError, "Failed to reload configuration: "+err.Error(), "internal_error", "reload_failed", 0)
		return
	}

	// Divergence guard (mirrors syncTokensAfterMutation): a real process
	// environment variable or -config JSON outranks ./.env, so the .env
	// write above may not move the effective credential. Answering ok:true
	// then would leave the old token (possibly the factory default) live
	// while telling the operator rotation succeeded.
	if newCfg.AdminToken != req.NewPassword {
		restoreEnvFile(oldBytes, oldErr)
		s.logger.Warn("admin change password shadowed by environment; restored .env")
		s.writeJSONError(w, http.StatusConflict,
			"ADMIN_TOKEN is overridden by the process environment or -config JSON — the .env write was rolled back and the running credential is unchanged; change ADMIN_TOKEN where it is actually set",
			"invalid_request_error", "admin_token_overridden", 0)
		return
	}

	s.cfg.Store(&newCfg)
	s.reg.SetConfig(&newCfg)
	s.pool.SetConfig(&newCfg)

	// Set updated session cookie
	s.adminAuth.setCookie(w, r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))

	s.logger.Info("admin password changed successfully", "remote", remoteHost(r))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"message": "Admin password updated successfully.",
	})
}
