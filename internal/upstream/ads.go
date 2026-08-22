package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// freebuffCliUA is the ads-API request User-Agent, mirroring the installed
// official CLI binary the proxy emulates (reference
// cli/src/hooks/use-gravity-ad.ts getCliAdRequestUserAgent:
// "Freebuff-CLI/<CODEBUFF_CLI_VERSION>"; 1.0.0 = cli/package.json version
// at reference/freebuff @19d905d).
const freebuffCliUA = "Freebuff-CLI/1.0.0"

const (
	// maxAdResponseRead caps the ad response body read.
	maxAdResponseRead = 512
)

// adUserAgents maps runtime.GOOS to the browser-like Chrome-124 UA sent to
// ad providers for targeting/fraud screening (#124). The CLI ships one entry
// per platform (reference common/src/util/ad-user-agent.ts: darwin/win32/
// linux AD_USER_AGENTS; use-gravity-ad.ts sends it as the body userAgent)
// and warns that native runtime UAs look bot-like to ad networks. The body
// UA must agree with the device block's os (deviceOS): a mixed signal (e.g.
// os:"linux" with a Windows UA) reads as spoofing to ad networks.
var adUserAgents = map[string]string{
	"darwin":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"windows": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"linux":   "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
}

// adBrowserUserAgent returns the platform-consistent ads body UA for the
// host, falling back to the Linux entry exactly like the CLI's
// getAdUserAgent (AD_USER_AGENTS[platformKey] ?? linux).
func adBrowserUserAgent() string {
	if ua, ok := adUserAgents[runtime.GOOS]; ok {
		return ua
	}
	return adUserAgents["linux"]
}

// waitingRoomChainTimeout bounds the whole best-effort pre-session chain so
// a hung upstream never blocks a session create for long.
const waitingRoomChainTimeout = 15 * time.Second

// FireWaitingRoomChain runs the reference pre-session flow (issue #94(b),
// WAITING_ROOM_CHAIN gate): POST /api/v1/ads per configured ad provider,
// then GET /api/v1/freebuff/streak — mirroring freebuff2api-optimized
// codebuff.py _request_ads_and_streak (surface="waiting_room"). Strictly
// best-effort: every failure is logged and swallowed; the caller must never
// depend on it (a gated stub whose real value is keeping the account's
// waiting-room requirement satisfied before the next session create). The
// streak call fires once after the provider loop, matching the reference.
func (c *Client) FireWaitingRoomChain(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, waitingRoomChainTimeout)
	defer cancel()
	for _, provider := range waitingRoomAdProviders {
		if err := c.requestAds(ctx, provider); err != nil {
			slog.Debug("waiting room chain: ads request failed", "provider", provider, "err", err)
		}
	}
	if err := c.getStreak(ctx); err != nil {
		slog.Debug("waiting room chain: streak request failed", "err", err)
	}
}

// waitingRoomAdProviders mirrors the reference default
// (freebuff2api-optimized config.py: ad_providers=("gravity","zeroclick")).
var waitingRoomAdProviders = []string{"gravity", "zeroclick"}

// requestAds POSTs one /api/v1/ads payload (reference cli/src/hooks/
// use-gravity-ad.ts fetchAd + common/src/util/ad-user-agent.ts: provider +
// device block + browser-like body userAgent + Freebuff-CLI header UA).
// Faithful details kept: messages stays [] and sessionId is omitted (the
// chain fires before a session exists — a fresh waiting-room).
func (c *Client) requestAds(ctx context.Context, provider string) error {
	payload := map[string]any{
		"provider": provider,
		"messages": []any{},
		"device": map[string]any{
			"os":       deviceOS(),
			"timezone": egressDeviceTimezone(),
			"locale":   egressDeviceLocale(),
		},
		// Body userAgent: the shared browser-like UA (NOT a runtime UA) so
		// every ad provider sees a usable targeting signal — the CLI sends
		// getAdUserAgent() here (#124).
		"userAgent": adBrowserUserAgent(),
		"surface":   "waiting_room",
	}
	body, _ := json.Marshal(payload)
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/ads", body)
	if err != nil {
		return err
	}
	// Header UA: Freebuff-CLI/<version> (getCliAdRequestUserAgent), NOT the
	// chat ai-sdk UA newRequest set — the CLI's ads POST carries exactly
	// this product UA (#124).
	req.Header.Set("User-Agent", freebuffCliUA)
	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return err
	}
	// do() returns a nil cancel when the context already carried a deadline
	// (the chain's own timeout), so guard the defer.
	if cancel != nil {
		defer cancel()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAdResponseRead))
		return fmt.Errorf("ads status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}

// deviceOS maps runtime.GOOS to the ads device block's wire contract
// (macos|windows|linux, use-gravity-ad.ts getDeviceInfo platformToOs):
// Go reports "darwin" but the API only accepts "macos", and anything
// unrecognized falls back to "linux" exactly like the CLI.
func deviceOS() string {
	return deviceOSFor(runtime.GOOS)
}

func deviceOSFor(goos string) string {
	switch goos {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	default:
		return "linux"
	}
}

// egressDeviceTimezone returns the host IANA timezone name for the ads
// device block, mirroring the CLI's
// Intl.DateTimeFormat().resolvedOptions().timeZone (use-gravity-ad.ts
// getDeviceInfo). time.Local.String() is the host zone when Go resolved a
// real IANA name; "Local" is Go's placeholder when it could not, so that
// (and anything LoadLocation rejects) falls back to the always-valid "UTC".
func egressDeviceTimezone() string {
	tz := time.Local.String()
	if tz == "" || tz == "Local" {
		return "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return "UTC"
	}
	return tz
}

// egressDeviceLocale returns the host locale for the ads device block,
// derived from LC_ALL/LC_MESSAGES/LANG (POSIX "en_US.UTF-8" → "en-US",
// charset stripped, "_" → "-"), falling back to "en-US" — the CLI's
// Intl.DateTimeFormat().resolvedOptions().locale shape (use-gravity-ad.ts
// getDeviceInfo). "C"/"POSIX" are not real locales and are skipped.
func egressDeviceLocale() string {
	for _, env := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		raw := os.Getenv(env)
		if raw == "" {
			continue
		}
		lang := strings.SplitN(raw, ".", 2)[0]
		lang = strings.ReplaceAll(lang, "_", "-")
		if lang == "" || lang == "C" || lang == "POSIX" {
			continue
		}
		return lang
	}
	return "en-US"
}

// getStreak GETs /api/v1/freebuff/streak (reference
// cli/src/hooks/use-freebuff-streak-query.ts: the request() helper sets NO
// UA override → bun's default `Bun/<version>`). The proxy's equivalent of
// "no override" is newRequest's bunUserAgent (Bun/1.3.14, the pinned
// .bun-version), which is what this request inherits.
func (c *Client) getStreak(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/freebuff/streak", nil)
	if err != nil {
		return err
	}
	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return err
	}
	if cancel != nil {
		defer cancel()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAdResponseRead))
		return fmt.Errorf("streak status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}
