package stealth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestLookup(t *testing.T) {
	tests := []struct {
		name   string
		want   *Profile
		wantOK bool
	}{
		{"chrome120", ProfileChrome120, true},
		{"Chrome120", ProfileChrome120, true},
		{"CHROME120", ProfileChrome120, true},
		{"safari17", ProfileSafari17, true},
		{"Safari17", ProfileSafari17, true},
		{"firefox120", ProfileFirefox120, true},
		{"Firefox120", ProfileFirefox120, true},
		{"random", ProfileRandom, true},
		{"Random", ProfileRandom, true},
		{"unknown", nil, false},
		{"", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Lookup(tt.name)
			if ok != tt.wantOK {
				t.Errorf("Lookup(%q) ok = %v, want %v", tt.name, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("Lookup(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
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
}

func TestSanitizeAndApply(t *testing.T) {
	h := http.Header{}
	h.Set("X-Forwarded-For", "1.2.3.4")
	h.Set("Authorization", "Bearer tok")

	SanitizeAndApply(h, ProfileChrome120)

	if v := h.Get("X-Forwarded-For"); v != "" {
		t.Errorf("proxy header not removed")
	}
	if v := h.Get("Authorization"); v != "Bearer tok" {
		t.Errorf("Authorization clobbered: %q", v)
	}
	if v := h.Get("User-Agent"); v != ProfileChrome120.UserAgent {
		t.Errorf("User-Agent = %q", v)
	}
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

	dialFN := Dialer(ProfileChrome120, nil, true)
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
	if dp != ProfileChrome120 {
		t.Errorf("DefaultProfile = %v, want ProfileChrome120", dp)
	}
}
