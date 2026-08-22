package upstream

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/stealth"
	"freebuff-proxy/internal/testutil"
)

func TestUAIsCLIUserAgent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = rc.Close()
		if got := mock.RecordedChatHeaders[i].Get("User-Agent"); got != cliUserAgent {
			t.Errorf("request %d UA = %q, want the fixed CLI UA %q", i, got, cliUserAgent)
		}
	}
}

func TestClientIDFormat(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := generateClientID()
		if !regexp.MustCompile(`^[0-9a-z]{13}$`).MatchString(id) {
			t.Fatalf("client_id %q not 13-char base36", id)
		}
	}
}

// TestGenerateClientIDFallbackPads verifies the time-seeded fallback never
// panics on a short base36 value: UnixNano in base36 is 12 digits today, and
// the old [:13] slice on it panicked whenever crypto/rand failed. The shared
// padBase36 helper must always yield the SDK's 13-char id.
func TestGenerateClientIDFallbackPads(t *testing.T) {
	for i := 0; i < 10; i++ {
		fallback := padBase36(strconv.FormatInt(time.Now().UnixNano(), 36))
		if !regexp.MustCompile(`^[0-9a-z]{13}$`).MatchString(fallback) {
			t.Fatalf("time fallback client_id %q not 13-char base36", fallback)
		}
	}
	if got := padBase36("abc"); got != "0000000000abc" {
		t.Errorf("padBase36(abc) = %q, want 0000000000abc (13 chars)", got)
	}
	if got := padBase36("0123456789abc"); got != "0123456789abc" {
		t.Errorf("padBase36(13-char) = %q, want unchanged", got)
	}
}

func TestNewTLSFingerprintInvalid(t *testing.T) {
	cfg := testConfig("", func(c *config.Config) { c.TLSFingerprint = "bogus" })
	_, err := New("tok", cfg)
	if err == nil {
		t.Fatal("New with bogus TLS_FINGERPRINT succeeded, want error")
	}
	if !strings.Contains(err.Error(), "TLS_FINGERPRINT") {
		t.Errorf("error = %q, want mention of TLS_FINGERPRINT", err)
	}
}

// TestStealthProfileResolvedOncePerRequest verifies that for TLS_FINGERPRINT
// auto/random the concrete profile is resolved ONCE per request: newRequest
// stashes it, and the dialer reads the same stash for the ClientHello — so
// the TLS fingerprint always matches the resolved profile. The request
// carries the CLI UA and NO browser headers (#109): header application is
// inverted — only the utls ClientHello impersonates the browser.
func TestStealthProfileResolvedOncePerRequest(t *testing.T) {
	client, err := New("tok-a", testConfig("", func(c *config.Config) { c.TLSFingerprint = "auto" }))
	if err != nil {
		t.Fatal(err)
	}
	req, err := client.newRequest(context.Background(), http.MethodGet, "/api/v1/freebuff/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	stashed := stealthProfileFrom(req.Context())
	if stashed == nil {
		t.Fatal("no concrete profile stashed in the request context")
	}
	if stashed.ID == stealth.ProfileIDAuto || stashed.ID == stealth.ProfileIDRandom {
		t.Fatalf("stashed profile %s is not concrete (auto must resolve once)", stashed.ID)
	}
	// The request carries the plain Bun fetch UA (session paths are bare
	// Bun traffic since G5), not the profile's browser UA.
	if got := req.Header.Get("User-Agent"); got != bunUserAgent {
		t.Errorf("request User-Agent %q != %q (no browser persona on API calls, #109)", got, bunUserAgent)
	}
	for _, hdr := range []string{"Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest",
		"Sec-CH-UA", "Sec-CH-UA-Mobile", "Sec-CH-UA-Platform"} {
		if got := req.Header.Get(hdr); got != "" {
			t.Errorf("%s = %q on an upstream API request, want absent (#109)", hdr, got)
		}
	}
	// The dialer must use the stashed profile for this request's dial.
	if dial := client.dialProfileFor(req.Context()); dial != stashed {
		t.Errorf("dialProfileFor(request ctx) = %p (%s), want the stashed profile %p", dial, dial.ID, stashed)
	}
	// A bare context (no stash) falls back to the unresolved profile; the
	// dialer resolves it per connection (pre-fix behavior for dials that
	// never went through newRequest).
	if dial := client.dialProfileFor(context.Background()); dial != stealth.ProfileAuto {
		t.Errorf("dialProfileFor(bare ctx) = %v, want ProfileAuto (dialer resolves per connection)", dial)
	}
	// Pinned profiles keep working unchanged.
	pinned, err := New("tok-a", testConfig("", func(c *config.Config) { c.TLSFingerprint = "chrome126" }))
	if err != nil {
		t.Fatal(err)
	}
	if dial := pinned.dialProfileFor(context.Background()); dial != stealth.ProfileChrome126 {
		t.Errorf("pinned dialProfileFor = %s, want chrome126", dial.ID)
	}
}

func TestNextStealthProfile(t *testing.T) {
	// Deterministic rotation across DISTINCT ClientHelloIDs.
	cases := []struct {
		cur  *stealth.Profile
		want *stealth.Profile
	}{
		{stealth.ProfileChrome120, stealth.ProfileSafari18},
		{stealth.ProfileChrome126, stealth.ProfileSafari18},
		{stealth.ProfileEdge126, stealth.ProfileSafari18},
		{stealth.ProfileSafari17, stealth.ProfileFirefox128},
		{stealth.ProfileSafari18, stealth.ProfileFirefox128},
		{stealth.ProfileFirefox120, stealth.ProfileChrome126},
		{stealth.ProfileFirefox128, stealth.ProfileChrome126},
	}
	for _, tc := range cases {
		if got := nextStealthProfile(tc.cur); got != tc.want {
			t.Errorf("nextStealthProfile(%s) = %s, want %s", tc.cur.ID, got.ID, tc.want.ID)
		}
	}
}

// TestNextStealthProfileUnknownFallback guards the unknown-profile fallback
// (G11): a profile outside the rotation order rotates to the first entry.
func TestNextStealthProfileUnknownFallback(t *testing.T) {
	got := nextStealthProfile(&stealth.Profile{ID: "bogus"})
	if want := retryProfileRotation[0].next; got != want {
		t.Errorf("nextStealthProfile(unknown) = %s, want %s", got.ID, want.ID)
	}
}
