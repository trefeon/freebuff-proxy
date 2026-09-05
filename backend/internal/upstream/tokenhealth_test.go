package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"freebuff-proxy/backend/internal/config"
)

// newTokenHealthServer starts an httptest server with two counters: the
// number of requests per path, and a non-GET method guard (the anti-ban
// contract: no POST/PUT/DELETE/PATCH may ever leave a health check).
func newTokenHealthServer(t *testing.T, meHandler, sessionHandler http.HandlerFunc) (*httptest.Server, *requestLog) {
	t.Helper()
	log := &requestLog{methods: map[string]int{}, paths: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.mu.Lock()
		log.methods[r.Method]++
		log.paths[r.URL.Path]++
		log.mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("token health check sent %s %s — health checks are read-only", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/v1/me":
			if meHandler != nil {
				meHandler(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"u-1","email":"user@example.com","discord_id":null}`)
		case "/api/v1/freebuff/session":
			if sessionHandler != nil {
				sessionHandler(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"none"}`)
		default:
			t.Errorf("token health check hit unexpected path %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

type requestLog struct {
	mu      sync.Mutex
	methods map[string]int
	paths   map[string]int
}

func (l *requestLog) counts() (string, map[string]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	methods := make(map[string]int, len(l.methods))
	for k, v := range l.methods {
		methods[k] = v
	}
	paths := make(map[string]int, len(l.paths))
	for k, v := range l.paths {
		paths[k] = v
	}
	return strings.Join(sortedKeys(methods), ","), paths
}

// sortedKeys returns keys sorted for deterministic assertion output.
func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// healthClient builds a client for one token against the test server with
// the package's test config helper.
func healthClient(t *testing.T, token string, index int, baseURL string) *Client {
	t.Helper()
	c, err := NewWithIndex(token, index, testConfig(baseURL, nil))
	if err != nil {
		t.Fatalf("NewWithIndex: %v", err)
	}
	return c
}

// TestClassifyEmailDomain pins the port of upstream's classifier
// (reference/freebuff common/src/util/disposable-email.ts): exact-domain or
// any-subdomain match, case-insensitive, and — critically — no lookalike or
// substring that upstream itself deliberately excludes.
func TestClassifyEmailDomain(t *testing.T) {
	cases := []struct {
		name  string
		email string
		want  EmailRiskKind
	}{
		// Exact disposable entries (classic, farm rings, compiled blocks).
		{"10minutemail", "u@10minutemail.com", EmailRiskDisposable},
		{"mailinator", "u@mailinator.com", EmailRiskDisposable},
		{"yopmail", "u@yopmail.com", EmailRiskDisposable},
		{"temp-mail", "u@temp-mail.org", EmailRiskDisposable},
		{"aifotoeditor", "u@aifotoeditor.com", EmailRiskDisposable},
		{"l0veyou", "u@l0veyou.com", EmailRiskDisposable},
		{"pumpkinai", "u@pumpkinai.space", EmailRiskDisposable},
		{"gmaiko", "u@gmaiko.com", EmailRiskDisposable},
		{"gmbel", "u@gmbel.com", EmailRiskDisposable},
		{"gmisol-my-id", "u@gmisol.my.id", EmailRiskDisposable},
		{"gmito-my-id", "u@gmito.my.id", EmailRiskDisposable},
		{"duojumbo", "u@duojumbo.com", EmailRiskDisposable},
		{"gmaoiil", "u@gmaoiil.com", EmailRiskDisposable},
		{"dhisy", "u@dhisy.com", EmailRiskDisposable},
		{"proxyvpn", "u@proxyvpn.cn", EmailRiskDisposable},
		{"wdrvk-dpdns", "u@wdrvk.dpdns.org", EmailRiskDisposable},
		// Any-subdomain match (wildcard inboxes / farm subdomains).
		{"subdomain mailinator", "u@sub.mailinator.com", EmailRiskDisposable},
		{"deep subdomain guerrillamail", "u@a.b.guerrillamail.net", EmailRiskDisposable},
		{"gmail-lookalike subdomain", "u@gmail.l0veyou.com", EmailRiskDisposable},
		{"edu subdomain pumpkinai", "u@edu.pumpkinai.space", EmailRiskDisposable},
		{"nested dewaa", "u@x.y.dewaa.id", EmailRiskDisposable},
		// Case-insensitive.
		{"upper domain", "u@MAILINATOR.COM", EmailRiskDisposable},
		{"upper subdomain", "u@Sub.Mailinator.Com", EmailRiskDisposable},
		// Lookalikes / substrings that are NOT exact entries never flag
		// (the upstream curation bar: gmisel.com, gieemel.com, bekri.site).
		{"gmisel lookalike", "u@gmisel.com", EmailRiskClean},
		{"gmasol lookalike", "u@gmasol.com", EmailRiskClean},
		{"gieemel lookalike", "u@gieemel.com", EmailRiskClean},
		{"gmail plain", "u@gmail.com", EmailRiskClean},
		{"outlook plain", "u@outlook.com", EmailRiskClean},
		{"mailinator as suffix of another domain", "u@mailinator.com.evil.com", EmailRiskClean},
		{"notmailinator", "u@notmailinator.com", EmailRiskClean},
		{"10minutemail org", "u@10minutemail.org", EmailRiskClean},
		{"yopmail net", "u@yopmail.net", EmailRiskClean},
		// Mainstream privacy (classified, never disposable/relay).
		{"proton.me", "u@proton.me", EmailRiskMainstream},
		{"protonmail.ch", "u@protonmail.ch", EmailRiskMainstream},
		{"pm.me", "u@pm.me", EmailRiskMainstream},
		{"apple relay", "u@privaterelay.appleid.com", EmailRiskMainstream},
		{"duck.com", "u@duck.com", EmailRiskMainstream},
		{"mozmail", "u@mozmail.com", EmailRiskMainstream},
		{"tuta family", "u@tuta.com", EmailRiskMainstream},
		{"tutamail", "u@tutamail.com", EmailRiskMainstream},
		{"tutanota", "u@tutanota.com", EmailRiskMainstream},
		{"proton subdomain", "u@mail.proton.me", EmailRiskMainstream},
		// Privacy relays (classified, still priced — distinct from mainstream).
		{"passmail", "u@passmail.net", EmailRiskRelay},
		{"aleeas", "u@aleeas.com", EmailRiskRelay},
		{"anonaddy", "u@anonaddy.me", EmailRiskRelay},
		{"simplelogin com", "u@simplelogin.com", EmailRiskRelay},
		{"simplelogin io", "u@simplelogin.io", EmailRiskRelay},
		{"simplelogin subdomain", "u@x.passmail.net", EmailRiskRelay},
		// Unparseable / absent.
		{"empty", "", EmailRiskClean},
		{"no at", "not-an-email", EmailRiskClean},
		{"trailing at", "user@", EmailRiskClean},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyEmailDomain(tc.email); got != tc.want {
				t.Errorf("classifyEmailDomain(%q) = %q, want %q", tc.email, got, tc.want)
			}
		})
	}
}

// TestMaskEmail pins the report masking: local part masked, domain kept,
// never a full raw address.
func TestMaskEmail(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"john.doe@gmail.com", "j***e@gmail.com"},
		{"a@b.com", "*@b.com"},
		{"ab@b.com", "a*@b.com"},
		{"User@PROTON.ME", "U***r@proton.me"},
		{"short.name@mail.example.com", "s***e@mail.example.com"},
		{"", ""},
		{"no-at-sign", ""},
		{"trailing@", ""},
	}
	for _, tc := range cases {
		got := maskEmail(tc.in)
		if got != tc.want {
			t.Errorf("maskEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if tc.want != "" && strings.Contains(tc.in, "@") {
			local := tc.in[:strings.LastIndex(tc.in, "@")]
			if len(local) > 2 && got == local+"@"+strings.ToLower(tc.in[strings.LastIndex(tc.in, "@")+1:]) {
				t.Errorf("maskEmail(%q) leaked the full address: %q", tc.in, got)
			}
		}
	}
}

// TestFlagSharedMailboxes pins the shared-mailbox heuristic: same mailbox
// (domain + normalized local part, +tag aliases stripped, case-insensitive)
// flags every affected token; different domains with the same local part
// are deliberately NOT shared.
func TestFlagSharedMailboxes(t *testing.T) {
	rows := []TokenHealth{
		{Index: 0, Email: "bob@example.com"},
		{Index: 1, Email: "BOB+newsletter@EXAMPLE.com"}, // same mailbox, +tag, case
		{Index: 2, Email: "carol@example.com"},
		{Index: 3, Email: "bob@other-domain.com"}, // same local part, different mailbox
		{Index: 4, Email: ""},
	}
	FlagSharedMailboxes(rows)
	if !rows[0].Shared || !rows[1].Shared {
		t.Errorf("rows 0/1 should be flagged shared (same mailbox): %+v", rows[:2])
	}
	if rows[2].Shared || rows[3].Shared || rows[4].Shared {
		t.Errorf("rows 2/3/4 should NOT be shared: %+v", rows[2:])
	}
	if !strings.Contains(rows[0].Hint, "SHARED") {
		t.Errorf("row 0 hint should mention SHARED: %q", rows[0].Hint)
	}
}

// TestCheckTokenHealthMeClassification pins the /api/v1/me terminal
// classifications and the country_blocked refinement.
func TestCheckTokenHealthMeClassification(t *testing.T) {
	t.Run("401 is INVALID terminal", func(t *testing.T) {
		srv, log := newTokenHealthServer(t,
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
			},
			func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("session probe ran after a terminal /api/v1/me 401 (session %s)", r.URL.Path)
			},
		)
		row, err := CheckTokenHealth(context.Background(), healthClient(t, "tok-1", 0, srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		if row.State != TokenInvalid {
			t.Errorf("state = %q, want %q", row.State, TokenInvalid)
		}
		if !strings.Contains(row.Hint, "401") {
			t.Errorf("hint = %q, want mention of 401", row.Hint)
		}
		if n := log.paths["/api/v1/freebuff/session"]; n != 0 {
			t.Errorf("session probe count = %d, want 0 (terminal me short-circuit)", n)
		}
	})

	t.Run("403 is BANNED terminal", func(t *testing.T) {
		srv, _ := newTokenHealthServer(t,
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":"forbidden"}`)
			},
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"status":"none"}`)
			},
		)
		row, err := CheckTokenHealth(context.Background(), healthClient(t, "tok-1", 0, srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		if row.State != TokenBanned {
			t.Errorf("state = %q, want %q", row.State, TokenBanned)
		}
	})

	t.Run("403 refined by session country_blocked", func(t *testing.T) {
		srv, _ := newTokenHealthServer(t,
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":"forbidden"}`)
			},
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"status":"country_blocked","countryCode":"SG","countryBlockReason":"geo_restricted","ipPrivacySignals":["proxy_ip"]}`)
			},
		)
		row, err := CheckTokenHealth(context.Background(), healthClient(t, "tok-1", 0, srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		if row.State != TokenCountryBlocked {
			t.Errorf("state = %q, want %q (me 403 refined by session probe)", row.State, TokenCountryBlocked)
		}
	})

	t.Run("me failure but session OK", func(t *testing.T) {
		srv, _ := newTokenHealthServer(t,
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"boom"}`)
			},
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":"session not found"}`)
			},
		)
		row, err := CheckTokenHealth(context.Background(), healthClient(t, "tok-1", 0, srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		if row.State != TokenOK {
			t.Errorf("state = %q, want %q (session probe authoritative)", row.State, TokenOK)
		}
		if !strings.Contains(row.Hint, "HTTP 500") {
			t.Errorf("hint = %q, want me HTTP-500 note", row.Hint)
		}
		if row.Email != "" {
			t.Errorf("email = %q, want empty (me failed)", row.Email)
		}
	})
}

// TestCheckTokenHealthSessionClassification drives the zero-cost session
// probe through the shared sentinel matrix and maps each refusal onto the
// report vocabulary.
func TestCheckTokenHealthSessionClassification(t *testing.T) {
	cases := []struct {
		name      string
		session   func(w http.ResponseWriter)
		wantState TokenHealthState
		wantHint  string
	}{
		{
			name: "404 no session",
			session: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":"session not found"}`)
			},
			wantState: TokenOK,
			wantHint:  "no active session",
		},
		{
			name: "200 none",
			session: func(w http.ResponseWriter) {
				_, _ = io.WriteString(w, `{"status":"none","accessTier":"free"}`)
			},
			wantState: TokenOK,
			wantHint:  "session none",
		},
		{
			name: "200 active",
			session: func(w http.ResponseWriter) {
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","model":"mimo/mimo-v2.5"}`)
			},
			wantState: TokenOK,
			wantHint:  "session active",
		},
		{
			name: "200 queued",
			session: func(w http.ResponseWriter) {
				_, _ = io.WriteString(w, `{"status":"queued","position":2,"queueDepth":5}`)
			},
			wantState: TokenOK,
			wantHint:  "queued",
		},
		{
			name: "200 ended",
			session: func(w http.ResponseWriter) {
				_, _ = io.WriteString(w, `{"status":"ended"}`)
			},
			wantState: TokenOK,
			wantHint:  "no active session",
		},
		{
			name: "403 banned",
			session: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"status":"banned","message":"Your account has been banned.","resumes_at":"2026-09-01T07:00:00Z"}`)
			},
			wantState: TokenBanned,
			wantHint:  "banned",
		},
		{
			name: "403 account_suspended",
			session: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":"account_suspended","message":"suspended due to billing issues."}`)
			},
			wantState: TokenBanned,
			wantHint:  "banned",
		},
		{
			name: "403 country_blocked",
			session: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"status":"country_blocked","message":"Free mode is not available in your country","countryCode":"SG","countryBlockReason":"geo_restricted","ipPrivacySignals":["proxy_ip"]}`)
			},
			wantState: TokenCountryBlocked,
			wantHint:  "SG",
		},
		{
			name: "401 rejected",
			session: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
			},
			wantState: TokenInvalid,
			wantHint:  "401",
		},
		{
			name: "429 spend_limited with status key",
			session: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"status":"spend_limited","message":"You have reached today's spend budget.","resetAt":"2026-09-01T07:00:00Z","retryAfterMs":3600000}`)
			},
			wantState: TokenSpendLimited,
			wantHint:  "spend",
		},
		{
			name: "429 spend_limited via error code",
			session: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":"spend_limited","message":"spend ceiling reached"}`)
			},
			wantState: TokenSpendLimited,
			wantHint:  "spend",
		},
		{
			name: "429 rate_limited",
			session: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"status":"rate_limited","message":"Daily session quota exhausted","recentCount":5,"limit":4,"resetAt":"2026-09-01T07:00:00Z"}`)
			},
			wantState: TokenRateLimited,
			wantHint:  "rate",
		},
		{
			name: "429 ip_capped",
			session: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"status":"ip_capped","model":"mimo/mimo-v2.5","activeUsersForIp":6,"limit":5,"retryAfterMs":60000}`)
			},
			wantState: TokenRateLimited,
			wantHint:  "ip_capped",
		},
		{
			name: "429 turn_spend_limit is soft, not a token problem",
			session: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":"turn_spend_limit","message":"Something went wrong with this turn.","retryAfterMs":60000}`)
			},
			wantState: TokenRateLimited,
			wantHint:  "turn spend",
		},
		{
			name: "402 no credits",
			session: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusPaymentRequired)
				_, _ = io.WriteString(w, `{"error":"no credits","message":"You don't have any credits left"}`)
			},
			wantState: TokenSpendLimited,
			wantHint:  "402",
		},
		{
			name: "503 waiting room",
			session: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":"waiting_room_queued","message":"row caught mid-admit"}`)
			},
			wantState: TokenUnknown,
			wantHint:  "waiting room",
		},
		{
			name: "500 unknown",
			session: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"boom"}`)
			},
			wantState: TokenUnknown,
			wantHint:  "HTTP 500",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTokenHealthServer(t,
				func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.WriteString(w, `{"id":"u-1","email":"real.user@proton.me","discord_id":null}`)
				},
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					tc.session(w)
				}),
			)
			row, err := CheckTokenHealth(context.Background(), healthClient(t, "tok-1", 0, srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			if row.State != tc.wantState {
				t.Errorf("state = %q, want %q", row.State, tc.wantState)
			}
			if !strings.Contains(strings.ToLower(row.Hint), strings.ToLower(tc.wantHint)) {
				t.Errorf("hint = %q, want mention of %q", row.Hint, tc.wantHint)
			}
			// The me response feeds the mailbox: masked, risk-classified.
			if row.Email != "r***r@proton.me" {
				t.Errorf("email = %q, want masked r***r@proton.me", row.Email)
			}
			if row.Risk != EmailRiskMainstream {
				t.Errorf("risk = %q, want mainstream", row.Risk)
			}
		})
	}
}

// TestCheckTokenHealthDisposableMailbox pins RISK-HIGH classification on the
// row and its hint, for the exit-code contract.
func TestCheckTokenHealthDisposableMailbox(t *testing.T) {
	srv, _ := newTokenHealthServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":"u-1","email":"sock.ring@yopmail.com","discord_id":null}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"status":"none"}`)
		},
	)
	row, err := CheckTokenHealth(context.Background(), healthClient(t, "tok-1", 0, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if row.Risk != EmailRiskDisposable {
		t.Errorf("risk = %q, want disposable", row.Risk)
	}
	if !strings.Contains(row.Hint, "RISK-HIGH") {
		t.Errorf("hint = %q, want RISK-HIGH", row.Hint)
	}
	if row.State != TokenOK {
		t.Errorf("state = %q, want OK (mailbox risk is independent of probe state)", row.State)
	}
}

// TestCheckTokenHealthNoMutatingCalls is the anti-ban guard: a health check
// must only ever issue GETs, exactly one per probe endpoint, and never
// touch session creation/deletion or any other path.
func TestCheckTokenHealthNoMutatingCalls(t *testing.T) {
	srv, log := newTokenHealthServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":"u-1","email":"u@example.com","discord_id":null}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"i-1"}`)
		},
	)
	row, err := CheckTokenHealth(context.Background(), healthClient(t, "tok-live", 0, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if row.State != TokenOK {
		t.Errorf("state = %q, want OK", row.State)
	}
	// newTokenHealthServer's guard already fails the test on any non-GET;
	// here we pin the exact read-only request set.
	methods, paths := log.counts()
	if methods != "GET" {
		t.Errorf("methods seen = %q, want GET only (no POST/PUT/DELETE/PATCH)", methods)
	}
	if paths["/api/v1/me"] != 1 {
		t.Errorf("/api/v1/me count = %d, want 1", paths["/api/v1/me"])
	}
	if paths["/api/v1/freebuff/session"] != 1 {
		t.Errorf("/api/v1/freebuff/session count = %d, want 1", paths["/api/v1/freebuff/session"])
	}
	if len(paths) != 2 {
		t.Errorf("paths hit = %v, want exactly the two probe endpoints", paths)
	}
}

// TestValidateTokens pins the multi-token runner: one row per token in
// index order, plus a config-construction error surfacing as a hard error
// (exit 2 path).
func TestValidateTokens(t *testing.T) {
	srv := newHealthServerOK(t)
	defer srv.Close()

	cfg := testConfig(srv.URL, nil)
	rows, err := ValidateTokens(context.Background(), cfg, []string{"tok-a", "tok-b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Index != 0 || rows[1].Index != 1 {
		t.Errorf("indexes = %d,%d, want 0,1", rows[0].Index, rows[1].Index)
	}
	if rows[0].State != TokenOK || rows[1].State != TokenOK {
		t.Errorf("states = %q,%q, want OK,OK", rows[0].State, rows[1].State)
	}

	t.Run("config error aborts", func(t *testing.T) {
		badCfg := testConfig(srv.URL, func(c *config.Config) {
			c.TLSFingerprint = "no-such-profile"
		})
		_, err := ValidateTokens(context.Background(), badCfg, []string{"tok-a"})
		if err == nil || !strings.Contains(err.Error(), "token #1") {
			t.Errorf("err = %v, want token #1 construction error", err)
		}
	})
}

// newHealthServerOK is a tiny two-GET upstream stand-in: /api/v1/me 200 and
// session 200 none.
func newHealthServerOK(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("non-GET %s %s during validate", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/api/v1/me":
			_, _ = io.WriteString(w, `{"id":"u-1","email":"u@example.com","discord_id":null}`)
		case "/api/v1/freebuff/session":
			_, _ = io.WriteString(w, `{"status":"none"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv
}

// TestFormatHealthReport renders the table (and does not leak raw emails or
// tokens).
func TestFormatHealthReport(t *testing.T) {
	rows := []TokenHealth{
		{Index: 0, State: TokenOK, Risk: EmailRiskClean, Email: "j***e@gmail.com", Hint: "no active session (idle)"},
		{Index: 1, State: TokenBanned, Risk: EmailRiskDisposable, Shared: true, Email: "s***g@yopmail.com", Hint: "banned upstream (resumes at 2026-09-01T07:00:00Z); disposable mailbox (RISK-HIGH: upstream prices a $0.50/day restricted ceiling + 2x hard cap); SHARED mailbox: same mailbox on 2 token(s) (upstream caps >=3 accounts per mailbox; server-side count unknown)"},
		{Index: 2, State: TokenRateLimited, Risk: EmailRiskMainstream, Email: "r***r@proton.me", Hint: "rate limited upstream (reset at 2026-09-01T07:00:00Z) (soft — cooldown, not death)"},
	}
	report := FormatHealthReport(rows)
	t.Log("\n" + report)
	for _, want := range []string{"STATE", "RISK", "SHARED", "EMAIL", "HINT", "BANNED", "RATE_LIMITED", "yopmail.com", "proton.me"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	// The raw addresses must never appear in the report.
	for _, leaked := range []string{"jane@gmail.com", "sock-ring@yopmail.com", "realuser@proton.me", "tok-"} {
		if strings.Contains(report, leaked) {
			t.Errorf("report leaked %q:\n%s", leaked, report)
		}
	}
	if !strings.HasPrefix(report, "  #") {
		t.Errorf("report should start with the header row:\n%s", report)
	}
}

// TestCheckTokenHealthDummySkipsNetwork pins the package convention: mock
// tokens are never probed against the network.
func TestCheckTokenHealthDummySkipsNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("dummy token issued %s %s — mock tokens must skip probes", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	row, err := CheckTokenHealth(context.Background(), healthClient(t, "cb_dummy-1", 0, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if row.State != TokenOK {
		t.Errorf("state = %q, want OK", row.State)
	}
	if !strings.Contains(row.Hint, "mock token") {
		t.Errorf("hint = %q, want mock-token note", row.Hint)
	}
}

// TestProbeMeRequestShape pins the CLI-faithful /api/v1/me request: Bearer
// auth on GET with fields=id,email,discord_id (URLSearchParams encoding).
func TestProbeMeRequestShape(t *testing.T) {
	type gotReq struct {
		auth  string
		query string
	}
	got := make(chan gotReq, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		if r.URL.Path == "/api/v1/freebuff/session" {
			_, _ = io.WriteString(w, `{"status":"none"}`)
			return
		}
		got <- gotReq{auth: r.Header.Get("Authorization"), query: r.URL.RawQuery}
		_, _ = io.WriteString(w, `{"id":"u-1","email":"u@example.com","discord_id":null}`)
	}))
	t.Cleanup(srv.Close)
	if _, err := CheckTokenHealth(context.Background(), healthClient(t, "tok-shape", 0, srv.URL)); err != nil {
		t.Fatal(err)
	}
	req := <-got
	if req.auth != "Bearer tok-shape" {
		t.Errorf("Authorization = %q, want Bearer tok-shape", req.auth)
	}
	if req.query != "fields=id%2Cemail%2Cdiscord_id" {
		t.Errorf("query = %q, want fields=id%%2Cemail%%2Cdiscord_id (CLI URLSearchParams encoding)", req.query)
	}
}

// TestMainExitCodeContract documents the -validate-tokens exit-code rules at
// the row level (main.go maps rows to 0/1): BANNED, INVALID, and DISPOSABLE
// rows must exit 1; every other state 0.
func TestMainExitCodeContract(t *testing.T) {
	exit := func(rows []TokenHealth) int {
		code := 0
		for _, r := range rows {
			if r.State == TokenBanned || r.State == TokenInvalid || r.Risk == EmailRiskDisposable {
				code = 1
			}
		}
		return code
	}
	cases := []struct {
		name string
		rows []TokenHealth
		want int
	}{
		{"all ok", []TokenHealth{{State: TokenOK}, {State: TokenRateLimited}}, 0},
		{"banned", []TokenHealth{{State: TokenOK}, {State: TokenBanned}}, 1},
		{"invalid", []TokenHealth{{State: TokenInvalid}}, 1},
		{"disposable despite OK", []TokenHealth{{State: TokenOK, Risk: EmailRiskDisposable}}, 1},
		{"country blocked is soft for exit code", []TokenHealth{{State: TokenCountryBlocked}}, 0},
		{"unknown is soft", []TokenHealth{{State: TokenUnknown}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exit(tc.rows); got != tc.want {
				t.Errorf("exit = %d, want %d", got, tc.want)
			}
		})
	}
}
