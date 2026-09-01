package upstream

// Review-fix regression tests for the ads device-block derivation
// (devdocs/review-2026-08-31.md): the anti-ban fingerprint surface the
// waiting-room chain sends upstream in the /api/v1/ads device block.
// Pins the (os, timezone, locale) derivation contract plus the browser-UA
// table the device block's os must agree with (a mixed signal — e.g.
// os:"linux" with a Windows UA — reads as spoofing to ad networks).

import (
	"runtime"
	"strings"
	"testing"
	"time"

	// Hermetic timezone derivation: real IANA zone names must resolve via
	// LoadLocation on every test host (Windows ships no system zoneinfo).
	_ "time/tzdata"
)

// TestEgressDeviceBlockMatrix pins the device-block derivation matrix:
// platform mapping (Go darwin → wire macos, unknown → linux), POSIX locale
// parsing (LC_ALL → LC_MESSAGES → LANG precedence, charset strip, "_" → "-",
// C/POSIX skipped, en-US default), and timezone fallbacks (the "Local"
// placeholder and any name LoadLocation rejects fall back to UTC).
func TestEgressDeviceBlockMatrix(t *testing.T) {
	t.Run("deviceOSFor platform matrix", func(t *testing.T) {
		cases := []struct{ goos, want string }{
			{"darwin", "macos"}, // Go reports darwin; the API only accepts macos
			{"windows", "windows"},
			{"linux", "linux"},
			{"freebsd", "linux"}, // unrecognized → the CLI's linux fallback
			{"", "linux"},
		}
		for _, tc := range cases {
			if got := deviceOSFor(tc.goos); got != tc.want {
				t.Errorf("deviceOSFor(%q) = %q, want %q", tc.goos, got, tc.want)
			}
		}
	})

	t.Run("browser UA table agrees with the device os", func(t *testing.T) {
		// Exactly the CLI's three platforms; every entry must be the pinned
		// Chrome-124 UA carrying the platform marker its device os implies.
		markers := map[string]string{
			"macos":   "Macintosh",
			"windows": "Windows NT",
			"linux":   "X11; Linux",
		}
		if len(adUserAgents) != len(markers) {
			t.Errorf("adUserAgents has %d entries, want exactly the darwin/windows/linux table", len(adUserAgents))
		}
		for goos, ua := range adUserAgents {
			os := deviceOSFor(goos)
			marker, ok := markers[os]
			if !ok {
				t.Errorf("deviceOSFor(%q) = %q, not a wire os (macos|windows|linux)", goos, os)
				continue
			}
			if !strings.Contains(ua, marker) {
				t.Errorf("adUserAgents[%q] (os %q) missing platform marker %q: %q", goos, os, marker, ua)
			}
			if !strings.Contains(ua, "Chrome/124.0.0.0 Safari/537.36") {
				t.Errorf("adUserAgents[%q] is not the pinned Chrome-124 UA: %q", goos, ua)
			}
		}
		// The body UA is the host's table entry (linux fallback excluded:
		// unknown hosts are not exercised here — the table loop covers them).
		if ua, ok := adUserAgents[runtime.GOOS]; ok {
			if got := adBrowserUserAgent(); got != ua {
				t.Errorf("adBrowserUserAgent() = %q, want the %q table entry %q", got, runtime.GOOS, ua)
			}
		}
	})

	t.Run("ads request header UA stays the CLI product UA", func(t *testing.T) {
		if freebuffCliUA != "Freebuff-CLI/1.0.0" {
			t.Errorf("freebuffCliUA = %q, want the pinned Freebuff-CLI product UA", freebuffCliUA)
		}
	})

	t.Run("egressDeviceLocale POSIX parsing", func(t *testing.T) {
		cases := []struct {
			name               string
			lcAll, lcMsg, lang string
			want               string
		}{
			{"LC_ALL wins over the rest", "en_US.UTF-8", "fr_FR.UTF-8", "it_IT.UTF-8", "en-US"},
			{"charset stripped and underscore dashed", "zh_CN.GB2312", "", "", "zh-CN"},
			{"C falls through to LC_MESSAGES", "C", "pt_BR.UTF-8", "de_DE.UTF-8", "pt-BR"},
			{"C and POSIX both fall through to LANG", "C", "POSIX", "de_DE.UTF-8", "de-DE"},
			{"unset LC_ALL and LC_MESSAGES fall to LANG", "", "", "es_MX.UTF-8", "es-MX"},
			{"every value C/POSIX/empty falls to default", "C", "POSIX", "", "en-US"},
			{"all unset falls to default", "", "", "", "en-US"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("LC_ALL", tc.lcAll)
				t.Setenv("LC_MESSAGES", tc.lcMsg)
				t.Setenv("LANG", tc.lang)
				if got := egressDeviceLocale(); got != tc.want {
					t.Errorf("egressDeviceLocale(LC_ALL=%q, LC_MESSAGES=%q, LANG=%q) = %q, want %q",
						tc.lcAll, tc.lcMsg, tc.lang, got, tc.want)
				}
			})
		}
	})

	t.Run("egressDeviceTimezone fallbacks", func(t *testing.T) {
		// time.Local is the derivation's only seam (it reads
		// time.Local.String(), not the environment). Tests run sequentially
		// within the package, so the swap below is deterministic; the defer
		// restores the original zone before any later test runs.
		orig := time.Local
		defer func() { time.Local = orig }()
		cases := []struct {
			name  string
			local *time.Location
			want  string
		}{
			{"real IANA zone passes through", mustLoadZone(t, "America/New_York"), "America/New_York"},
			{"UTC passes through", time.UTC, "UTC"},
			{"Local placeholder falls back to UTC", time.FixedZone("Local", 0), "UTC"},
			{"name LoadLocation rejects falls back to UTC", time.FixedZone("Not/ARealZone", 3600), "UTC"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				time.Local = tc.local
				if got := egressDeviceTimezone(); got != tc.want {
					t.Errorf("egressDeviceTimezone(local=%v) = %q, want %q", tc.local, got, tc.want)
				}
			})
		}
	})
}

// mustLoadZone loads an IANA zone for the timezone table, failing the test
// (not silently degrading the pin) when even the embedded tzdata cannot
// resolve it.
func mustLoadZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("time.LoadLocation(%q) failed even with embedded tzdata: %v", name, err)
	}
	return loc
}
