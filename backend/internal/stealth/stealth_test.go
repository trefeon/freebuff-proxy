package stealth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

func TestLookup(t *testing.T) {
	tests := []struct {
		name   string
		wantID ProfileID
		wantOK bool
	}{
		{"chrome120", ProfileIDChrome120, true},
		{"chrome126", ProfileIDChrome126, true},
		{"chrome", ProfileIDChrome126, true},
		{"safari17", ProfileIDSafari17, true},
		{"safari18", ProfileIDSafari18, true},
		{"safari", ProfileIDSafari18, true},
		{"firefox120", ProfileIDFirefox120, true},
		{"firefox128", ProfileIDFirefox128, true},
		{"firefox", ProfileIDFirefox128, true},
		{"edge126", ProfileIDEdge126, true},
		{"edge", ProfileIDEdge126, true},
		{"random", ProfileIDRandom, true},
		{"auto", ProfileIDAuto, true},
		{"unknown", "", false},
	}
	for _, tt := range tests {
		got, ok := Lookup(tt.name)
		if ok != tt.wantOK {
			t.Errorf("Lookup(%q) ok = %v, want %v", tt.name, ok, tt.wantOK)
		}
		if ok && got.ID != tt.wantID {
			t.Errorf("Lookup(%q) ID = %q, want %q", tt.name, got.ID, tt.wantID)
		}
	}
}

func TestSanitizeHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Forwarded-For", "1.2.3.4")
	h.Set("Via", "1.1 proxy")
	h.Set("CF-Connecting-IP", "5.6.7.8")
	h.Set("True-Client-IP", "9.10.11.12")
	h.Set("X-Real-IP", "13.14.15.16")
	h.Set("X-Cache", "HIT")
	h.Set("Content-Type", "application/json")

	SanitizeHeaders(h)

	for _, hdr := range []string{"X-Forwarded-For", "Via", "CF-Connecting-IP", "True-Client-IP", "X-Real-IP", "X-Cache"} {
		if v := h.Get(hdr); v != "" {
			t.Errorf("header %q not removed: %s", hdr, v)
		}
	}
	if v := h.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", v)
	}
}

func TestApplyProfileHeaders(t *testing.T) {
	t.Run("Chrome", func(t *testing.T) {
		h := http.Header{}
		ApplyProfileHeaders(h, ProfileChrome120)

		if got := h.Get("User-Agent"); got == "" || got != ProfileChrome120.UserAgent {
			t.Errorf("User-Agent = %q, want Chrome UA", got)
		}
		if got := h.Get("Sec-CH-UA"); got != ProfileChrome120.SecChUA {
			t.Errorf("Sec-CH-UA = %q", got)
		}
		if got := h.Get("Sec-CH-UA-Mobile"); got != "?0" {
			t.Errorf("Sec-CH-UA-Mobile = %q, want ?0", got)
		}
		if got := h.Get("Sec-Fetch-Site"); got != "cross-site" {
			t.Errorf("Sec-Fetch-Site = %q, want cross-site", got)
		}
		if got := h.Get("Sec-Fetch-Mode"); got != "cors" {
			t.Errorf("Sec-Fetch-Mode = %q, want cors", got)
		}
		if got := h.Get("Sec-Fetch-Dest"); got != "empty" {
			t.Errorf("Sec-Fetch-Dest = %q, want empty", got)
		}
		if got := h.Get("Upgrade-Insecure-Requests"); got != "" {
			t.Errorf("Upgrade-Insecure-Requests = %q, want absent (only for navigation GETs)", got)
		}
	})

	t.Run("Firefox", func(t *testing.T) {
		h := http.Header{}
		ApplyProfileHeaders(h, ProfileFirefox120)

		if got := h.Get("Sec-CH-UA"); got != "" {
			t.Errorf("Firefox Sec-CH-UA = %q, want empty", got)
		}
		if got := h.Get("Sec-CH-UA-Mobile"); got != "" {
			t.Errorf("Firefox Sec-CH-UA-Mobile = %q, want empty (Firefox has no Client Hints)", got)
		}
		if got := h.Get("User-Agent"); got == "" || got != ProfileFirefox120.UserAgent {
			t.Errorf("User-Agent = %q", got)
		}
		if got := h.Get("Sec-Fetch-Site"); got != "cross-site" {
			t.Errorf("Sec-Fetch-Site = %q, want cross-site", got)
		}
	})

	t.Run("no-hint profile deletes stale Chromium hints", func(t *testing.T) {
		// Rotation path: a Chrome request retried under Safari/Firefox must
		// not keep the Chromium client-hint headers (they would mismatch the
		// new TLS fingerprint).
		h := http.Header{}
		ApplyProfileHeaders(h, ProfileChrome120)
		ApplyProfileHeaders(h, ProfileFirefox120)

		for _, hdr := range []string{"Sec-CH-UA", "Sec-CH-UA-Mobile", "Sec-CH-UA-Platform"} {
			if v := h.Get(hdr); v != "" {
				t.Errorf("%s = %q after Firefox apply, want deleted", hdr, v)
			}
		}
		if got := h.Get("User-Agent"); got != ProfileFirefox120.UserAgent {
			t.Errorf("User-Agent = %q, want Firefox UA", got)
		}
	})
}

func TestDialerTLS(t *testing.T) {
	// Generate a self-signed cert for the TLS server.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	ts := &http.Server{
		Addr:    "127.0.0.1:0",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ts.TLSConfig)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = ts.Serve(ln) }()
	defer func() { _ = ts.Close() }()

	addr := ln.Addr().String()

	dialFN := Dialer(ProfileChrome120, nil, true, nil)
	tr := &http.Transport{
		DialTLSContext: dialFN,
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("TLS dial failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestDefaultProfile(t *testing.T) {
	dp := DefaultProfile()
	if dp != ProfileChrome126 {
		t.Errorf("DefaultProfile = %v, want ProfileChrome126", dp)
	}
}

func TestGetProfileForConnection(t *testing.T) {
	t.Run("auto", func(t *testing.T) {
		p := GetProfileForConnection(ProfileAuto)
		if p == nil || p.ID == ProfileIDAuto {
			t.Errorf("GetProfileForConnection(ProfileAuto) returned unresolved profile: %v", p)
		}
	})
	t.Run("random", func(t *testing.T) {
		p := GetProfileForConnection(ProfileRandom)
		if p.UserAgent == "" {
			t.Errorf("GetProfileForConnection(ProfileRandom) returned empty User-Agent")
		}
	})
}

func TestProfileRandomClientHints(t *testing.T) {
	for i := 0; i < 50; i++ {
		p := GetProfileForConnection(ProfileRandom)
		if p.UserAgent == "" {
			t.Fatal("GetProfileForConnection(ProfileRandom) returned empty User-Agent")
		}
		h := http.Header{}
		ApplyProfileHeaders(h, p)
		if strings.Contains(p.UserAgent, "Chrome/") || strings.Contains(p.UserAgent, "Edg/") {
			if p.SecChUA == "" {
				t.Fatalf("Chromium UA %q had empty SecChUA", p.UserAgent)
			}
			if !strings.Contains(p.SecChUA, "Chromium") {
				t.Fatalf("Chromium UA %q SecChUA = %q, want Chromium brand", p.UserAgent, p.SecChUA)
			}
			if p.SecChUAPlatform == "" {
				t.Fatalf("Chromium UA %q had empty SecChUAPlatform", p.UserAgent)
			}
			if got := h.Get("Sec-CH-UA"); got != p.SecChUA {
				t.Fatalf("header Sec-CH-UA = %q, want %q", got, p.SecChUA)
			}
			if got := h.Get("Sec-CH-UA-Mobile"); got != "?0" {
				t.Fatalf("header Sec-CH-UA-Mobile = %q, want ?0", got)
			}
			if got := h.Get("Sec-CH-UA-Platform"); got != p.SecChUAPlatform {
				t.Fatalf("header Sec-CH-UA-Platform = %q, want %q", got, p.SecChUAPlatform)
			}
		} else {
			if p.SecChUA != "" || p.SecChUAPlatform != "" {
				t.Fatalf("non-Chromium UA %q has non-empty client hints: %q, %q", p.UserAgent, p.SecChUA, p.SecChUAPlatform)
			}
			if got := h.Get("Sec-CH-UA"); got != "" {
				t.Fatalf("non-Chromium header Sec-CH-UA = %q, want empty", got)
			}
		}
	}
}

// startTLSStub starts a local TLS server that completes any handshake and
// answers every request with 200, returning its host:port address.
func startTLSStub(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ts := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ts.TLSConfig)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = ts.Serve(ln) }()
	t.Cleanup(func() { _ = ts.Close() })
	return ln.Addr().String()
}

// freshSafariProfile returns a copy of a Safari profile with a FRESH
// ClientHelloSpec. The package-level profiles share one mutable
// utls.ClientHelloSpec singleton: utls's ApplyPreset writes the first
// connection's key-share into the shared KeyShareExtension, so the second
// connection reuses a stale public key with no matching private key and the
// TLS handshake fails ("local error: tls: internal error" / server
// "bad record MAC"). utls documents this: "Fields of TLSExtensions that are
// slices/pointers are shared across different connections with same
// ClientHelloSpec. It is advised to use different specs and avoid any shared
// state." Dialer now deep-clones the spec per connection (cloneSpec), so the
// shared singleton is never mutated; these fresh-spec profiles remain for
// tests that build their own.
func freshSafariProfile(id ProfileID) *Profile {
	p := *ProfileSafari18
	if id == ProfileIDSafari17 {
		p = *ProfileSafari17
	}
	p.CustomSpec = safari17Spec()
	return &p
}

// TestDialerSafariCustomSpec exercises the custom ClientHelloSpec path
// (G13): the Safari 17/18 profiles carry utls.HelloCustom plus a hand-built
// spec, so their handshake must complete against a real TLS server — the
// built-in presets alone do not cover this.
func TestDialerSafariCustomSpec(t *testing.T) {
	addr := startTLSStub(t)
	for _, prof := range []*Profile{freshSafariProfile(ProfileIDSafari17), freshSafariProfile(ProfileIDSafari18)} {
		t.Run(string(prof.ID), func(t *testing.T) {
			dialFN := Dialer(prof, nil, true, nil)
			conn, err := dialFN(context.Background(), "tcp", addr)
			if err != nil {
				t.Fatalf("handshake with custom spec failed: %v", err)
			}
			defer func() { _ = conn.Close() }()
		})
	}
}

// TestDialerDoesNotMutateSharedSpec is the regression for the shared-spec
// mutation bug: utls ApplyPreset writes the first connection's key shares
// and GREASE values into the spec's extension objects. Safari17/18 share one
// package-level singleton, so without a per-dial clone the second connection
// fails the handshake. The dial must leave the shared spec byte-identical
// (func fields aside — GetPaddingLen is code, never mutated).
func TestDialerDoesNotMutateSharedSpec(t *testing.T) {
	specBefore := cloneSpec(ProfileSafari18.CustomSpec)

	addr := startTLSStub(t)
	dialFN := Dialer(ProfileSafari18, nil, true, nil) // REAL shared singleton, not a fresh copy
	for range 5 {
		conn, err := dialFN(context.Background(), "tcp", addr)
		if err != nil {
			t.Fatalf("handshake through shared Safari18 spec failed: %v", err)
		}
		_ = conn.Close()
	}

	if !specEqualIgnoringFuncs(specBefore, cloneSpec(ProfileSafari18.CustomSpec)) {
		t.Fatal("Dialer mutated the shared Safari18 ClientHelloSpec; per-connection clone is broken")
	}
	// The clone helper itself must not alias the source's nested objects.
	if cloneSpec(ProfileSafari18.CustomSpec) == ProfileSafari18.CustomSpec {
		t.Fatal("cloneSpec aliased the source spec")
	}
}

// specEqualIgnoringFuncs reports deep equality between two specs, treating
// Func fields as always-equal (reflect.DeepEqual never considers two funcs
// equal, but utls specs carry a GetPaddingLen func that is code, never
// mutated state).
func specEqualIgnoringFuncs(a, b *utls.ClientHelloSpec) bool {
	return valueEqualIgnoringFuncs(reflect.ValueOf(a), reflect.ValueOf(b))
}

func valueEqualIgnoringFuncs(a, b reflect.Value) bool {
	switch a.Kind() {
	case reflect.Func:
		return true
	case reflect.Interface:
		return valueEqualIgnoringFuncs(a.Elem(), b.Elem())
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return valueEqualIgnoringFuncs(a.Elem(), b.Elem())
	case reflect.Slice:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		if a.Len() != b.Len() {
			return false
		}
		for i := range a.Len() {
			if !valueEqualIgnoringFuncs(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Array:
		if a.Len() != b.Len() {
			return false
		}
		for i := range a.Len() {
			if !valueEqualIgnoringFuncs(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Struct:
		for i := range a.NumField() {
			if !valueEqualIgnoringFuncs(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a.Interface(), b.Interface())
	}
}

// alpnCount returns how many ALPN extensions the UConn's extension list
// currently holds.
func alpnCount(u *utls.UConn) int {
	n := 0
	for _, e := range u.Extensions {
		if _, ok := e.(*utls.ALPNExtension); ok {
			n++
		}
	}
	return n
}

// stubConn is a net.Conn that performs no I/O; enough to build a utls.UConn
// for extension-list assertions (BuildHandshakeState never touches the
// connection).
type stubConn struct{}

func (stubConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (stubConn) Write(p []byte) (int, error)      { return len(p), nil }
func (stubConn) Close() error                     { return nil }
func (stubConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (stubConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (stubConn) SetDeadline(time.Time) error      { return nil }
func (stubConn) SetReadDeadline(time.Time) error  { return nil }
func (stubConn) SetWriteDeadline(time.Time) error { return nil }

// TestSetALPN guards setALPN (G14): an existing ALPN extension is replaced
// in place (no duplicate, no reorder), a missing one is appended, and
// re-running BuildHandshakeState after the mutation keeps the list stable.
func TestSetALPN(t *testing.T) {
	newUConn := func() *utls.UConn {
		return utls.UClient(stubConn{}, &utls.Config{ServerName: "example.com"}, utls.HelloChrome_120)
	}

	t.Run("replaces existing ALPN in place", func(t *testing.T) {
		u := newUConn()
		if err := u.BuildHandshakeState(); err != nil {
			t.Fatal(err)
		}
		if alpnCount(u) != 1 {
			t.Fatalf("chrome preset carries %d ALPN extensions, want 1", alpnCount(u))
		}
		before := len(u.Extensions)
		setALPN(u, []string{"http/1.1"})
		if alpnCount(u) != 1 {
			t.Errorf("setALPN left %d ALPN extensions, want 1 (in-place replace)", alpnCount(u))
		}
		if len(u.Extensions) != before {
			t.Errorf("extension count changed: %d -> %d", before, len(u.Extensions))
		}
		var got []string
		for _, e := range u.Extensions {
			if ext, ok := e.(*utls.ALPNExtension); ok {
				got = ext.AlpnProtocols
			}
		}
		if len(got) != 1 || got[0] != "http/1.1" {
			t.Errorf("ALPN protocols = %v, want [http/1.1]", got)
		}
		// Rebuilding with the mutated list must not duplicate or reorder.
		if err := u.BuildHandshakeState(); err != nil {
			t.Fatal(err)
		}
		if alpnCount(u) != 1 {
			t.Errorf("after rebuild ALPN count = %d, want 1", alpnCount(u))
		}
		if len(u.Extensions) != before {
			t.Errorf("extension count changed after rebuild: %d -> %d", before, len(u.Extensions))
		}
	})

	t.Run("appends when ALPN absent", func(t *testing.T) {
		u := newUConn()
		if err := u.BuildHandshakeState(); err != nil {
			t.Fatal(err)
		}
		for i, e := range u.Extensions {
			if _, ok := e.(*utls.ALPNExtension); ok {
				u.Extensions = append(u.Extensions[:i], u.Extensions[i+1:]...)
				break
			}
		}
		if alpnCount(u) != 0 {
			t.Fatal("failed to strip ALPN from the preset")
		}
		before := len(u.Extensions)
		setALPN(u, []string{"http/1.1"})
		if alpnCount(u) != 1 {
			t.Errorf("setALPN append produced %d ALPN extensions, want 1", alpnCount(u))
		}
		if len(u.Extensions) != before+1 {
			t.Errorf("extension count = %d, want %d", len(u.Extensions), before+1)
		}
	})
}

// closingConn records Close calls so tests can assert the raw connection is
// released on failure paths.
type closingConn struct {
	closed chan struct{}
	once   sync.Once
}

func (c *closingConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *closingConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *closingConn) Close() error                     { c.once.Do(func() { close(c.closed) }); return nil }
func (c *closingConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *closingConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *closingConn) SetDeadline(time.Time) error      { return nil }
func (c *closingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *closingConn) SetWriteDeadline(time.Time) error { return nil }

// TestDialerInvalidAddr guards the SplitHostPort failure path (G15): an
// address without a port must produce a wrapped error and close the raw
// connection.
func TestDialerInvalidAddr(t *testing.T) {
	closed := make(chan struct{})
	baseDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return &closingConn{closed: closed}, nil
	}
	dialFN := Dialer(ProfileChrome120, baseDial, true, nil)
	_, err := dialFN(context.Background(), "tcp", "missing-port")
	if err == nil {
		t.Fatal("dial with an invalid address succeeded")
		return
	}
	if !strings.Contains(err.Error(), "invalid address") {
		t.Errorf("err = %v, want mention of invalid address", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Error("raw connection not closed after invalid-address failure")
	}
}

// TestProfileResolutionEdgeCases guards GetProfileForConnection auto
// membership, random UA validity, and Lookup case-insensitivity (G16).
func TestProfileResolutionEdgeCases(t *testing.T) {
	t.Run("auto stays within modern set", func(t *testing.T) {
		allowed := map[ProfileID]bool{
			ProfileIDChrome126:  true,
			ProfileIDFirefox128: true,
			ProfileIDSafari18:   true,
			ProfileIDEdge126:    true,
		}
		for i := 0; i < 50; i++ {
			p := GetProfileForConnection(ProfileAuto)
			if p == nil || !allowed[p.ID] {
				t.Fatalf("auto resolved to %v, want one of {Chrome126, Firefox128, Safari18, Edge126}", p)
			}
		}
	})

	t.Run("random UA from known set", func(t *testing.T) {
		known := []string{
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
			"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:128.0) Gecko/20100101 Firefox/128.0",
			"Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0",
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0",
		}
		for i := 0; i < 50; i++ {
			ua := RandomUserAgent()
			found := false
			for _, a := range known {
				if ua == a {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("random UA %q is not in the known 7-agent set", ua)
			}
		}
		// A random connection profile must always carry a valid UA.
		for i := 0; i < 20; i++ {
			p := GetProfileForConnection(ProfileRandom)
			if p.UserAgent == "" {
				t.Fatal("GetProfileForConnection(ProfileRandom) returned an empty UA")
			}
		}
	})

	t.Run("lookup is case-insensitive", func(t *testing.T) {
		for _, name := range []string{"CHROME126", "Safari18", "FireFox120", "EDGE", "AUTO", "Random"} {
			p, ok := Lookup(name)
			if !ok || p == nil {
				t.Errorf("Lookup(%q) failed", name)
			}
		}
		if _, ok := Lookup("BOGUS"); ok {
			t.Error("Lookup(BOGUS) succeeded")
		}
	})
}

// writeCaptureConn records the first bytes written on the connection so
// tests can observe the ClientHello without parsing TLS records.
type writeCaptureConn struct {
	net.Conn
	first *[]byte
	once  sync.Once
}

func (w *writeCaptureConn) Write(p []byte) (int, error) {
	w.once.Do(func() { *w.first = append([]byte(nil), p...) })
	return w.Conn.Write(p)
}

// TestDialerProfileSwapChangesClientHello guards TLS-level fingerprint
// rotation (G17): the profile a retry rotates to must emit a different
// ClientHello at the dial layer, not just different headers.
func TestDialerProfileSwapChangesClientHello(t *testing.T) {
	addr := startTLSStub(t)
	capture := func(prof *Profile) []byte {
		t.Helper()
		var first []byte
		baseDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &writeCaptureConn{Conn: c, first: &first}, nil
		}
		conn, err := Dialer(prof, baseDial, true, nil)(context.Background(), "tcp", addr)
		if err != nil {
			t.Fatalf("%s handshake failed: %v", prof.ID, err)
		}
		_ = conn.Close()
		if len(first) == 0 {
			t.Fatalf("%s wrote no ClientHello bytes", prof.ID)
		}
		return first
	}

	chrome := capture(ProfileChrome126)
	// The profile chrome126 rotates to on retry; use a fresh spec so the
	// dial is hermetic (see freshSafariProfile).
	safari := capture(freshSafariProfile(ProfileIDSafari18))
	if bytes.Equal(chrome, safari) {
		t.Error("rotated profile emitted an identical ClientHello")
	}
}

// TestDialerALPNNegotiation guards the ALPN knob (issue #51): with
// ["h2","http/1.1"] (a real browser's ALPN) the dialer negotiates h2
// against an h2-capable server; with ["http/1.1"] it stays h1. The
// negotiated protocol MUST match the transport the caller wires up — h2
// ALPN with Go's h1 transport chokes on server SETTINGS frames.
func TestDialerALPNNegotiation(t *testing.T) {
	// A TLS server advertising h2 + http/1.1 (like a Cloudflare front).
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	ts.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	ts.StartTLS()
	defer ts.Close()

	negotiated := func(alpn []string) string {
		t.Helper()
		dialFN := Dialer(ProfileChrome120, nil, true, alpn)
		conn, err := dialFN(context.Background(), "tcp", ts.Listener.Addr().String())
		if err != nil {
			t.Fatalf("dial with ALPN %v failed: %v", alpn, err)
		}
		defer func() { _ = conn.Close() }()
		u, ok := conn.(*utls.UConn)
		if !ok {
			t.Fatalf("dial returned %T, want *utls.UConn", conn)
		}
		return u.ConnectionState().NegotiatedProtocol
	}

	if got := negotiated([]string{"h2", "http/1.1"}); got != "h2" {
		t.Errorf("h2 ALPN negotiated %q, want h2", got)
	}
	if got := negotiated([]string{"http/1.1"}); got != "http/1.1" {
		t.Errorf("h1 ALPN negotiated %q, want http/1.1", got)
	}
	// nil falls back to the h1 default (pre-#51 behavior).
	if got := negotiated(nil); got != "http/1.1" {
		t.Errorf("nil ALPN negotiated %q, want http/1.1 (default)", got)
	}
}

// TestProfileSelectionLogs verifies T18: every GetProfileForConnection
// resolution logs a Debug line naming the selected profile — static,
// auto-resolved, and random alike.
func TestProfileSelectionLogs(t *testing.T) {
	var sink bytes.Buffer
	SetLogger(slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { SetLogger(nil) })

	if p := GetProfileForConnection(ProfileChrome120); p != ProfileChrome120 {
		t.Fatalf("static selection = %v, want ProfileChrome120", p)
	}
	if !strings.Contains(sink.String(), "stealth profile selected") || !strings.Contains(sink.String(), "profile=chrome120") {
		t.Errorf("static profile selection not logged: %s", sink.String())
	}

	before := sink.Len()
	sel := GetProfileForConnection(ProfileAuto)
	if sel == nil || sel.ID == ProfileIDAuto {
		t.Fatal("auto profile not resolved to a concrete profile")
		return
	}
	after := sink.String()[before:]
	if !strings.Contains(after, "stealth profile selected") || !strings.Contains(after, "profile=") {
		t.Errorf("auto profile selection not logged: %s", after)
	}

	before = sink.Len()
	GetProfileForConnection(ProfileRandom)
	after = sink.String()[before:]
	if !strings.Contains(after, "stealth profile selected") || !strings.Contains(after, "profile=random") {
		t.Errorf("random profile selection not logged: %s", after)
	}
}
