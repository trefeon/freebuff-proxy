// Package egress probes the gateway's outbound network path — the public IP
// and country code seen by a remote service — over the direct egress route.
// Results back the doctor's region row and give operators a fast
// ban-avoidance signal (requests unexpectedly appearing to originate from
// another country).
package egress

import (
	"bufio"
	"context"
	cryptoRand "crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"freebuff-proxy/internal/stealth"
)

// ProbeURL is the Cloudflare trace endpoint that reports the caller's
// public IP (ip=) and ISO country code (loc=). Exported so tests can point
// the probe at a local server; production never changes it.
var ProbeURL = "https://www.cloudflare.com/cdn-cgi/trace"

// DefaultTTL is how long a cached probe result stays fresh and the default
// interval between periodic probes when EGRESS_PROBE_ENABLED is set.
const DefaultTTL = 10 * time.Minute

// DefaultJitter is the symmetric ± fraction applied to the periodic probe
// interval so the loop never fires on a fixed clock (issue #123). It
// mirrors the official CLI's session-poll cadence of 30s ± 20% symmetric
// jitter (reference: use-freebuff-session.ts POLL_INTERVAL_ACTIVE_MS with
// utils/polling-backoff.ts jitterPollIntervalMs).
const DefaultJitter = 0.2

// ProbeTimeout bounds a single probe request end to end (dial, TLS, body).
const ProbeTimeout = 10 * time.Second

// Result is one probe: the public IP and 2-letter ISO country code seen at
// the far end of the egress path. Err carries the failure when the probe
// could not complete; callers treat that as "unknown egress" (fail-open).
type Result struct {
	IP      string
	Country string
	Err     error
}

// Probe GETs ProbeURL through dialer, bounded by timeout, and parses the
// ip= and loc= lines of the Cloudflare trace body. Any failure — dial,
// TLS, non-200 status, unreadable body — returns Result{Err: err}; the
// probe never retries and never touches the configured upstream auth.
func Probe(ctx context.Context, dialer func(ctx context.Context, network, addr string) (net.Conn, error), timeout time.Duration) Result {
	return probeOnce(ctx, timeout, dialer, nil)
}

// ProbeTLS is Probe for a DialTLSContext dialer — the utls stealth dialer
// the gateway already uses for API traffic (issue #123) — so the probe's
// ClientHello matches the gateway's other traffic instead of standing out
// as Go's default TLS fingerprint. A nil dialTLS falls back to the plain
// direct dialer, so callers can pass StealthDialer(fingerprint) and get a
// sane default when no fingerprint is configured.
func ProbeTLS(ctx context.Context, dialTLS func(ctx context.Context, network, addr string) (net.Conn, error), timeout time.Duration) Result {
	return probeOnce(ctx, timeout, nil, dialTLS)
}

// probeOnce is the shared Probe/ProbeTLS body. dialer is used as the
// transport's DialContext (plain TCP + Go TLS); when dialTLS is non-nil it
// replaces the TLS layer (utls stealth) and dialer is ignored.
func probeOnce(ctx context.Context, timeout time.Duration, dialer, dialTLS func(ctx context.Context, network, addr string) (net.Conn, error)) Result {
	if dialer == nil {
		dialer = DirectDialer(timeout)
	}
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	// Dedicated transport without ProxyFromEnvironment: the probe must go
	// through exactly the dialer given, not whatever env proxies exist.
	tr := &http.Transport{
		DialContext:         dialer,
		MaxIdleConns:        1,
		IdleConnTimeout:     DefaultTTL,
		TLSHandshakeTimeout: timeout,
	}
	if dialTLS != nil {
		tr.DialTLSContext = dialTLS
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: timeout}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, ProbeURL, nil)
	if err != nil {
		return Result{Err: err}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Result{Err: fmt.Errorf("egress probe: %s returned %s", ProbeURL, resp.Status)}
	}

	var ip, loc string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "ip="):
			ip = strings.TrimPrefix(line, "ip=")
		case strings.HasPrefix(line, "loc="):
			loc = strings.TrimPrefix(line, "loc=")
		}
	}
	if err := sc.Err(); err != nil {
		return Result{Err: fmt.Errorf("egress probe: reading trace body: %w", err)}
	}
	return Result{IP: ip, Country: loc}
}

// DirectDialer returns the dial function for the direct egress path: a
// plain net dialer with the given connection timeout.
func DirectDialer(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{Timeout: timeout}).DialContext
}

// Path identifies one egress path to probe: the cache key ("direct") and
// the dialer that routes the probe connection. DialTLS is optional: when
// set, the transport dials TLS through it (the utls stealth dialer, issue
// #123) instead of Go's default handshake.
type Path struct {
	Key     string
	Dialer  func(ctx context.Context, network, addr string) (net.Conn, error)
	DialTLS func(ctx context.Context, network, addr string) (net.Conn, error)
}

// StealthDialer returns a DialTLSContext for the probe that impersonates
// the given TLS fingerprint — the same utls dialer construction as the
// upstream API client (stealth.Dialer over the direct base dial, h1 ALPN)
// — so a probe request is indistinguishable from API traffic at the TLS
// layer (issue #123). Returns nil when fingerprint is empty or unknown;
// ProbeTLS then falls back to Go's plain TLS, so the call stays safe.
func StealthDialer(fingerprint string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if fingerprint == "" {
		return nil
	}
	profile, ok := stealth.Lookup(fingerprint)
	if !ok {
		return nil
	}
	// baseDial nil = the stealth dialer's default net.Dialer: direct egress
	// only, never a proxy. h1 ALPN matches the plain http.Transport below.
	return stealth.Dialer(profile, nil, false, []string{"http/1.1"})
}

// ProbeOnce runs a single probe pass over paths — one request per path,
// results stored into cache, successes fed to the risk engine, failures
// logged — and returns. The gateway uses it for the startup one-shot when
// the periodic loop is disabled (issue #123); RunLoop calls it for its
// first pass and each tick.
func ProbeOnce(ctx context.Context, logger *slog.Logger, cache *Cache, paths []Path, timeout time.Duration) {
	if cache == nil {
		panic("egress: ProbeOnce requires a non-nil cache")
	}
	if logger == nil {
		logger = slog.Default()
	}
	for key, r := range probeAll(ctx, paths, timeout) {
		cache.Set(key, r)
		if r.Err != nil {
			logger.Warn("egress probe failed", "path", key, "err", r.Err)
		} else {
			logger.Debug("egress probe", "path", key, "ip", r.IP, "country", r.Country)
			// Passive ban-risk feed (#64): every successful probe
			// contributes an egress-geo sample to the shared risk
			// engine. Read-only; the engine only warns.
			stealth.DefaultRiskEngine.Observe(stealth.RiskSample{
				At:       time.Now(),
				EgressIP: r.IP,
				Country:  r.Country,
			})
		}
	}
}

// RunLoop probes all paths once at startup, then on a jittered interval
// until ctx is canceled, storing each result into cache. Probe failures are
// logged and cached with Err set (fail-open); the loop keeps running. The
// interval is jittered ±DefaultJitter so the gateway never probes on a
// fixed clock (issue #123, mirroring the CLI's 30s ±20% poll cadence).
func RunLoop(ctx context.Context, logger *slog.Logger, cache *Cache, paths []Path, timeout, interval time.Duration) {
	if cache == nil {
		panic("egress: RunLoop requires a non-nil cache")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		// time.NewTimer panics on a non-positive duration; fall back to the
		// default so a misconfigured caller gets periodic probing instead of
		// a crash. (Audit B7.)
		interval = DefaultTTL
	}
	ProbeOnce(ctx, logger, cache, paths, timeout)
	timer := time.NewTimer(jittered(interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			ProbeOnce(ctx, logger, cache, paths, timeout)
			timer.Reset(jittered(interval))
		}
	}
}

// jittered returns interval adjusted by a symmetric ±DefaultJitter uniform
// draw, matching the official CLI's symmetric session-poll jitter
// (reference: utils/polling-backoff.ts jitterPollIntervalMs — 30s ± 20%).
// crypto/rand (not math/rand) matches the request-jitter pattern in the
// upstream client, so the cadence sequence is not reproducible from the
// process seed.
func jittered(interval time.Duration) time.Duration {
	if interval <= 0 {
		return interval
	}
	span := int64(float64(interval) * DefaultJitter)
	if span <= 0 {
		return interval
	}
	var b [8]byte
	_, _ = cryptoRand.Read(b[:])
	u := binary.BigEndian.Uint64(b[:])
	return interval - time.Duration(span) + time.Duration(u%(2*uint64(span)+1))
}

// probeAll probes every path concurrently and returns one Result per key.
// A failing path yields a Result with Err set (fail-open) and never aborts
// the other probes.
func probeAll(ctx context.Context, paths []Path, timeout time.Duration) map[string]Result {
	results := make(map[string]Result, len(paths))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(p Path) {
			defer wg.Done()
			var r Result
			if p.DialTLS != nil {
				r = ProbeTLS(ctx, p.DialTLS, timeout)
			} else {
				r = Probe(ctx, p.Dialer, timeout)
			}
			mu.Lock()
			results[p.Key] = r
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return results
}

type cachedResult struct {
	Result
	At time.Time
}

// Cache stores the latest probe result per egress path so the health
// surface and doctor can report the egress region without re-probing.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]cachedResult
	ttl     time.Duration
}

// NewCache returns a cache with the default 10-minute TTL.
func NewCache() *Cache { return NewCacheWithTTL(DefaultTTL) }

// NewCacheWithTTL returns a cache whose entries expire after ttl.
func NewCacheWithTTL(ttl time.Duration) *Cache {
	return &Cache{entries: make(map[string]cachedResult), ttl: ttl}
}

// Get returns the cached result for key when present and unexpired.
func (c *Cache) Get(key string) (Result, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	if !ok {
		return Result{}, false
	}
	if time.Since(e.At) > c.ttl {
		return Result{}, false
	}
	return e.Result, true
}

// Set stores the latest probe result for key, refreshing its timestamp.
func (c *Cache) Set(key string, r Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cachedResult{Result: r, At: time.Now()}
}
