// Chat request plumbing for the wire client: the request builder
// (newRequest), the retry loop (do) with its transient-failure detection,
// pinned TLS-fingerprint rotation and jittered backoff, the SSE stream
// plumbing (transparent decompression and cancel-aware response bodies),
// and the SDK-faithful client_id generator.
package upstream

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	cryptoRand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"

	"freebuff-proxy/internal/stealth"
	"freebuff-proxy/internal/telemetry"
)

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("upstream: build %s %s: %w", method, path, err)
	}
	// A bodyless POST/PUT/PATCH is trivially replayable on a transient
	// retry: give it a NoBody GetBody so do()'s TRANSIENT_RETRIES replay
	// works (a nil GetBody silently disables retries, which after #120
	// would break the bodyless session POST's transport-level retry). GETs
	// and DELETEs stay nil-GetBody (never retried — idempotent reads fail
	// fast and the poll loop's own backoff owns them).
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		if body == nil {
			req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.authOnly {
		// Token-less login-flow client (#62/#66): never send an empty
		// credential pair — the /api/auth/cli/* endpoints take the login
		// User-Agent instead (see authLoginRequest).
		req.Header.Del("Authorization")
	}
	// Content-Type only when a body is present (#120): the CLI sets it iff
	// body !== undefined (reference/freebuff codebuff-api.ts:344-346), so a
	// bodyless session POST must not carry it. Chat always has a body.
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// UA scoping (newest-CLI wire behavior, audit G5): the real CLI sends
	// the pinned llm-providers ai-sdk UA ONLY on chat; every other call
	// goes through plain Bun fetch, whose default UA is Bun/<version>
	// (.bun-version pins 1.3.14). newRequest therefore defaults to
	// bunUserAgent for all session/agent-runs/streak calls, and
	// ChatCompletions overrides with cliUserAgent. No browser headers on
	// any API path (#108/#109 fix option (a)): the utls ClientHello
	// impersonation stays, the browser header persona does not.
	// x-codebuff-api-key is never sent — Bearer is the only credential
	// (#107, reference/freebuff codebuff-api.ts:337-345).
	req.Header.Set("User-Agent", bunUserAgent)
	ctx = req.Context()
	if profile := c.currentStealthProfile(); profile != nil {
		// Resolve the concrete profile ONCE per request and stash it: the
		// dialer reads the stash for the ClientHello, so the TLS fingerprint
		// matches the profile. Pinned profiles resolve to themselves;
		// auto/random get one concrete draw. Only SanitizeHeaders runs here
		// (protective strip of proxy-identifying headers) — the profile's
		// browser headers are deliberately NOT applied to upstream API calls.
		connProf := stealth.GetProfileForConnection(profile)
		ctx = withStealthProfile(ctx, connProf)
		stealth.SanitizeHeaders(req.Header)
	}
	if ctx != req.Context() {
		req = req.WithContext(ctx)
	}
	return req, nil
}

// do executes req, enforcing the given timeout unless ctx already carries an
// earlier deadline. The returned cancel must be released once the caller is
// done with the response BODY: canceling the request context aborts in-flight
// body reads, so it must outlive body streaming. cancel is nil when no
// timeout was applied. Failures are wrapped so errors.Is works both ways.
//
// When TRANSIENT_RETRIES > 0, transport-level failures (dial/TLS handshake/
// reset/EOF) are retried up to that many additional attempts: the body is
// replayed from GetBody on a fresh connection (req.Close), the pinned TLS
// fingerprint is rotated, and a randomized 200-600ms backoff precedes each
// retry. Classified upstream errors (429/403/401, session/run invalids,
// waiting room), any HTTP status >= 400, context cancellation, and requests
// whose body cannot be replayed are NEVER retried.
func (c *Client) do(req *http.Request, timeout time.Duration) (*http.Response, context.CancelFunc, error) {
	ctx := req.Context()
	start := time.Now()
	var cancel context.CancelFunc
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		// The caller already bound the request. The control-call timeout is
		// still honored as an upper bound when it is the TIGHTER of the two:
		// a long caller deadline (e.g. a 15m request timeout) must not
		// silently defeat SessionCallTimeout on session/run control calls.
		if timeout > 0 {
			if remaining := time.Until(deadline); timeout < remaining {
				ctx, cancel = context.WithTimeout(ctx, timeout)
				req = req.WithContext(ctx)
			}
		}
	} else if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		req = req.WithContext(ctx)
	}

	// Capture the body so a transient failure can replay an identical
	// request. nil bodies (GETs) and non-replayable bodies never retry.
	var replayBody func() (io.ReadCloser, error)
	if req.GetBody != nil {
		replayBody = req.GetBody
	}

	for attempt := 1; ; attempt++ {
		resp, err := c.http.Do(req)
		if err == nil {
			if werr := wrapDecompress(resp); werr != nil {
				_ = resp.Body.Close()
				if cancel != nil {
					cancel()
				}
				return nil, nil, fmt.Errorf("upstream: %s %s: %w", req.Method, req.URL.Path, werr)
			}
			if resp.StatusCode >= 400 {
				// T5 wire transparency: error responses are read (2KB cap),
				// logged as `upstream response` (redacted, ≤500 runes), and
				// re-wrapped so the caller's classification parses the same
				// body. Never logged as `upstream ok` — a transport-level
				// 200 and an upstream 429 are different classes of event.
				bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
				_ = resp.Body.Close()
				bodyText := telemetry.RedactSecrets(string(bodyBytes))
				class := errClassName(classifyError(resp.StatusCode, bodyText, resp.Header))
				attrs := []any{
					"method", req.Method, "path", req.URL.Path,
					"status", resp.StatusCode, "ms", time.Since(start).Milliseconds(),
					"class", class,
					"body", truncateRunes(bodyText, 500),
				}
				if reqID := ReqID(ctx); reqID != "" {
					attrs = append(attrs, "req_id", reqID)
				}
				slog.Debug("upstream response", attrs...)
				resp.Body = io.NopCloser(strings.NewReader(bodyText))
				return resp, cancel, nil
			}
			slog.Debug("upstream ok", "method", req.Method, "path", req.URL.Path,
				"status", resp.StatusCode, "ms", time.Since(start).Milliseconds(),
				"req_id", ReqID(ctx))
			return resp, cancel, nil
		}

		// Transient transport failure with attempts remaining: rotate the
		// pinned fingerprint, replay the body on a fresh connection, and
		// retry after a jittered backoff.
		if c.transientRetriesLimit > 0 && attempt <= c.transientRetriesLimit &&
			ctx.Err() == nil && replayBody != nil && isTransient(err) {
			c.rotateStealthProfileForRetry(req)
			body, bodyErr := replayBody()
			if bodyErr != nil {
				slog.Debug("upstream retry aborted: body replay failed",
					"token", c.tokenIndex+1, "attempt", attempt, "err", bodyErr,
					"req_id", ReqID(ctx))
			} else {
				// Count the retry only once the replay succeeded: the counter
				// reflects retries that actually fired, not aborted ones.
				c.transientRetries.Add(1)
				req.Body = body
				req.Close = true // fresh connection for the retry
				slog.Debug("upstream transient failure, retrying",
					"token", c.tokenIndex+1, "attempt", attempt, "reason", err.Error(),
					"path", req.URL.Path, "req_id", ReqID(ctx))
				timer := time.NewTimer(c.retryDelay())
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
				}
				if ctx.Err() == nil {
					continue
				}
				// Context died during the backoff: a retry would fail
				// instantly, surface the context error instead.
				err = ctx.Err()
			}
		}

		slog.Debug("upstream error", "method", req.Method, "path", req.URL.Path,
			"ms", time.Since(start).Milliseconds(), "err", err, "req_id", ReqID(ctx))
		if cancel != nil {
			cancel()
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, nil, context.Canceled
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, nil, fmt.Errorf("%w: %s %s", context.DeadlineExceeded, req.Method, req.URL.Path)
		}
		return nil, nil, fmt.Errorf("upstream: %s %s: %w", req.Method, req.URL.Path, err)
	}
}

// transientMarkers are transport-level failure signatures that are safe to
// retry: the request never reached the application layer, so no upstream
// quota/credits were burned and nothing was processed. Classified upstream
// errors (429/403/401, session/run invalids, waiting room) and any HTTP
// status >= 400 are handled at the response layer and never enter this path.
// Markers are lowercase: isTransient lowercases the wrapped error messages
// before matching. "tls: handshake failure" is Go's own alert string;
// "tls handshake failed" appears in wrapper libraries (e.g. stealth/uTLS).
var transientMarkers = []string{
	"tls handshake failed",
	"tls: handshake failure",
	"tls: internal error",
	"connection refused",
	"connection reset",
	"unexpected eof",
	"network is unreachable",
	"no route to host",
	"i/o timeout", // dial timeout
}

// isTransient reports whether err is a transient transport failure safe to
// retry. It walks the wrapped error chain and matches message fragments, so
// stealth-wrapped dial errors ("stealth: tcp dial failed: ...: connection
// refused") classify the same as the bare dial error.
//
// Bare "EOF" is matched on exact whole-message equality only: a substring
// match on "eof" would over-retry unrelated errors that merely mention the
// letters ("... eof marker ...").
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		msg := strings.ToLower(cur.Error())
		for _, marker := range transientMarkers {
			if strings.Contains(msg, marker) {
				return true
			}
		}
		if msg == "eof" {
			return true
		}
	}
	return false
}

// retryProfileRotation is the pinned-profile rotation order for transient
// retries: one entry per distinct ClientHelloID, so a retry presents a
// genuinely different JA3 (rotating chrome120 -> chrome126 would change only
// headers, not the TLS fingerprint). ProfileRandom/ProfileAuto are excluded:
// they already resolve a fresh fingerprint per connection.
var retryProfileRotation = []struct {
	ids  []stealth.ProfileID
	next *stealth.Profile
}{
	{ids: []stealth.ProfileID{stealth.ProfileIDChrome120, stealth.ProfileIDChrome126, stealth.ProfileIDEdge126}, next: stealth.ProfileSafari18},
	{ids: []stealth.ProfileID{stealth.ProfileIDSafari17, stealth.ProfileIDSafari18}, next: stealth.ProfileFirefox128},
	{ids: []stealth.ProfileID{stealth.ProfileIDFirefox120, stealth.ProfileIDFirefox128}, next: stealth.ProfileChrome126},
}

// rotateStealthProfileForRetry swaps the pinned TLS fingerprint to a
// different profile before a retry so the retried connection does not repeat
// the fingerprint that just failed. The request keeps its CLI headers —
// only proxy-identifying headers are re-stripped; no browser persona is
// applied on API paths (#109). random/auto already rotate per connection and
// are left alone. No-op when retries are disabled or no fingerprint is
// pinned.
func (c *Client) rotateStealthProfileForRetry(req *http.Request) {
	c.profileMu.Lock()
	defer c.profileMu.Unlock()
	if c.transientRetriesLimit <= 0 || c.stealthProfile == nil {
		return
	}
	id := c.stealthProfile.ID
	if id == stealth.ProfileIDRandom || id == stealth.ProfileIDAuto {
		return
	}
	next := nextStealthProfile(c.stealthProfile)
	if next.ID == id {
		return
	}
	c.stealthProfile = next
	c.fingerprintRotations.Add(1)
	stealth.SanitizeHeaders(req.Header)
}

// nextStealthProfile returns the profile to rotate to after cur: the next
// entry in the fixed rotation order whose ClientHelloID differs from cur's.
func nextStealthProfile(cur *stealth.Profile) *stealth.Profile {
	for _, entry := range retryProfileRotation {
		for _, id := range entry.ids {
			if id == cur.ID {
				return entry.next
			}
		}
	}
	return retryProfileRotation[0].next
}

// retryDelay returns the sleep before a transient retry: a randomized
// 200-600ms backoff using crypto/rand (matching the request-jitter pattern).
// Tests pin it via Client.retryBackoff.
func (c *Client) retryDelay() time.Duration {
	if c.retryBackoff != nil {
		return c.retryBackoff()
	}
	var b [8]byte
	_, _ = cryptoRand.Read(b[:])
	u := binary.BigEndian.Uint64(b[:])
	return 200*time.Millisecond + time.Duration(u%uint64(400*time.Millisecond))
}

// wrapDecompress replaces resp.Body with a transparent decompressing reader
// when the upstream compresses the response. This is REQUIRED with the
// stealth profile: the browser Accept-Encoding ("gzip, deflate, br") makes
// Go's transport skip its automatic gzip handling (that only kicks in when
// Go itself set the header), so compressed bodies would arrive as garbage.
// The plain transport sends no Accept-Encoding and is unaffected (Go
// decompresses its own gzip transparently and strips the header).
func wrapDecompress(resp *http.Response) error {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return nil
	}
	underlying := resp.Body
	switch enc {
	case "gzip":
		zr, err := gzip.NewReader(underlying)
		if err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		resp.Body = &decompressCloser{Reader: zr, underlying: underlying}
	case "deflate":
		// RFC 9110 §8.4.1.3 defines Content-Encoding: deflate as a
		// zlib-wrapped stream (RFC 1950), but some servers historically
		// send raw DEFLATE (RFC 1951). Sniff the zlib header (CMF/FLG:
		// CM=8, CINFO<=7, 16-bit header a multiple of 31) WITHOUT
		// consuming bytes — a consumed header would corrupt the raw
		// fallback — and decode accordingly. (Audit B1: the raw-only
		// reader broke mid-stream on conforming zlib responses.)
		br := bufio.NewReader(underlying)
		head, _ := br.Peek(2)
		if len(head) == 2 && head[0]&0x0f == 8 && head[0]>>4 <= 7 &&
			(uint16(head[0])<<8|uint16(head[1]))%31 == 0 {
			zr, err := zlib.NewReader(br)
			if err != nil {
				return fmt.Errorf("deflate: %w", err)
			}
			resp.Body = &decompressCloser{Reader: zr, underlying: underlying}
		} else {
			resp.Body = &decompressCloser{Reader: flate.NewReader(br), underlying: underlying}
		}
	case "br":
		resp.Body = &decompressCloser{Reader: brotli.NewReader(underlying), underlying: underlying}
	case "zstd":
		// The stealth profiles advertise zstd in Accept-Encoding, so the
		// upstream may legitimately respond with it.
		zr, err := zstd.NewReader(underlying, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return fmt.Errorf("zstd: %w", err)
		}
		// zstd decoders are stateful (per-response buffers), unlike
		// gzip/brotli: Close must release the decoder's resources, not just
		// the underlying socket. (Audit B9.)
		resp.Body = &decompressCloser{Reader: zr, underlying: underlying, closeFn: func() error { zr.Close(); return nil }}
	default:
		return fmt.Errorf("unsupported Content-Encoding %q", enc)
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	return nil
}

// decompressCloser bridges a decompressing reader back to the underlying
// response body so Close always reaches the socket. closeFn optionally
// releases decoder-local resources (e.g. a zstd decoder's buffers) that are
// distinct from the underlying stream.
type decompressCloser struct {
	io.Reader
	underlying io.ReadCloser
	closeFn    func() error
}

func (d *decompressCloser) Close() error {
	if d.closeFn != nil {
		_ = d.closeFn()
	}
	return d.underlying.Close()
}

// releaseCancel cancels a do() timeout context unless it is nil.
func releaseCancel(cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
}

// cancelBody closes the underlying body and then releases the request
// context, so a streamed response body lives exactly as long as its reader.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	releaseCancel(b.cancel)
	return err
}

// generateClientID mints the SDK-faithful 13-char base36 client id
// (Math.random().toString(36).substring(2, 15)).
func generateClientID() string {
	var b [16]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable in practice; fall back to a
		// time-seeded value rather than panicking mid-request. UnixNano in
		// base36 is only 12 digits today, so pad to the SDK's 13-char length
		// (the old [:13] slice panicked on short values).
		return padBase36(strconv.FormatInt(time.Now().UnixNano(), 36))
	}
	n := new(big.Int).SetBytes(b[:])
	mod := new(big.Int).Exp(big.NewInt(36), big.NewInt(13), nil)
	return padBase36(n.Mod(n, mod).Text(36))
}

// padBase36 left-pads a base36 string with '0' to the SDK-faithful 13-char
// client id length. Both the crypto/rand draw and the time-seeded fallback
// need it: the latter is 12 digits, which would otherwise come out shorter
// than the JS substring(2, 15) equivalent.
func padBase36(id string) string {
	for len(id) < 13 {
		id = "0" + id
	}
	return id
}
