// Headless OAuth login flow for the dashboard token wizard (#62) and the
// -refresh-token CLI mode (#66). Both drive the upstream /api/auth/cli/code
// + /api/auth/cli/status endpoints the official CLI uses, reusing the
// proxy's own transport/stealth wiring (a token-less Client built by
// NewForAuth). Port of:
//
//   - reference/freebuff-reverse/internal/channels/freebuff/account_login.go
//     (startGitHubLoginWithProfile + pollGitHubLogin), and
//   - reference/freebuff2api-chenjh/src/login.ts (device-code login).
//
// The protocol login (ProtocolGitHubLogin) additionally walks GitHub's own
// HTML forms with a cookie jar — password + TOTP per
// reference/freebuff-reverse .../github_protocol_login.go — before the same
// status poll, so the CLI can refresh a token non-interactively.
package upstream

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/upstream/login"
)

// loginCallTimeout bounds each login HTTP call (code request, status poll,
// protocol page walk).
const loginCallTimeout = 30 * time.Second

// loginPollInterval is how long between status polls (the upstream device
// code takes a human to complete; 5s matches the reference CLI default
// login-flow.ts pollLoginStatus intervalMs=5000, not the old 3s).
const loginPollInterval = 5 * time.Second

// NewForAuth builds a token-less client for the headless OAuth login flow
// (#62/#66). The transport/stealth/proxy wiring is identical to a pooled
// client (built through NewWithIndex with a placeholder token), but the
// token is zeroed and newRequest skips auth headers, so the login endpoints
// receive the official CLI login signature only.
func NewForAuth(cfg *config.Config) (*Client, error) {
	c, err := NewWithIndex("login-flow", 0, cfg)
	if err != nil {
		return nil, err
	}
	c.token = ""
	c.authOnly = true
	return c, nil
}

// authLoginRequest builds an /api/auth/cli/* request with the plain Bun
// fetch User-Agent (bunUserAgent): the real CLI's login flow goes through
// bare Bun fetch with no UA override (login-flow.ts request()), never the
// chat ai-sdk UA.
func (c *Client) authLoginRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("upstream: build %s %s: %w", method, path, err)
	}
	req.Header.Set("User-Agent", bunUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// CLILoginCode is the upstream response to POST /api/auth/cli/code: the
// one-time login URL plus the poll credentials the status endpoint needs.
type CLILoginCode struct {
	FingerprintID   string
	FingerprintHash string
	LoginURL        string
	ExpiresAt       time.Time
	// ExpiresAtRaw is the upstream expiresAt echoed verbatim on the status
	// poll: the protocol is epoch MILLISECONDS (reference
	// freebuff2api-chenjh/src/login.ts:41 "epoch ms", validated in ms at
	// :262), and the status backend compares it against Date.now() in ms.
	// Converting to seconds and re-encoding would make every code look
	// already-expired.
	ExpiresAtRaw int64
}

// CLILoginUser is the GitHub user metadata returned once the login
// completes.
type CLILoginUser struct {
	ID    string
	Name  string
	Email string
}

// CLILoginStatus is one poll of GET /api/auth/cli/status. Done is true once
// authToken is present (the browser login completed).
type CLILoginStatus struct {
	AuthToken string
	User      CLILoginUser
	Done      bool
}

// StartCLILogin begins the headless GitHub OAuth login with the stable
// machine-derived fingerprint (reference account_login.go startGitHubLoginWithProfile).
func (c *Client) StartCLILogin(ctx context.Context) (*CLILoginCode, error) {
	return c.StartCLILoginWithFingerprint(ctx, login.GenerateFingerprintID())
}

// StartCLILoginIsolated begins the login with a fresh, random "enhanced-"
// fingerprint (mirroring gen-freebuff-token.sh). Used by the dashboard login
// wizard so multiple accounts added to a pool are not correlated by a shared hardware identifier.
func (c *Client) StartCLILoginIsolated(ctx context.Context) (*CLILoginCode, error) {
	return c.StartCLILoginWithFingerprint(ctx, login.GenerateIsolatedFingerprintID())
}

// StartCLILoginWithFingerprint begins the login with an explicit fingerprintId.
func (c *Client) StartCLILoginWithFingerprint(ctx context.Context, fingerprintID string) (*CLILoginCode, error) {
	if fingerprintID == "" {
		fingerprintID = login.GenerateFingerprintID()
	}
	payload, _ := json.Marshal(map[string]any{"fingerprintId": fingerprintID})
	req, err := c.authLoginRequest(ctx, http.MethodPost, "/api/auth/cli/code", payload)
	if err != nil {
		return nil, err
	}
	resp, cancel, classErr := c.do(req, loginCallTimeout)
	if classErr != nil && resp == nil {
		return nil, fmt.Errorf("upstream: start login: %w", classErr)
	}
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("upstream: read login code response: %w", err)
	}
	if classErr != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream: start login failed: status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var decoded struct {
		FingerprintID   string `json:"fingerprintId"`
		FingerprintHash string `json:"fingerprintHash"`
		LoginURL        string `json:"loginUrl"`
		ExpiresAt       int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("upstream: decode login code: %w", err)
	}
	if decoded.FingerprintHash == "" || decoded.LoginURL == "" {
		return nil, fmt.Errorf("upstream: login code response missing fields")
	}
	if decoded.ExpiresAt <= 0 {
		decoded.ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
	}
	echoed := fingerprintID
	if decoded.FingerprintID != "" {
		echoed = decoded.FingerprintID
	}
	return &CLILoginCode{
		FingerprintID:   echoed,
		FingerprintHash: decoded.FingerprintHash,
		LoginURL:        decoded.LoginURL,
		ExpiresAt:       loginExpiresAt(decoded.ExpiresAt),
		ExpiresAtRaw:    decoded.ExpiresAt,
	}, nil
}

// loginExpiresAt converts the upstream expiresAt to a time.Time, tolerating
// ms vs s epochs (reference freebuffExpiresAtUnix).
func loginExpiresAt(raw int64) time.Time {
	if raw > 1_000_000_000_000 {
		raw /= 1000
	}
	return time.Unix(raw, 0)
}

// PollCLILogin polls GET /api/auth/cli/status for a started login.
// Pending — 401 while the device code is unclaimed, a transient 5xx, or a
// transport failure — returns Done=false WITHOUT error so callers keep
// polling until the 5-minute deadline, mirroring login-flow.ts
// pollLoginStatus (5xx + network errors are logged and retried; only the
// deadline / shouldContinue abort). A completed login returns the token
// and user metadata (reference pollGitHubLogin + login.ts pollLoginStatus).
func (c *Client) PollCLILogin(ctx context.Context, code *CLILoginCode) (*CLILoginStatus, error) {
	u, err := url.Parse(c.baseURL + "/api/auth/cli/status")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("fingerprintId", code.FingerprintID)
	q.Set("fingerprintHash", code.FingerprintHash)
	expiresRaw := code.ExpiresAtRaw
	if expiresRaw == 0 {
		expiresRaw = code.ExpiresAt.UnixMilli()
	}
	q.Set("expiresAt", strconv.FormatInt(expiresRaw, 10))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("upstream: build login status request: %w", err)
	}
	req.Header.Set("User-Agent", bunUserAgent)
	resp, cancel, classErr := c.do(req, loginCallTimeout)
	if classErr != nil && resp == nil {
		// Transient transport failure: login-flow.ts logs and keeps polling
		// through network errors — return pending so the caller retries
		// until its 5-minute deadline (#125).
		slog.Warn("login status poll: transient transport error, will retry", "err", classErr)
		return &CLILoginStatus{}, nil
	}
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if classErr != nil {
		// Classified >=400 response: a 5xx is transient upstream (warn +
		// pending); a 401 pending needs no warning. Both keep polling.
		if resp.StatusCode >= 500 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			slog.Warn("login status poll: transient upstream status, will retry", "status", resp.StatusCode, "body", truncate(string(raw), 200))
		}
		return &CLILoginStatus{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		// Pending: 401 while the device code is unclaimed (login.ts keeps
		// retrying exactly this).
		return &CLILoginStatus{}, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("upstream: read login status: %w", err)
	}
	var decoded struct {
		AuthToken string `json:"authToken"`
		User      struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Email     string `json:"email"`
			AuthToken string `json:"authToken"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("upstream: decode login status: %w", err)
	}
	token := strings.TrimSpace(decoded.AuthToken)
	if token == "" {
		token = strings.TrimSpace(decoded.User.AuthToken)
	}
	if token == "" {
		return &CLILoginStatus{}, nil
	}
	return &CLILoginStatus{
		AuthToken: token,
		User:      CLILoginUser{ID: decoded.User.ID, Name: decoded.User.Name, Email: decoded.User.Email},
		Done:      true,
	}, nil
}

// ProtocolGitHubLogin runs the reference GitHub password+TOTP protocol
// login (#66, github_protocol_login.go RunGitHubProtocolLoginInput) against
// a freshly started CLI login: walk the OAuth login URL with a cookie jar,
// submit the GitHub username/password form, answer the TOTP challenge, and
// follow the OAuth callback back to codebuff — then poll the CLI status for
// the resulting FreeBuff token (same PollCLILogin contract). now is a test
// seam for the TOTP window (nil = time.Now).
//
// Best-effort against GitHub's live HTML: every challenge class the
// reference recognizes (captcha, passkey, device verification, ...) is
// surfaced as an error message, never a panic. The status vocabulary is
// returned inside the error text so the CLI can tell the user what to do.
func (c *Client) ProtocolGitHubLogin(ctx context.Context, username, password, totpSecret string, now func() time.Time) (*CLILoginStatus, error) {
	if now == nil {
		now = time.Now
	}
	code, err := c.StartCLILogin(ctx)
	if err != nil {
		return nil, err
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Transport: c.http.Transport,
		Jar:       jar,
		// The protocol page walk (login-code -> authorize -> form -> TOTP ->
		// callback) must not hang on a stalled GitHub page: bound every call
		// (the caller's ctx only covers the whole flow, not
		// individual page loads).
		Timeout: loginCallTimeout,
		// GitHub's OAuth dance redirects many times; the reference allows 12.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 12 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	// 1. Load the login page so the jar picks up session cookies.
	if _, err := getWithUA(ctx, client, code.LoginURL, bunUserAgent); err != nil {
		return nil, fmt.Errorf("freebuff github protocol login: login page: %w", err)
	}

	// 2. Walk the OAuth authorize page: GET the sign-in URL (the login page
	// redirects there), find the login form, submit username+password.
	if authorizeURL := githubProtocolAuthorizeURL(code.LoginURL); authorizeURL != "" {
		if _, err := getWithUA(ctx, client, authorizeURL, bunUserAgent); err != nil {
			return nil, fmt.Errorf("freebuff github protocol login: authorize page: %w", err)
		}
	}

	// 3. The login page may live at the login URL itself (github.com/login)
	// or on the authorize flow; submit the password form wherever it is.
	// We track the most recent page body through the login form.
	loginResp, err := getWithUA(ctx, client, code.LoginURL, bunUserAgent)
	if err != nil {
		return nil, fmt.Errorf("freebuff github protocol login: login form: %w", err)
	}
	form, ok := githubProtocolFindLoginForm(loginResp.body)
	if !ok {
		return nil, fmt.Errorf("freebuff github protocol login: github login form not found (captcha, passkey, or a changed sign-in surface; open %s in a browser)", code.LoginURL)
	}
	form.Fields.Set("login", username)
	form.Fields.Set("password", password)
	postResp, err := submitForm(ctx, client, form, loginResp.finalURL, bunUserAgent)
	if err != nil {
		return nil, fmt.Errorf("freebuff github protocol login: password submit: %w", err)
	}

	// 4. TOTP challenge when GitHub asks for it (2FA enabled).
	if totpForm, ok := githubProtocolFindTOTPForm(postResp.body); ok || strings.Contains(strings.ToLower(postResp.finalURL), "/sessions/two-factor") {
		if !ok {
			return nil, fmt.Errorf("freebuff github protocol login: github two-factor form not found (device verification or trusted-device gate; open %s in a browser)", code.LoginURL)
		}
		code6, err := githubProtocolTOTPAt(totpSecret, now())
		if err != nil {
			return nil, fmt.Errorf("freebuff github protocol login: invalid totp secret: %w", err)
		}
		totpForm.Fields.Set("app_otp", code6)
		if totpForm.Fields.Get("otp") != "" {
			totpForm.Fields.Set("otp", code6)
		}
		postResp, err = submitForm(ctx, client, totpForm, postResp.finalURL, bunUserAgent)
		if err != nil {
			return nil, fmt.Errorf("freebuff github protocol login: totp submit: %w", err)
		}
	}

	// 5. Follow the OAuth callback meta-refresh / link back to codebuff.
	if callback := githubProtocolOAuthCallbackURL(postResp.body); callback != "" {
		target := callback
		if strings.HasPrefix(target, "/") {
			target = "https://github.com" + target
		}
		if _, err := getWithUA(ctx, client, target, bunUserAgent); err != nil {
			return nil, fmt.Errorf("freebuff github protocol login: oauth callback: %w", err)
		}
	}

	// 6. Poll the CLI status with the started code until the token lands.
	return c.pollForCompletion(ctx, code, 0)
}

// pollForCompletion polls PollCLILogin until Done, up to the 5-minute
// deadline. 5xx and transport failures are already transient inside
// PollCLILogin (pending, no error), so the only errors landing here are
// locally-built-request failures; the loop otherwise mirrors login-flow.ts
// (keep polling, abort only on deadline / ctx cancel).
func (c *Client) pollForCompletion(ctx context.Context, code *CLILoginCode, _ time.Duration) (*CLILoginStatus, error) {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("login timed out after 5m")
		}
		status, err := c.PollCLILogin(ctx, code)
		if err != nil {
			return nil, err
		}
		if status.Done {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(loginPollInterval):
		}
	}
}

func githubProtocolDecodeTOTPSecret(secret string) ([]byte, error) {
	normalized := strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(secret)))
	normalized = strings.TrimRight(normalized, "=")
	if normalized == "" {
		return nil, fmt.Errorf("empty totp secret")
	}
	if decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(normalized); err == nil {
		return decoded, nil
	}
	padded := normalized
	if rem := len(padded) % 8; rem != 0 {
		padded += strings.Repeat("=", 8-rem)
	}
	return base32.StdEncoding.DecodeString(padded)
}

// --- protocol HTTP helpers ---------------------------------------------------

// bodyResp carries a page body + the final URL after redirects.
type bodyResp struct {
	body     []byte
	finalURL string
}

// getWithUA GETs url with the login user agent; redirects are followed by
// the client (the jar keeps session cookies), the final URL is returned.
func getWithUA(ctx context.Context, client *http.Client, url, ua string) (*bodyResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return &bodyResp{body: body, finalURL: resp.Request.URL.String()}, nil
}

// submitForm POSTs a parsed form to base (the page the form came from),
// resolving a relative action, with the login user agent.
func submitForm(ctx context.Context, client *http.Client, form githubProtocolForm, base, ua string) (*bodyResp, error) {
	action := form.Action
	if action == "" {
		action = base
	} else if u, err := url.Parse(action); err == nil && !u.IsAbs() {
		if b, err := url.Parse(base); err == nil {
			action = b.ResolveReference(u).String()
		}
	} else if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, action, strings.NewReader(form.Fields.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return &bodyResp{body: body, finalURL: resp.Request.URL.String()}, nil
}
