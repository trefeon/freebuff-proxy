// Package stealth provides JA3 TLS fingerprint impersonation and browser
// header sanitization. It makes upstream connections indistinguishable from
// real browsers at the TLS layer by using utls ClientHello presets.
package stealth

import (
	cryptoRand "crypto/rand"
	"encoding/binary"
	"strings"

	utls "github.com/refraction-networking/utls"
)

// ProfileID uniquely identifies a browser fingerprint profile.
type ProfileID string

const (
	ProfileIDChrome120  ProfileID = "chrome120"
	ProfileIDChrome126  ProfileID = "chrome126"
	ProfileIDSafari17   ProfileID = "safari17"
	ProfileIDSafari18   ProfileID = "safari18"
	ProfileIDFirefox120 ProfileID = "firefox120"
	ProfileIDFirefox128 ProfileID = "firefox128"
	ProfileIDEdge126    ProfileID = "edge126"
	ProfileIDRandom     ProfileID = "random"
	ProfileIDAuto       ProfileID = "auto"
)
// Profile defines a complete browser TLS fingerprint including the utls
// ClientHelloID and matching HTTP headers.
type Profile struct {
	ID              ProfileID
	ClientHelloID   utls.ClientHelloID
	CustomSpec      *utls.ClientHelloSpec
	UserAgent       string
	SecChUA         string
	SecChUAPlatform string
	AcceptLanguage  string
	AcceptEncoding  string
}

// Pre-built browser profiles.
var (
	// ProfileChrome120 mimics Chrome 120 on Windows.
	ProfileChrome120 = &Profile{
		ID:              ProfileIDChrome120,
		ClientHelloID:   utls.HelloChrome_120,
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		SecChUA:         `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`,
		SecChUAPlatform: `"Windows"`,
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
	}

	// ProfileChrome126 mimics Chrome 126 on Windows (2024+).
	ProfileChrome126 = &Profile{
		ID:              ProfileIDChrome126,
		ClientHelloID:   utls.HelloChrome_120,
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		SecChUA:         `"Not/A)Brand";v="8", "Chromium";v="126", "Google Chrome";v="126"`,
		SecChUAPlatform: `"Windows"`,
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br, zstd",
	}

	// ProfileSafari17 mimics Safari 17 on macOS.
	ProfileSafari17 = &Profile{
		ID:             ProfileIDSafari17,
		ClientHelloID:  utls.HelloCustom,
		CustomSpec:     safari17Spec(),
		UserAgent:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15",
		AcceptLanguage: "en-US,en;q=0.9",
		AcceptEncoding: "gzip, deflate, br",
	}

	// ProfileSafari18 mimics Safari 18 on macOS (2024+).
	ProfileSafari18 = &Profile{
		ID:             ProfileIDSafari18,
		ClientHelloID:  utls.HelloCustom,
		CustomSpec:     safari17Spec(),
		UserAgent:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15",
		AcceptLanguage: "en-US,en;q=0.9",
		AcceptEncoding: "gzip, deflate, br",
	}

	// ProfileFirefox120 mimics Firefox 120 on Linux.
	ProfileFirefox120 = &Profile{
		ID:             ProfileIDFirefox120,
		ClientHelloID:  utls.HelloFirefox_120,
		UserAgent:      "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0",
		AcceptLanguage: "en-US,en;q=0.5",
		AcceptEncoding: "gzip, deflate, br",
	}

	// ProfileFirefox128 mimics Firefox 128 ESR on Linux (2024+).
	ProfileFirefox128 = &Profile{
		ID:             ProfileIDFirefox128,
		ClientHelloID:  utls.HelloFirefox_120,
		UserAgent:      "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0",
		AcceptLanguage: "en-US,en;q=0.5",
		AcceptEncoding: "gzip, deflate, br, zstd",
	}

	// ProfileEdge126 mimics Microsoft Edge 126 on Windows (2024+).
	ProfileEdge126 = &Profile{
		ID:              ProfileIDEdge126,
		ClientHelloID:   utls.HelloChrome_120,
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0",
		SecChUA:         `"Not/A)Brand";v="8", "Chromium";v="126", "Microsoft Edge";v="126"`,
		SecChUAPlatform: `"Windows"`,
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br, zstd",
	}

	// ProfileRandom picks a random fingerprint per connection.
	ProfileRandom = &Profile{
		ID:             ProfileIDRandom,
		ClientHelloID:  utls.HelloRandomized,
		AcceptLanguage: "en-US,en;q=0.9",
		AcceptEncoding: "gzip, deflate, br",
	}

	// ProfileAuto rotates across modern profiles per connection.
	ProfileAuto = &Profile{
		ID: ProfileIDAuto,
	}
)

// DefaultProfile returns Chrome 126 as the default modern profile.
func DefaultProfile() *Profile { return ProfileChrome126 }

// Lookup returns the profile matching the given name (case-insensitive)
// and true, or nil, false for unknown names.
func Lookup(name string) (*Profile, bool) {
	switch strings.ToLower(name) {
	case "chrome120":
		return ProfileChrome120, true
	case "chrome126", "chrome":
		return ProfileChrome126, true
	case "safari17":
		return ProfileSafari17, true
	case "safari18", "safari":
		return ProfileSafari18, true
	case "firefox120":
		return ProfileFirefox120, true
	case "firefox128", "firefox":
		return ProfileFirefox128, true
	case "edge126", "edge":
		return ProfileEdge126, true
	case "random":
		return ProfileRandom, true
	case "auto":
		return ProfileAuto, true
	default:
		return nil, false
	}
}

// GetProfileForConnection returns a concrete profile for one connection.
// For static profiles it returns p unchanged. For ProfileRandom or ProfileAuto,
// it resolves a fresh profile and User-Agent using crypto/rand (#3, #21).
func GetProfileForConnection(p *Profile) *Profile {
	if p == nil {
		p = DefaultProfile()
	}
	if p.ID == ProfileIDAuto {
		presets := []*Profile{
			ProfileChrome126,
			ProfileFirefox128,
			ProfileSafari18,
			ProfileEdge126,
		}
		idx := cryptoRandInt(len(presets))
		return presets[idx]
	}
	if p.ID == ProfileIDRandom {
		prof := *p
		prof.UserAgent = RandomUserAgent()
		return &prof
	}
	return p
}

// cryptoRandInt returns a crypto-random integer in [0, n).
func cryptoRandInt(n int) int {
	if n <= 0 {
		return 0
	}
	var b [8]byte
	_, _ = cryptoRand.Read(b[:])
	u := binary.BigEndian.Uint64(b[:])
	return int(u % uint64(n))
}

// RandomUserAgent generates a randomized browser User-Agent per connection
// using crypto/rand (#21).
func RandomUserAgent() string {
	agents := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:128.0) Gecko/20100101 Firefox/128.0",
		"Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0",
	}
	return agents[cryptoRandInt(len(agents))]
}


// safari17Spec returns the custom ClientHelloSpec for Safari 17 on macOS.
// Ported exactly from the reference implementation.
func safari17Spec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		CipherSuites: []uint16{
			utls.TLS_AES_128_GCM_SHA256,
			utls.TLS_AES_256_GCM_SHA384,
			utls.TLS_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			utls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_RSA_WITH_AES_128_CBC_SHA,
		},
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519,
				utls.CurveP256,
				utls.CurveP384,
				utls.CurveP521,
			}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256,
				utls.PSSWithSHA256,
				utls.PKCS1WithSHA256,
				utls.ECDSAWithP384AndSHA384,
				utls.ECDSAWithSHA1,
				utls.PSSWithSHA384,
				utls.PSSWithSHA512,
				utls.PKCS1WithSHA384,
				utls.PKCS1WithSHA512,
				utls.PKCS1WithSHA1,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519},
			}},
			&utls.SupportedVersionsExtension{Versions: []uint16{
				utls.GREASE_PLACEHOLDER,
				utls.VersionTLS13,
				utls.VersionTLS12,
			}},
			&utls.UtlsGREASEExtension{},
			&utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle},
		},
	}
}
