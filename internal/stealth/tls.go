package stealth

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Dialer returns a DialTLSContext function for http.Transport that uses
// utls to impersonate a specific browser's TLS fingerprint (JA3).
//
// By sending a ClientHello matching a real browser (Chrome, Safari, Firefox),
// the connection is indistinguishable from a genuine browser session at the
// TLS layer — defeating JA3/JA3S fingerprinting deployed by CDN/WAF
// infrastructure.
//
// baseDial provides the underlying TCP dial (e.g. SOCKS5). When nil, a
// default net.Dialer with 30s timeout is used.
func Dialer(profile *Profile, baseDial func(ctx context.Context, network, addr string) (net.Conn, error), insecureSkipVerify bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if profile == nil {
		profile = DefaultProfile()
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		connProfile := GetProfileForConnection(profile)
		dialFN := baseDial
		if dialFN == nil {
			dialFN = (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext
		}

		rawConn, err := dialFN(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("stealth: tcp dial failed: %w", err)
		}

		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("stealth: invalid address %q: %w", addr, err)
		}

		helloID := connProfile.ClientHelloID

		uConn := utls.UClient(rawConn, &utls.Config{
			ServerName:         host,
			InsecureSkipVerify: insecureSkipVerify,
			MinVersion:         tls.VersionTLS12,
		}, helloID)

		if connProfile.CustomSpec != nil {
			if err := uConn.ApplyPreset(connProfile.CustomSpec); err != nil {
				_ = rawConn.Close()
				return nil, fmt.Errorf("stealth: apply custom spec failed: %w", err)
			}
		}

		// Materialize the preset's extensions first, then pin ALPN to
		// HTTP/1.1. The browser presets advertise "h2,http/1.1" and upstream
		// would negotiate h2 — but Go's http.Transport cannot use HTTP/2
		// over a *utls.UConn (its h2 path type-asserts the connection to
		// *tls.Conn), so it falls back to HTTP/1.x and chokes on the
		// server's h2 SETTINGS frame ("malformed HTTP response").
		//
		// BuildHandshakeState() must run BEFORE the mutation: the first
		// build applies the preset spec (clobbering uconn.Extensions), and
		// every later build re-applies the (mutated) extension list.
		// JA3 hashes extension types, not ALPN values, so the fingerprint is
		// unaffected.
		if err := uConn.BuildHandshakeState(); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("stealth: build handshake state failed: %w", err)
		}
		setALPN(uConn, []string{"http/1.1"})

		if err := uConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("stealth: tls handshake failed: %w", err)
		}

		return uConn, nil
	}
}

// setALPN replaces (or appends) the ALPN extension on a utls UConn before
// the handshake. utls UConn exposes its extension list for mutation; the
// preset/custom-spec ALPN entry is replaced in place so no other extension
// ordering is disturbed.
func setALPN(uConn *utls.UConn, protocols []string) {
	ext := &utls.ALPNExtension{AlpnProtocols: protocols}
	for i, e := range uConn.Extensions {
		if _, ok := e.(*utls.ALPNExtension); ok {
			uConn.Extensions[i] = ext
			return
		}
	}
	uConn.Extensions = append(uConn.Extensions, ext)
}
