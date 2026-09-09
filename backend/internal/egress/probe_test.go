package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// traceServer serves a Cloudflare-trace-style body at any path so the
// probe can be pointed at it via ProbeURL.
func traceServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestProbeParsesTrace guards the core contract: a successful GET parses
// the ip= and loc= lines into the result, and the request goes through the
// given dialer.
func TestProbeParsesTrace(t *testing.T) {
	const body = "fl=7f0\nh=www.cloudflare.com\nip=203.0.113.7\nloc=JP\ntls=ON\n"
	ts := traceServer(t, body, http.StatusOK)

	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	var dialed atomic.Int64
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed.Add(1)
		return DirectDialer(5*time.Second)(ctx, network, addr)
	}

	res := Probe(context.Background(), dialer, 5*time.Second)
	if res.Err != nil {
		t.Fatalf("Probe failed: %v", res.Err)
	}
	if res.IP != "203.0.113.7" || res.Country != "JP" {
		t.Errorf("Probe = ip %q loc %q, want 203.0.113.7 / JP", res.IP, res.Country)
	}
	if dialed.Load() == 0 {
		t.Error("probe did not dial through the provided dialer")
	}
}

// TestProbeMissingTraceFields guards fail-open parsing: a 200 body without
// loc (or without either field) must not be an error; missing fields stay
// empty so the caller can report "unavailable".
func TestProbeMissingTraceFields(t *testing.T) {
	ts := traceServer(t, "ip=203.0.113.7\n", http.StatusOK)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	res := Probe(context.Background(), DirectDialer(5*time.Second), 5*time.Second)
	if res.Err != nil {
		t.Fatalf("Probe failed: %v", res.Err)
	}
	if res.IP != "203.0.113.7" || res.Country != "" {
		t.Errorf("Probe = ip %q loc %q, want 203.0.113.7 / empty", res.IP, res.Country)
	}
}

// TestProbeErrorStatus guards non-200 handling: a trace endpoint that
// answers with an error status is a probe failure, not a parsed result.
func TestProbeErrorStatus(t *testing.T) {
	ts := traceServer(t, "ip=203.0.113.7\nloc=JP\n", http.StatusInternalServerError)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	res := Probe(context.Background(), DirectDialer(5*time.Second), 5*time.Second)
	if res.Err == nil {
		t.Fatal("Probe succeeded against a 500 response")
		return
	}
	if !strings.Contains(res.Err.Error(), "500") {
		t.Errorf("error %q does not mention the status", res.Err)
	}
}

// TestProbeDialError guards fail-open on an unreachable path: the dialer
// error is returned as Err rather than hanging or panicking.
func TestProbeDialError(t *testing.T) {
	ts := traceServer(t, "ip=1.2.3.4\nloc=US\n", http.StatusOK)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	boom := errors.New("dial refused")
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, boom
	}
	res := Probe(context.Background(), dialer, 5*time.Second)
	if !errors.Is(res.Err, boom) {
		t.Errorf("Err = %v, want the dialer error", res.Err)
	}
}

// TestProbeTimeout guards the timeout bound: a dialer that never answers
// must yield an Err within the probe timeout instead of blocking forever.
func TestProbeTimeout(t *testing.T) {
	ts := traceServer(t, "ip=1.2.3.4\nloc=US\n", http.StatusOK)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	// Dialer that ignores ctx and never completes; the client timeout is
	// what must bound the probe.
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		select {}
	}
	start := time.Now()
	res := Probe(context.Background(), dialer, 50*time.Millisecond)
	if res.Err == nil {
		t.Fatal("blocked dial did not time out")
		return
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("probe took %v, want bounded by the 50ms timeout", elapsed)
	}
}

// TestProbeAll guards probeAll: every path yields a result, paths run
// concurrently, and one failing path never aborts the others.
func TestProbeAll(t *testing.T) {
	ts := traceServer(t, "ip=203.0.113.1\nloc=US\n", http.StatusOK)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	t.Run("failure isolation", func(t *testing.T) {
		boom := errors.New("path dial refused")
		paths := []Path{
			{Key: "direct", Dialer: DirectDialer(5 * time.Second)},
			{Key: "proxy-0", Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return nil, boom
			}},
			{Key: "proxy-1", Dialer: DirectDialer(5 * time.Second)},
		}
		results := probeAll(context.Background(), paths, 5*time.Second)
		if len(results) != 3 {
			t.Fatalf("results = %d entries, want 3", len(results))
		}
		if r, ok := results["direct"]; !ok || r.Err != nil || r.IP != "203.0.113.1" {
			t.Errorf("direct result = %+v, want parsed success", r)
		}
		if r, ok := results["proxy-0"]; !ok || !errors.Is(r.Err, boom) {
			t.Errorf("proxy-0 result = %+v, want the dial error", r)
		}
		if r, ok := results["proxy-1"]; !ok || r.Err != nil {
			t.Errorf("proxy-1 result = %+v, want success despite proxy-0 failure", r)
		}
	})

	t.Run("paths run concurrently", func(t *testing.T) {
		// Both dialers block until BOTH have started; if probeAll dialed
		// serially this would deadlock and the timeout below fails the test.
		started := make(chan string, 2)
		release := make(chan struct{})
		dialer := func(name string) func(ctx context.Context, network, addr string) (net.Conn, error) {
			return func(ctx context.Context, network, addr string) (net.Conn, error) {
				started <- name
				<-release
				return nil, errors.New(name + " failed")
			}
		}
		paths := []Path{
			{Key: "a", Dialer: dialer("a")},
			{Key: "b", Dialer: dialer("b")},
		}
		go func() {
			<-started
			<-started
			close(release)
		}()
		done := make(chan map[string]Result, 1)
		go func() { done <- probeAll(context.Background(), paths, time.Second) }()
		select {
		case results := <-done:
			if len(results) != 2 {
				t.Fatalf("results = %d entries, want 2", len(results))
			}
			for _, key := range []string{"a", "b"} {
				if r, ok := results[key]; !ok || r.Err == nil {
					t.Errorf("result[%q] = %+v, want a failure result", key, r)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("probeAll did not run paths concurrently")
		}
	})
}

// TestCache guards the in-package TTL cache: get/set round-trip, expiry,
// re-Set refresh, TTL=0 semantics (always expired), and missing keys. This
// coverage previously lived only in backend/cmd/freebuff-proxy, so the egress
// package itself counted 0 for it.
func TestCache(t *testing.T) {
	t.Run("missing key", func(t *testing.T) {
		c := NewCache()
		if _, ok := c.Get("nope"); ok {
			t.Error("Get on empty cache returned ok")
		}
	})
	t.Run("set and get", func(t *testing.T) {
		c := NewCache()
		r := Result{IP: "1.2.3.4", Country: "US"}
		c.Set("direct", r)
		got, ok := c.Get("direct")
		if !ok || got.IP != r.IP || got.Country != r.Country || got.Err != nil {
			t.Errorf("Get = %+v ok=%v, want %+v", got, ok, r)
		}
	})
	t.Run("ttl expiry", func(t *testing.T) {
		c := NewCacheWithTTL(time.Minute)
		c.Set("direct", Result{IP: "1.2.3.4"})
		if _, ok := c.Get("direct"); !ok {
			t.Fatal("fresh entry not returned")
		}
		// Age the entry past the TTL deterministically (white-box: the
		// time.Since comparison is what matters, not real clock waits).
		c.mu.Lock()
		e := c.entries["direct"]
		e.At = time.Now().Add(-2 * time.Minute)
		c.entries["direct"] = e
		c.mu.Unlock()
		if _, ok := c.Get("direct"); ok {
			t.Error("expired entry still returned")
		}
	})
	t.Run("re-set refreshes ttl", func(t *testing.T) {
		c := NewCacheWithTTL(time.Minute)
		c.Set("direct", Result{IP: "1.1.1.1"})
		c.mu.Lock()
		e := c.entries["direct"]
		e.At = time.Now().Add(-59 * time.Second)
		c.entries["direct"] = e
		c.mu.Unlock()
		if _, ok := c.Get("direct"); !ok {
			t.Fatal("entry aged to 59s should still be fresh under a 60s TTL")
		}
		c.Set("direct", Result{IP: "2.2.2.2"})
		c.mu.Lock()
		refreshed := c.entries["direct"].At
		c.mu.Unlock()
		if time.Since(refreshed) > time.Second {
			t.Error("re-Set did not refresh the entry timestamp")
		}
	})
	t.Run("ttl zero expires immediately", func(t *testing.T) {
		c := NewCacheWithTTL(0)
		c.Set("direct", Result{IP: "1.2.3.4"})
		// With TTL=0 the entry expires as soon as the clock advances past
		// the Set instant (time.Since(e.At) > 0), so a Get racing the Set
		// on the same clock tick is undefined. Force the timestamp into the
		// past to pin the contract deterministically: an aged entry is
		// never returned.
		c.mu.Lock()
		e := c.entries["direct"]
		e.At = time.Now().Add(-time.Millisecond)
		c.entries["direct"] = e
		c.mu.Unlock()
		if _, ok := c.Get("direct"); ok {
			t.Error("Get with TTL=0 returned an entry whose timestamp is in the past")
		}
	})
}

// TestProbeBodyAndCancelEdges guards the remaining Probe branches: ctx
// cancellation mid-flight, opaque 200 bodies, CRLF line endings, and the
// scanner error path.
func TestProbeBodyAndCancelEdges(t *testing.T) {
	t.Run("ctx canceled mid-flight", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		accepted := make(chan struct{})
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			close(accepted)
			// Hold the connection open without responding; io.Copy ends
			// when the probe tears the connection down after cancel.
			_, _ = io.Copy(io.Discard, c)
			_ = c.Close()
		}()

		old := ProbeURL
		ProbeURL = "http://" + ln.Addr().String()
		defer func() { ProbeURL = old }()

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan Result, 1)
		go func() { done <- Probe(ctx, DirectDialer(5*time.Second), time.Minute) }()
		select {
		case <-accepted:
		case <-time.After(5 * time.Second):
			t.Fatal("probe never dialed")
		}
		cancel()
		select {
		case res := <-done:
			if !errors.Is(res.Err, context.Canceled) {
				t.Errorf("Err = %v, want context.Canceled", res.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("probe did not abort on cancel")
		}
	})

	t.Run("opaque 200 body is fail-open", func(t *testing.T) {
		ts := traceServer(t, "hello world\nnot a trace body\n", http.StatusOK)
		old := ProbeURL
		ProbeURL = ts.URL
		defer func() { ProbeURL = old }()
		res := Probe(context.Background(), DirectDialer(5*time.Second), 5*time.Second)
		if res.Err != nil {
			t.Fatalf("opaque body errored: %v", res.Err)
		}
		if res.IP != "" || res.Country != "" {
			t.Errorf("opaque body parsed %q/%q, want empty fields", res.IP, res.Country)
		}
	})

	t.Run("CRLF line endings parse", func(t *testing.T) {
		ts := traceServer(t, "ip=203.0.113.1\r\nloc=US\r\n", http.StatusOK)
		old := ProbeURL
		ProbeURL = ts.URL
		defer func() { ProbeURL = old }()
		res := Probe(context.Background(), DirectDialer(5*time.Second), 5*time.Second)
		if res.Err != nil {
			t.Fatalf("CRLF body errored: %v", res.Err)
		}
		if res.IP != "203.0.113.1" || res.Country != "US" {
			t.Errorf("CRLF body parsed %q/%q, want 203.0.113.1/US", res.IP, res.Country)
		}
	})

	t.Run("scanner error surfaces", func(t *testing.T) {
		// A line longer than bufio.Scanner's 64KB token cap makes the scan
		// fail; the probe must surface it as Err, not a partial result.
		big := strings.Repeat("x", 70*1024)
		ts := traceServer(t, "ip=203.0.113.1\n"+big+"\n", http.StatusOK)
		old := ProbeURL
		ProbeURL = ts.URL
		defer func() { ProbeURL = old }()
		res := Probe(context.Background(), DirectDialer(5*time.Second), 5*time.Second)
		if res.Err == nil {
			t.Fatal("oversized trace line did not fail the scanner")
			return
		}
		if !strings.Contains(res.Err.Error(), "trace body") {
			t.Errorf("Err = %v, want a trace-body read error", res.Err)
		}
	})
}
