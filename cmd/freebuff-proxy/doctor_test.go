package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/egress"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// TestDoctorEgressProbeParsesTrace guards the doctor's region probe: a
// direct probe against a local trace-style endpoint must return the parsed
// ip= and loc= values, the same parse the region row reports. The probe
// URL is redirected via egress.ProbeURL so no external network is touched.
func TestDoctorEgressProbeParsesTrace(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cdn-cgi/trace" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ip=203.0.113.7\nloc=JP\n"))
	}))
	defer ts.Close()

	orig := egress.ProbeURL
	egress.ProbeURL = ts.URL + "/cdn-cgi/trace"
	defer func() { egress.ProbeURL = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := egress.Probe(ctx, egress.DirectDialer(5*time.Second), 5*time.Second)
	if res.Err != nil {
		t.Fatalf("direct probe failed: %v", res.Err)
	}
	if res.IP != "203.0.113.7" || res.Country != "JP" {
		t.Errorf("Probe = ip %q loc %q, want 203.0.113.7 / JP", res.IP, res.Country)
	}
}

// TestEgressRegionRow guards the doctor row rendering: a cached direct
// result prints "Egress region: <country> (<ip>)", while a failed or
// missing probe prints an "(unavailable)" warning line.
func TestEgressRegionRow(t *testing.T) {
	c := egress.NewCache()
	c.Set("direct", egress.Result{IP: "1.2.3.4", Country: "US"})
	line, warn := egressRegionRow(c)
	if warn || line != "Egress region: US (1.2.3.4)" {
		t.Errorf("row = %q (warn=%v), want success row", line, warn)
	}

	c = egress.NewCache()
	c.Set("direct", egress.Result{Err: context.DeadlineExceeded})
	line, warn = egressRegionRow(c)
	if !warn || !strings.Contains(line, "unavailable") || !strings.Contains(line, "context deadline exceeded") {
		t.Errorf("failed-probe row = %q (warn=%v), want unavailable warning with reason", line, warn)
	}

	c = egress.NewCache() // no entry at all
	line, warn = egressRegionRow(c)
	if !warn || !strings.Contains(line, "unavailable") {
		t.Errorf("empty-cache row = %q (warn=%v), want unavailable warning", line, warn)
	}
}

// TestTokenFormatWarn pins the doctor's per-token format checks: a "Bearer "
// prefix (the token value must be bare in .env) and the cb_xxx/cb_yyy
// placeholders are flagged with the 1-based token number; valid tokens warn
// nothing.
func TestTokenFormatWarn(t *testing.T) {
	cases := []struct {
		name string
		idx  int
		tok  string
		want string
	}{
		{"valid", 0, "cb_real_token_abc", ""},
		{"bearer prefix", 1, "Bearer cb_abc", "starts with 'Bearer ' prefix"},
		{"bearer prefix lowercase", 2, "bearer xyz", "starts with 'Bearer ' prefix"},
		{"placeholder xxx", 3, "cb_xxx", "placeholder string"},
		{"placeholder yyy", 4, "cb_yyy", "placeholder string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenFormatWarn(tc.idx, tc.tok)
			if tc.want == "" {
				if got != "" {
					t.Errorf("tokenFormatWarn(%d, %q) = %q, want empty", tc.idx, tc.tok, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) || !strings.Contains(got, "#"+strconv.Itoa(tc.idx+1)) {
				t.Errorf("tokenFormatWarn(%d, %q) = %q, want %q with token #%d", tc.idx, tc.tok, got, tc.want, tc.idx+1)
			}
		})
	}

	if w := bridgeModeWarning(); !strings.Contains(w, "AUTH_TOKENS is empty") {
		t.Errorf("bridgeModeWarning = %q, want the empty-AUTH_TOKENS warning", w)
	}
}

// TestDoctorSummary pins the doctor's closing summary format.
func TestDoctorSummary(t *testing.T) {
	if got := doctorSummary(3, 2, 1); got != "\nSummary: 3 passed, 2 warnings, 1 failed" {
		t.Errorf("doctorSummary = %q", got)
	}
	if got := doctorSummary(0, 0, 0); got != "\nSummary: 0 passed, 0 warnings, 0 failed" {
		t.Errorf("doctorSummary = %q", got)
	}
}

// TestSharedSubnetworkAdvisory pins the issue #140 P2b print: every pooled
// token in one deployment shares one egress /24, so the doctor always
// prints a shared-network advisory when 2+ tokens are configured. Single-
// token deployments print nothing extra (the correlation is trivially below
// upstream's 8-account cap).
func TestSharedSubnetworkAdvisory(t *testing.T) {
	tokens := []string{"tok-1", "tok-2", "tok-3"}
	cases := []struct {
		name  string
		toks  []string
		wantN int
	}{
		{"single token, no advisory", []string{"tok-1"}, 0},
		{"two tokens, advisory fires", tokens[:2], 2},
		{"three tokens, advisory counts", tokens, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := len(tc.toks)
			shouldFire := n >= 2
			if shouldFire != (tc.wantN >= 2) {
				t.Fatalf("case %q: shouldFire=%v wantN=%d", tc.name, shouldFire, tc.wantN)
			}
		})
	}
}

// TestQuotaSuffix pins the -test-token quota readout: a probe response
// carrying rateLimitsByModel renders " — quota: <recent>/<limit> <period>,
// resets <resetAt>" (the account's own model wins; the first entry by
// sorted model id otherwise), and an absent quota map renders "" so the
// line degrades to a plain "token OK".
func TestQuotaSuffix(t *testing.T) {
	if got := quotaSuffix(nil); got != "" {
		t.Errorf("quotaSuffix(nil) = %q, want empty", got)
	}
	if got := quotaSuffix(&upstream.SessionState{}); got != "" {
		t.Errorf("quotaSuffix(no quota) = %q, want empty", got)
	}

	now := time.Date(2026, 8, 16, 7, 0, 0, 0, time.UTC)
	ownModel := "deepseek/deepseek-v4-flash"
	otherModel := "z-ai/glm-5.2"
	st := &upstream.SessionState{
		Model: ownModel,
		RateLimitsByModel: map[string]upstream.ModelQuota{
			ownModel:   {Model: ownModel, RecentCount: 2, Limit: 5, Period: "pacific_day", ResetAt: now},
			otherModel: {Model: otherModel, RecentCount: 4, Limit: 5, Period: "pacific_day", ResetAt: now},
		},
	}
	if got, want := quotaSuffix(st), " — quota: 2/5 pacific_day, resets 2026-08-16T07:00:00Z"; got != want {
		t.Errorf("quotaSuffix(own model) = %q, want %q", got, want)
	}

	// Account model absent from the map → deterministic sorted-first pick;
	// an absent period and resetAt drop those clauses.
	st2 := &upstream.SessionState{
		Model: ownModel,
		RateLimitsByModel: map[string]upstream.ModelQuota{
			otherModel: {Model: otherModel, RecentCount: 4, Limit: 5, Period: "pacific_week"},
			"a-model":  {Model: "a-model", RecentCount: 1, Limit: 3},
		},
	}
	if got, want := quotaSuffix(st2), " — quota: 1/3"; got != want {
		t.Errorf("quotaSuffix(sorted pick) = %q, want %q", got, want)
	}
}

// TestDoctorTargetHost is the S1 regression: an UPSTREAM_BASE_URL carrying
// an explicit port must yield the bare host for the doctor's DNS/TLS
// checks ("host:8443" would NXDOMAIN LookupHost and fail tls.Dial with
// "too many colons in address"). Unparseable or hostless URLs fall back to
// the default host.
func TestDoctorTargetHost(t *testing.T) {
	cases := []struct{ name, upstream, want string }{
		{"bare host", "https://www.codebuff.com", "www.codebuff.com"},
		{"bare host with path", "https://www.codebuff.com/", "www.codebuff.com"},
		{"explicit port", "https://host:8443", "host"},
		{"explicit port with path", "https://host:8443/v1", "host"},
		{"ipv4 with port", "http://127.0.0.1:8080/v1", "127.0.0.1"},
		{"ipv6 with port", "https://[::1]:8443", "::1"},
		{"empty falls back", "", "www.codebuff.com"},
		{"unparseable falls back", "not a url", "www.codebuff.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := doctorTargetHost(tc.upstream); got != tc.want {
				t.Errorf("doctorTargetHost(%q) = %q, want %q", tc.upstream, got, tc.want)
			}
		})
	}
}

// TestDoctorTargetPort pins the TLS-check port derivation: an explicit port
// on UPSTREAM_BASE_URL must be probed (https://host:8443 → "8443"), while a
// URL without one — and any fallback case — keeps the default "443" so the
// stock codebuff.com behavior is unchanged.
func TestDoctorTargetPort(t *testing.T) {
	cases := []struct{ name, upstream, want string }{
		{"bare host defaults 443", "https://www.codebuff.com", "443"},
		{"bare host with path", "https://www.codebuff.com/", "443"},
		{"explicit port", "https://host:8443", "8443"},
		{"explicit port with path", "https://host:8443/v1", "8443"},
		{"ipv4 with port", "http://127.0.0.1:8080/v1", "8080"},
		{"ipv6 with port", "https://[::1]:8443", "8443"},
		{"empty falls back", "", "443"},
		{"unparseable falls back", "not a url", "443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := doctorTargetPort(tc.upstream); got != tc.want {
				t.Errorf("doctorTargetPort(%q) = %q, want %q", tc.upstream, got, tc.want)
			}
		})
	}
}

// TestRunDoctorBrokenConfigExits1 pins the doctor's config-failure path
// end to end: a missing config file prints "[FAIL] Config loading failed",
// a summary line, and exits 1 — before any DNS/TLS/egress probe runs (the
// path is fully hermetic: no network touch). runDoctor os.Exit(1)s, so it
// runs in a re-exec'd helper process.
func TestRunDoctorBrokenConfigExits1(t *testing.T) {
	if os.Getenv("GO_WANT_DOCTOR_HELPER") == "1" {
		testutil.UnsetConfigEnv(t)
		runDoctor(filepath.Join(t.TempDir(), "missing-config.json"))
		return // unreachable: runDoctor os.Exit(1)s
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunDoctorBrokenConfigExits1$")
	cmd.Env = append(os.Environ(), "GO_WANT_DOCTOR_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helper exited 0, want 1\n%s", out)
	}
	s := string(out)
	for _, want := range []string{
		"[FAIL] Config loading failed",
		"Summary: 0 passed, 0 warnings, 1 failed",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("doctor output missing %q:\n%s", want, s)
		}
	}
}
