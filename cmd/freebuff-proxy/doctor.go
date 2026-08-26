package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/egress"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/upstream"
)

// egressRegionRow renders the doctor's egress region line from the direct
// probe cache entry: "Egress region: <country> (<ip>)" on success, an
// "(unavailable)" warning when the probe failed or no result is cached.
func egressRegionRow(cache *egress.Cache) (line string, warn bool) {
	r, ok := cache.Get("direct")
	if !ok || r.Err != nil || r.Country == "" || r.IP == "" {
		reason := "no direct probe result"
		if ok && r.Err != nil {
			reason = fmt.Sprintf("direct probe failed: %v", r.Err)
		}
		return fmt.Sprintf("Egress region: unavailable (%s)", reason), true
	}
	return fmt.Sprintf("Egress region: %s (%s)", r.Country, r.IP), false
}

// doctorTargetHost derives the host the doctor's DNS/TLS reachability
// checks probe, from the upstream base URL. An explicit port on the URL is
// stripped: LookupHost and tls.Dial take a bare host, and "host:8443" would
// otherwise NXDOMAIN the lookup and fail tls.Dial with "too many colons in
// address" (S1) — failing a healthy config. Falls back to the default host
// when the URL is unparseable or hostless.
func doctorTargetHost(upstreamBaseURL string) string {
	if u, err := url.Parse(upstreamBaseURL); err == nil && u.Host != "" {
		if host, _, err := net.SplitHostPort(u.Host); err == nil {
			return host
		}
		return u.Host
	}
	return "www.codebuff.com"
}

// doctorTargetPort returns the TCP port the doctor's TLS check probes, from
// the upstream base URL. It defaults to "443" when the URL carries no
// explicit port (or is unparseable/hostless), keeping the default
// codebuff.com behavior identical. A self-hosted UPSTREAM_BASE_URL with an
// explicit port (e.g. https://host:8443) must probe that port, not 443 —
// otherwise the TLS diagnostic fails falsely against a healthy config.
func doctorTargetPort(upstreamBaseURL string) string {
	if u, err := url.Parse(upstreamBaseURL); err == nil && u.Host != "" {
		if _, port, err := net.SplitHostPort(u.Host); err == nil && port != "" {
			return port
		}
	}
	return "443"
}

// tokenFormatWarn returns the doctor's warning for a configured token, or
// "" when its format looks valid. "Bearer " prefixes (the token value must
// be bare in .env) and the cb_xxx/cb_yyy placeholders are flagged.
func tokenFormatWarn(index int, token string) string {
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return fmt.Sprintf("Token #%d starts with 'Bearer ' prefix -- remove it from .env", index+1)
	}
	if token == "cb_xxx" || token == "cb_yyy" {
		return fmt.Sprintf("Token #%d is a placeholder string %q", index+1, token)
	}
	return ""
}

// bridgeModeWarning is the doctor warning shown when AUTH_TOKENS is empty
// (bridge mode active).
func bridgeModeWarning() string {
	return "AUTH_TOKENS is empty (bridge mode active). Clients must supply Authorization: Bearer <token>"
}

// doctorSummary renders the doctor's closing summary line.
func doctorSummary(passed, warnings, failed int) string {
	return fmt.Sprintf("\nSummary: %d passed, %d warnings, %d failed", passed, warnings, failed)
}

// runTokenTest probes the first configured token with a zero-cost GET
// /api/v1/freebuff/session probe (no session claimed, no daily slot
// consumed) and exits 0 on success, 1 on failure. Exposed as -test-token
// for installers and scripts.
func runTokenTest(configPath string) {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -test-token: config load failed: %v\n", err)
		os.Exit(1)
	}
	if cfg.BridgeMode() {
		fmt.Fprintln(os.Stderr, "freebuff-proxy: -test-token: no AUTH_TOKENS configured (bridge mode); nothing to probe")
		os.Exit(1)
	}
	clientCfg := cfg
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -test-token: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	st, err := client.ProbeAccount(ctx)
	if err != nil {
		if errors.Is(err, upstream.ErrNoActiveSession) {
			fmt.Println("freebuff-proxy: token OK (no active session)")
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -test-token: token rejected upstream: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("freebuff-proxy: token OK%s\n", quotaSuffix(st))
	os.Exit(0)
}

// quotaSuffix renders the live session quota read back by a successful
// account probe: " — quota: 4/5 pacific_day, resets 2026-08-16T07:00:00Z"
// (the account's own model when present in rateLimitsByModel, else the
// first entry by sorted model id). Returns "" when the probe response
// carried no quota — compact responses omit rateLimitsByModel, so the line
// degrades to a plain "token OK".
func quotaSuffix(st *upstream.SessionState) string {
	if st == nil || len(st.RateLimitsByModel) == 0 {
		return ""
	}
	q, ok := st.RateLimitsByModel[st.Model]
	if !ok {
		ids := make([]string, 0, len(st.RateLimitsByModel))
		for id := range st.RateLimitsByModel {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		q = st.RateLimitsByModel[ids[0]]
	}
	s := fmt.Sprintf(" — quota: %g/%g", q.RecentCount, q.Limit)
	if q.Period != "" {
		s += " " + q.Period
	}
	if !q.ResetAt.IsZero() {
		s += fmt.Sprintf(", resets %s", q.ResetAt.Format(time.RFC3339))
	}
	return s
}

func runDoctor(configPath string) {
	fmt.Println("freebuff-proxy doctor diagnostic tool")
	fmt.Println("=====================================")

	passed := 0
	warnings := 0
	failed := 0

	ok := func(msg string) {
		fmt.Printf("[ok] %s\n", msg)
		passed++
	}
	warn := func(msg string) {
		fmt.Printf("[!!] %s\n", msg)
		warnings++
	}
	fail := func(msg string) {
		fmt.Printf("[FAIL] %s\n", msg)
		failed++
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fail(fmt.Sprintf("Config loading failed: %v", err))
		fmt.Println(doctorSummary(passed, warnings, failed))
		os.Exit(1)
	}
	ok("Configuration loaded & validated successfully")

	if cfg.BridgeMode() {
		warn(bridgeModeWarning())
	} else {
		ok(fmt.Sprintf("AUTH_TOKENS: %d token(s) configured", len(cfg.AuthTokens)))
		for i, tok := range cfg.AuthTokens {
			if w := tokenFormatWarn(i, tok); w != "" {
				warn(w)
			} else {
				ok(fmt.Sprintf("Token #%d format valid (%d chars)", i+1, len(tok)))
			}
		}
	}

	// Port availability check
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		fail(fmt.Sprintf("Listen address %s is not available: %v", cfg.ListenAddr, err))
	} else {
		_ = ln.Close()
		ok(fmt.Sprintf("Listen address %s is available", cfg.ListenAddr))
	}

	// DNS & TLS reachability check
	targetHost := doctorTargetHost(cfg.UpstreamBaseURL)
	targetPort := doctorTargetPort(cfg.UpstreamBaseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupHost(ctx, targetHost)
	if err != nil {
		fail(fmt.Sprintf("DNS lookup for %s failed: %v", targetHost, err))
	} else {
		ok(fmt.Sprintf("DNS lookup for %s resolved (%s)", targetHost, strings.Join(addrs, ", ")))
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tlsConn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(targetHost, targetPort), &tls.Config{ServerName: targetHost})
	if err != nil {
		fail(fmt.Sprintf("TLS connection to %s:%s failed: %v", targetHost, targetPort, err))
	} else {
		_ = tlsConn.Close()
		ok(fmt.Sprintf("TLS connection to %s:%s succeeded", targetHost, targetPort))
	}

	// Egress region check: one live probe of the direct outbound path
	// through a plain dialer. The probe result lands in a doctor-local
	// cache (the runtime no longer probes — #123); a failed probe is a
	// warning, not a doctor failure — the proxy keeps working, only the
	// region readout is missing.
	egressCache := egress.NewCache()
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	res := egress.Probe(probeCtx, egress.DirectDialer(5*time.Second), 5*time.Second)
	probeCancel()
	egressCache.Set("direct", res)
	if line, isWarn := egressRegionRow(egressCache); isWarn {
		warn(line)
	} else {
		ok(line)
	}

	// Registry test
	reg := registry.New(&cfg, &http.Client{Timeout: 10 * time.Second})
	reg.LoadFallback()
	ok(fmt.Sprintf("Model registry offline fallback loaded (%d models, %d agents)", reg.ModelCount(), len(reg.AgentIDs())))

	if err := reg.Refresh(ctx); err != nil {
		warn(fmt.Sprintf("Registry live refresh warning: %v (offline fallback retained)", err))
	} else {
		ok(fmt.Sprintf("Registry live refresh succeeded (%d models)", reg.ModelCount()))
	}

	// Token validity probe: one zero-cost GET /api/v1/freebuff/session probe
	// per configured token (no session claimed, no daily slot consumed). This
	// is the check that catches expired/revoked tokens before the first chat
	// 401s. Probes always run: unlike the old session-handshake probes they
	// never touch the session create API, so there is no allowance cost to
	// opt out of.
	if !cfg.BridgeMode() {
		warn(fmt.Sprintf("Probing %d token(s) (zero-cost GET probes)", len(cfg.AuthTokens)))
		for i, tok := range cfg.AuthTokens {
			clientCfg := cfg
			client, err := upstream.New(tok, &clientCfg)
			if err != nil {
				fail(fmt.Sprintf("Token #%d: cannot build client: %v", i+1, err))
				continue
			}
			probeCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			_, err = client.ProbeAccount(probeCtx)
			cancel()
			if err != nil {
				if errors.Is(err, upstream.ErrNoActiveSession) {
					ok(fmt.Sprintf("Token #%d validity probe succeeded (no active session)", i+1))
					continue
				}
				fail(fmt.Sprintf("Token #%d validity probe failed: %v (re-run the upstream CLI to refresh the token)", i+1, err))
				continue
			}
			ok(fmt.Sprintf("Token #%d validity probe succeeded", i+1))
		}

		// Shared-network advisory (issue #140 P2b): every pooled token in
		// this deployment shares ONE egress path (the proxy's own), so all
		// its accounts are on one /24 by construction. Upstream's
		// shared_signup_network cap permanently limits accounts once ~8 share
		// a /24 (README key-hygiene section) — surface the correlation so an
		// operator running many accounts through one box knows before
		// upstream caps them all.
		if n := len(cfg.AuthTokens); n >= 2 {
			warn(fmt.Sprintf(
				"Shared-network advisory: all %d pooled tokens egress from this machine's IP (one /24). Upstream's shared_signup_network cap permanently limits accounts once ~8 share a subnet; route distinct accounts through distinct residential exits for full-trust isolation.",
				n))
		}
	}

	fmt.Println(doctorSummary(passed, warnings, failed))
	if failed > 0 {
		os.Exit(1)
	}
	os.Exit(0)
}
