// Package upstream implements the codebuff.com wire client with the CLI
// request envelope required to pass the free-mode gate
// (403 free_mode_cli_required): x-freebuff-* headers, codebuff_metadata,
// provider.data_collection=deny, forced streaming, and the JSON-quoted
// "cb_easp" stop sentinel. Error handling mirrors proxy-freebuff's recovery
// matrix: typed sentinels let callers refresh sessions, rotate runs, or cool
// down tokens.
package upstream

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/stealth"
)

// Client speaks the codebuff.com wire protocol for a single token.
type Client struct {
	token      string
	tokenIndex int // 0-based index into the pool's token list (0 for bridge clients)
	baseURL    string
	http       *http.Client

	requestTimeout     time.Duration
	sessionCallTimeout time.Duration
	requestJitter      time.Duration
	costMode           string
	userID             string // optional x-freebuff-acting-user-id (ACTING_USER_ID; see New's doc + client.go acting-user comment: only the token's OWN account id is safe)
	debugDump          bool

	// transientRetriesLimit is TRANSIENT_RETRIES: the maximum number of
	// additional attempts after a transient transport failure (0 disables
	// retries entirely). Only transport-level failures (dial/TLS/reset/EOF)
	// retry; classified upstream errors never do.
	transientRetriesLimit int

	// capacityDeferredRetries counts free_mode_capacity_deferred retries
	// served by this client: the free-tier capacity queue is retried
	// in-place against the SAME lease/session, bounded by the
	// TRANSIENT_RETRIES budget (per-request, tracked separately from
	// transient transport retries).
	capacityDeferredRetries atomic.Int64

	// stealthProfile is the active TLS fingerprint. profileMu guards swaps
	// made by the retry loop (rotating the pinned profile before a retry);
	// newRequest and the dialer read it per request/connection. nil means
	// the plain Go transport.
	profileMu      sync.Mutex
	stealthProfile *stealth.Profile

	// http2Upstream negotiates HTTP/2 with the upstream so the TLS ALPN list
	// matches real browsers ("h2,http/1.1") instead of the h1-only list that
	// is itself a JA4 ALPN mismatch (#51). false forces HTTP/1.1.
	http2Upstream bool

	// risk is the passive ban-risk engine fed from session/probe responses
	// (#64). Production always uses stealth.DefaultRiskEngine; nil disables
	// feeding (test seam).
	risk *stealth.RiskEngine

	// Counters surfaced via the pool snapshot for /metrics.
	transientRetries     atomic.Int64 // transient transport failures retried
	fingerprintRotations atomic.Int64 // pinned fingerprint swaps ahead of a retry

	// rateLimitEvents is the T7 rate-limit ledger: upstream rate-limit
	// classifications counted by body code (rate_limited, spend_limited,
	// ip_capped, insufficient_quota, limit_burst_rate,
	// free_mode_rate_limited, ...). rateLimitMu guards the map; values are
	// atomics so snapshot reads never race a concurrent classification.
	rateLimitMu     sync.Mutex
	rateLimitEvents map[string]*atomic.Int64

	// waitingRoomRequired records that the last upstream refusal was a 428
	// waiting_room_required (issue #94): the pre-session ad-chain + streak
	// flow must fire before the next session create (WAITING_ROOM_CHAIN
	// gate). Set by classifyError; consumed (cleared) by the pool's
	// acquire path when the chain fires.
	waitingRoomRequired atomic.Bool

	// authOnly marks a token-less client built by NewForAuth (issue #62):
	// newRequest must never attach auth headers (there is no credential),
	// and the /api/auth/cli/* flow uses its own login-request helper.
	authOnly bool

	// retryBackoff overrides the randomized 200-600ms pre-retry sleep (test
	// seam; nil uses the crypto/rand jitter).
	retryBackoff func() time.Duration
}

// TokenKey returns a stable, non-secret key derived from the client token
// for session-state persistence. The key is a SHA-256 hex digest of the raw
// token, so the token itself never appears in the persisted file.
func (c *Client) TokenKey() string {
	sum := sha256.Sum256([]byte(c.token))
	return hex.EncodeToString(sum[:])
}

// cliUserAgent mirrors the official CLI chat user agent: the pinned
// @codebuff/llm-providers version, NOT the CLI_VERSION knob
// (reference/freebuff model-provider.ts:150; llm-providers package.json
// 1.0.0). The upstream free-tier gate (403 free_mode_cli_required) keys on
// the CLI request envelope (x-freebuff-* headers, codebuff_metadata, forced
// streaming and the JSON-quoted "cb_easp" stop sentinel — see the package
// comment), but the server still fingerprints the UA. ChatCompletions is the
// ONLY caller:
// empirically + snapshot-verified, the real CLI emits this UA on chat only;
// every other upstream call goes through plain Bun fetch (#108/#109
// rationale superseded by newest-source evidence).
const cliUserAgent = "ai-sdk/openai-compatible/1.0.0/codebuff"

// bunUserAgent is the default Bun fetch User-Agent the real CLI's non-chat
// calls carry: session POST/GET/probe/DELETE, agent-runs START/FINISH,
// auth login code/status and usage all use bare fetch() with no UA override,
// so Bun sends its own default `Bun/<version>`. 1.3.14 matches the pinned
// reference/freebuff/.bun-version and the live probe.
const bunUserAgent = "Bun/1.3.14"

const (
	// maxErrorBodyRead caps the upstream error response body read for
	// classification and logging.
	maxErrorBodyRead = 2048
	// maxDumpRead caps the debug dump body read.
	maxDumpRead = 51200
)

// New builds the client for one token.
func New(token string, cfg *config.Config) (*Client, error) {
	return NewWithIndex(token, 0, cfg)
}

// NewWithIndex builds the client for token at tokenIndex (the token's
// 0-based position in the pool's token list). Egress is always DIRECT: this
// gateway spoofs the official FreeBuff CLI, which has no outbound proxy
// machinery anywhere, and the upstream server hard-blocks proxy/VPN/Tor
// egress — a proxy would only add ban risk.
func NewWithIndex(token string, tokenIndex int, cfg *config.Config) (*Client, error) {
	if token == "" {
		return nil, errors.New("upstream: empty token")
	}
	if cfg == nil {
		return nil, errors.New("upstream: nil config")
	}

	c := &Client{
		token:                 token,
		tokenIndex:            tokenIndex,
		baseURL:               cfg.UpstreamBaseURL,
		requestTimeout:        cfg.RequestTimeout,
		sessionCallTimeout:    cfg.SessionCallTimeout,
		requestJitter:         cfg.RequestJitter,
		costMode:              cfg.CostMode,
		userID:                cfg.ActingUserID,
		debugDump:             cfg.DebugDump,
		transientRetriesLimit: cfg.TransientRetries,
		http2Upstream:         cfg.HTTP2Upstream,
		risk:                  stealth.DefaultRiskEngine,
		rateLimitEvents:       make(map[string]*atomic.Int64),
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	var baseDial func(ctx context.Context, network, addr string) (net.Conn, error)

	var stealthProf *stealth.Profile
	if cfg.TLSFingerprint != "" {
		profile, ok := stealth.Lookup(cfg.TLSFingerprint)
		if !ok {
			return nil, fmt.Errorf("upstream: unknown TLS_FINGERPRINT %q", cfg.TLSFingerprint)
		}
		stealthProf = profile
	}

	// Direct egress only (no proxy support): this gateway spoofs the
	// official FreeBuff CLI, which has no proxy machinery, and the upstream
	// server hard-blocks proxy/VPN/Tor egress. The DefaultTransport clone
	// inherits http.ProxyFromEnvironment; disable it so an operator
	// HTTP_PROXY/HTTPS_PROXY env var never routes upstream traffic through a
	// proxy either (full egress control).
	transport.Proxy = nil

	if stealthProf != nil {
		// Resolve the profile per request (instead of capturing it) so a
		// transient retry can swap the pinned fingerprint without rebuilding
		// the transport: rotateStealthProfileForRetry swaps c.stealthProfile
		// and the next dial picks it up. For auto/random, newRequest resolves
		// a concrete profile and stashes it so the browser headers and the
		// ClientHello always match; dialProfileFor prefers that stash.
		// baseDial is nil on the direct-only path, so the stealth dialer
		// falls back to the default net.Dialer.
		// The ALPN list must match the transport that will speak next: h2
		// when the http2 transport below is registered, h1 otherwise.
		alpn := []string{"http/1.1"}
		if c.http2Upstream {
			alpn = h2ALPN()
		}
		transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return stealth.Dialer(c.dialProfileFor(ctx), baseDial, false, alpn)(ctx, network, addr)
		}
	}

	// HTTP/2 upstream (issue #51). Real browsers advertise "h2,http/1.1";
	// forcing h1-only at the TLS layer is itself a JA4 ALPN mismatch. With
	// the stealth profile the stdlib transport cannot dispatch HTTP/2 over a
	// *utls.UConn (its h2 path type-asserts the conn to *tls.Conn), so a
	// dedicated http2.Transport takes over the "https" scheme and dials with
	// the SAME utls dialer (which now advertises h2).
	//
	// KNOWN LIMITATION (documented): the standard http2 transport writes its
	// own SETTINGS/WINDOW_UPDATE frames (order EnablePush, InitialWindowSize,
	// MaxFrameSize, MaxHeaderListSize, HeaderTableSize) and no priority
	// frames — a real Chrome sends its own ordering plus priorities. The
	// values below approximate Chrome's SETTINGS (HEADER_TABLE_SIZE 65536,
	// INITIAL_WINDOW_SIZE 6291456, MAX_HEADER_LIST_SIZE 262144 per
	// reference/tls-client profiles), killing the JA4 ALPN mismatch; exact
	// per-profile SETTINGS-frame fingerprinting is not feasible with the
	// stdlib transport.
	//
	// HTTP2_UPSTREAM=false restores the previous h1-only behavior.
	if c.http2Upstream {
		if stealthProf != nil {
			h2t := &http2.Transport{
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					return stealth.Dialer(c.dialProfileFor(ctx), baseDial, false, h2ALPN())(ctx, network, addr)
				},
				MaxDecoderHeaderTableSize: 65536,   // Chrome SETTINGS_HEADER_TABLE_SIZE
				MaxHeaderListSize:         262_144, // Chrome SETTINGS_MAX_HEADER_LIST_SIZE
			}
			transport.RegisterProtocol("https", h2t)
			// The stdlib runs onceSetNextProtoDefaults on the
			// transport's first use; its bundled h2 configure would
			// call RegisterProtocol("https") again and panic — the
			// recovered error is logged as "protocol https already
			// registered" on every stealth client's first request.
			// The documented empty-TLSNextProto kill switch makes
			// protocols().HTTP2() false so that configure is skipped;
			// https dispatch to our custom h2t goes through
			// altProto (alternateRoundTripper) and is independent of
			// the bundled configure.
			transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		} else {
			// Plain Go transport: the stdlib already negotiates HTTP/2 by
			// default (the DefaultTransport clone carries
			// ForceAttemptHTTP2=true, and its bundled h2 transport handles
			// the ALPN dispatch because the TLS handshake is the stdlib's
			// own). HTTP2_UPSTREAM=false forces HTTP/1.1 instead — an empty
			// TLSNextProto map is the documented way to disable h2.
			if !c.http2Upstream {
				transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
			}
		}
	} else if stealthProf == nil {
		// HTTP2_UPSTREAM=false on the plain path: force HTTP/1.1 (the
		// stdlib would otherwise negotiate h2).
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	c.stealthProfile = stealthProf
	c.http = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			// Go strips Authorization/Cookie on cross-host redirects but not
			// x-codebuff-api-key, which carried the same raw token (defensive —
			// newRequest no longer sets it, #107). Drop both when the redirect
			// target is a different host OR downgrades the scheme https->http
			// (same host, plaintext) so the token never leaks to a redirect
			// target; same-scheme same-host redirects (e.g. CDN or bare-host
			// -> www) keep their credentials.
			if !strings.EqualFold(via[0].URL.Host, req.URL.Host) ||
				(strings.EqualFold(via[0].URL.Scheme, "https") && strings.EqualFold(req.URL.Scheme, "http")) {
				req.Header.Del("Authorization")
				req.Header.Del("x-codebuff-api-key")
			}
			return nil
		},
	}
	return c, nil
}

// reqIDKey carries the request correlation id (opts.RequestID) through the
// request context for the do()/retry log lines. The key type is unexported;
// the server threads the same id via ChatOptions.RequestID (its own
// unexported server-side key is separate).
type reqIDKey struct{}

// withReqID returns a context carrying the request correlation id.
func withReqID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqIDKey{}, id)
}

// ReqID returns the request correlation id carried in ctx, or "" when the
// call was not made through ChatCompletions with opts.RequestID set (e.g.
// session/run management calls).
func ReqID(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey{}).(string)
	return id
}

// requestProfileKey stashes the concrete stealth profile resolved for one
// request in its context, so the transport dialer builds the ClientHello
// from the SAME profile whose browser headers were applied (auto/random
// must not draw twice — headers and TLS fingerprint would mismatch).
type requestProfileKey struct{}

func withStealthProfile(ctx context.Context, p *stealth.Profile) context.Context {
	return context.WithValue(ctx, requestProfileKey{}, p)
}

func stealthProfileFrom(ctx context.Context) *stealth.Profile {
	if p, ok := ctx.Value(requestProfileKey{}).(*stealth.Profile); ok {
		return p
	}
	return nil
}

// BaseURL returns the upstream base URL this client dials (used by the pool
// to build probe clients with the same upstream the pooled tokens use).
func (c *Client) BaseURL() string { return c.baseURL }

// currentStealthProfile returns the active stealth profile (nil = plain Go
// transport). Guarded by profileMu: the retry loop swaps the pinned profile
// to rotate the fingerprint, so readers must take the lock.
func (c *Client) currentStealthProfile() *stealth.Profile {
	c.profileMu.Lock()
	defer c.profileMu.Unlock()
	return c.stealthProfile
}

// dialProfileFor returns the stealth profile the transport dialer should use
// for a connection under ctx. For ProfileAuto/ProfileRandom the concrete
// profile stashed by newRequest wins, so the ClientHello matches the profile
// resolved for that request; a bare context (no stash) resolves per
// connection as before. For pinned profiles the current c.stealthProfile is
// authoritative: the retry loop swaps it ahead of a retry, so the stash
// would be stale.
func (c *Client) dialProfileFor(ctx context.Context) *stealth.Profile {
	profile := c.currentStealthProfile()
	if profile != nil && (profile.ID == stealth.ProfileIDAuto || profile.ID == stealth.ProfileIDRandom) {
		if stashed := stealthProfileFrom(ctx); stashed != nil {
			return stashed
		}
	}
	return profile
}

// h2ALPN returns the ALPN list a real browser advertises — the JA4-correct
// fingerprint for HTTP/2 upstreams (#51).
var _h2ALPN = [2]string{"h2", "http/1.1"}

func h2ALPN() []string { return _h2ALPN[:] }

// TransientRetries returns how many transient transport failures were
// retried by this client (pool snapshot /metrics aggregation).
func (c *Client) TransientRetries() int64 { return c.transientRetries.Load() }

// CapacityDeferredRetries returns how many free_mode_capacity_deferred
// retries this client served (same-session retries under the
// TRANSIENT_RETRIES budget, issue #75).
func (c *Client) CapacityDeferredRetries() int64 { return c.capacityDeferredRetries.Load() }

// PendingWaitingRoomChain reports whether the client last classified a 428
// waiting_room_required (issue #94) and the pre-session chain has not been
// fired/cleared yet. The pool consults it before a session create when
// WAITING_ROOM_CHAIN is enabled.
func (c *Client) PendingWaitingRoomChain() bool { return c.waitingRoomRequired.Load() }

// ConsumeWaitingRoomChain clears the 428 flag and reports whether it was
// set (so the caller fires the chain exactly once per 428).
func (c *Client) ConsumeWaitingRoomChain() bool { return c.waitingRoomRequired.Swap(false) }

// FingerprintRotations returns how many times the pinned TLS fingerprint was
// rotated ahead of a retry (pool snapshot /metrics aggregation).
func (c *Client) FingerprintRotations() int64 { return c.fingerprintRotations.Load() }

// SetTransport replaces the HTTP transport backing the client. Exported as a
// test seam for retry-injection tests (substituting a flaky RoundTripper);
// production code never calls it.
func (c *Client) SetTransport(rt http.RoundTripper) { c.http.Transport = rt }
