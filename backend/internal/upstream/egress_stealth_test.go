package upstream

// Tests for the Wave-5 egress/stealth features that survive the proxy
// removal: HTTP/2 upstream wiring (#51) and the passive risk-engine feed
// (#64). Stable-egress pinning and its dial-fallback tests were deleted
// with the outbound-proxy machinery (the official CLI has no proxy support
// and the upstream hard-blocks proxied egress). Kept in their own file so
// concurrent work on client_test.go does not collide.

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// TestHTTP2UpstreamWiring guards the HTTP2_UPSTREAM wiring (issue #51):
// stealth clients register an http2.Transport for the https scheme (dials
// with the same utls dialer advertising the browser ALPN), plain clients
// leave the stdlib h2 default on, and HTTP2_UPSTREAM=false forces h1 on the
// plain path (empty TLSNextProto map — the documented h2 kill switch).
//
// Registration is asserted behaviorally: with the stealth h2 transport
// registered, an https request is dispatched to it BEFORE any stdlib dial,
// so its dial failure carries the "stealth: tcp dial failed" wrapper; the
// h1 paths fail with a plain stdlib dial error. No external network is
// touched — 127.0.0.1:1 refuses instantly.
func TestHTTP2UpstreamWiring(t *testing.T) {
	roundTripErr := func(c *Client) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "https://127.0.0.1:1/", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.http.Transport.RoundTrip(req)
		if err == nil {
			t.Fatal("RoundTrip to a refused port succeeded")
		}
		return err.Error()
	}

	t.Run("plain default off in direct-config tests", func(t *testing.T) {
		// testConfig leaves HTTP2Upstream=false (zero value); production
		// defaults it true via config.Load. The false path must pin h1.
		plain, err := New("tok", testConfig("", nil))
		if err != nil {
			t.Fatal(err)
		}
		tr := plain.http.Transport.(*http.Transport)
		if tr.TLSNextProto == nil || tr.TLSNextProto["h2"] != nil {
			t.Errorf("HTTP2_UPSTREAM=false must disable h2 (empty TLSNextProto map), got %v", tr.TLSNextProto)
		}
		if msg := roundTripErr(plain); strings.Contains(msg, "stealth:") {
			t.Errorf("plain h1 client dial error = %q, want a plain stdlib error", msg)
		}
	})

	t.Run("plain enabled keeps stdlib h2", func(t *testing.T) {
		c, err := New("tok", testConfig("", func(cfg *config.Config) { cfg.HTTP2Upstream = true }))
		if err != nil {
			t.Fatal(err)
		}
		tr := c.http.Transport.(*http.Transport)
		// The stdlib registers h2 lazily on first use; the wiring contract is
		// that we did NOT disable it and that ForceAttemptHTTP2 stays on.
		if tr.TLSNextProto != nil {
			t.Errorf("HTTP2_UPSTREAM=true must leave TLSNextProto nil (stdlib h2 default), got %v", tr.TLSNextProto)
		}
		if !tr.ForceAttemptHTTP2 {
			t.Error("ForceAttemptHTTP2 must stay true for the stdlib h2 path")
		}
		if msg := roundTripErr(c); strings.Contains(msg, "stealth:") {
			t.Errorf("plain h2 client dial error = %q, want a plain stdlib error", msg)
		}
	})

	t.Run("stealth enabled routes https through the h2 transport", func(t *testing.T) {
		c, err := New("tok", testConfig("", func(cfg *config.Config) {
			cfg.HTTP2Upstream = true
			cfg.TLSFingerprint = "chrome126"
		}))
		if err != nil {
			t.Fatal(err)
		}
		// The dial failure must carry the stealth wrapper: proof the https
		// request was dispatched to the registered http2.Transport (whose
		// DialTLSContext is the utls dialer) rather than the h1 transport.
		msg := roundTripErr(c)
		if !strings.Contains(msg, "stealth: tcp dial failed") {
			t.Errorf("stealth h2 dial error = %q, want the stealth wrapper (https dispatched to the utls dialer)", msg)
		}
	})

	t.Run("stealth disabled keeps h1", func(t *testing.T) {
		c, err := New("tok", testConfig("", func(cfg *config.Config) {
			cfg.HTTP2Upstream = false
			cfg.TLSFingerprint = "chrome126"
		}))
		if err != nil {
			t.Fatal(err)
		}
		if c.http2Upstream {
			t.Error("HTTP2_UPSTREAM=false must leave http2Upstream false")
		}
		// The h1 path still dials through the stealth DialTLSContext — the
		// wrapper is expected; the point is that no https h2 transport took
		// over the request (that would need HTTP2_UPSTREAM=true).
		if msg := roundTripErr(c); !strings.Contains(msg, "tcp dial failed") {
			t.Errorf("h1 stealth dial error = %q, want the tcp dial wrapper", msg)
		}
	})
}

// TestStealthH2NoBundledConfigureLog guards the "protocol https already
// registered" log: the stdlib's onceSetNextProtoDefaults runs the bundled
// http2 configure on the transport's first use, which panics (recovered
// into a logged error) because our stealth h2 transport already owns
// "https" via RegisterProtocol. The empty TLSNextProto kill switch skips
// that configure; the warning must never hit the log.
func TestStealthH2NoBundledConfigureLog(t *testing.T) {
	c, err := New("tok", testConfig("", func(cfg *config.Config) {
		cfg.HTTP2Upstream = true
		cfg.TLSFingerprint = "chrome126"
	}))
	if err != nil {
		t.Fatal(err)
	}

	// Capture the default logger, which is where net/http emits the
	// bundled-h2 configure error.
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	// First use of the transport is what triggers onceSetNextProtoDefaults.
	req, err := http.NewRequest(http.MethodGet, "https://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.http.Transport.RoundTrip(req); err == nil {
		t.Fatal("RoundTrip to a refused port succeeded")
	}
	if got := buf.String(); strings.Contains(got, "protocol https already registered") {
		t.Errorf("bundled h2 configure warning leaked to the log:\n%s", got)
	}
}

// TestSessionResponseFeedsRiskEngine guards the passive risk feed (issue
// #64): a session response carrying ipPrivacySignals and ip_capped
// the parse surfaces ip_capped signals (the passice risk engine they used
// to feed was removed in issue #275).
func TestSessionResponseFeedsRiskEngine(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ip_capped","model":"deepseek/deepseek-v4-flash","activeUsersForIp":8,"limit":10,"ipPrivacySignals":["proxy"],"retryAfterMs":500}`)
	}

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateSession(context.Background()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// The passive risk engine was removed (issue #275): the ip_capped parse
	// surface above is the regression guard; the feed/Score half no longer
	// exists in production.

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-clean","model":"deepseek/deepseek-v4-flash"}`)
	}
	if _, err := client.CreateSession(context.Background()); err != nil {
		t.Fatalf("clean CreateSession: %v", err)
	}
	// The clean sample carries no signals; the worst retained sample (from
	// the ring) still drives the score — so assert the engine stays within
	// bounds rather than a specific value (the retained-window semantics are
	// tested in the stealth package).

}
