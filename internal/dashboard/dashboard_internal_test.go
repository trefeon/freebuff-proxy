package dashboard

// Internal (package dashboard) unit tests for the pure rendering helpers and
// the metrics sample window — the biggest coverage lever for the dashboard
// package (43.8% → the functions below were almost entirely untested).

import (
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/upstream"
)

// testDashboard builds a dashboard over an empty (bridge-mode) pool: enough
// for the metrics/hist path, which only needs PoolSnapshot + registry.
func testDashboard(t *testing.T) *Dashboard {
	t.Helper()
	cfg := &config.Config{UpstreamBaseURL: "https://www.codebuff.com"}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	return New(func() *config.Config { return cfg }, p, reg, nil, nil)
}

func TestSparklineSVG(t *testing.T) {
	// Empty series: a flat baseline, never a crash.
	got := string(sparklineSVG(nil, "var(--fp-amber)", "label"))
	if !strings.Contains(got, `<polyline points="0,42 260,42"`) {
		t.Errorf("empty series = %q, want flat baseline", got)
	}
	if !strings.Contains(got, `aria-label="label"`) {
		t.Errorf("empty series missing aria-label: %s", got)
	}

	// Single sample: same flat branch.
	got = string(sparklineSVG([]float64{7}, "c", "l"))
	if !strings.Contains(got, `<polyline points="0,42 260,42"`) {
		t.Errorf("single-sample = %q, want flat baseline", got)
	}

	// Constant series (>=2): flat polyline with one point per sample.
	got = string(sparklineSVG([]float64{5, 5, 5}, "c", "l"))
	if !strings.Contains(got, `points="0.0,42.0 130.0,42.0 260.0,42.0"`) {
		t.Errorf("constant series = %q, want flat three-point polyline", got)
	}

	// Varying series: min at bottom (42) and max at top (2).
	got = string(sparklineSVG([]float64{0, 10}, "c", "l"))
	if !strings.Contains(got, `points="0.0,42.0 260.0,2.0"`) {
		t.Errorf("varying 2-series = %q, want 0→bottom / 10→top", got)
	}
	got = string(sparklineSVG([]float64{0, 5, 10}, "c", "l"))
	if !strings.Contains(got, `points="0.0,42.0 130.0,22.0 260.0,2.0"`) {
		t.Errorf("varying 3-series = %q, want linearly scaled points", got)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{4*time.Hour + 12*time.Minute, "4h 12m"},
		{45 * time.Minute, "45m"},
		{30 * time.Second, "1m"}, // sub-minute rounds up so countdowns never show a false 0s
		{0, "1m"},
		{90 * time.Second, "2m"},
		{3 * time.Hour, "3h"},
		{5*time.Hour + 59*time.Minute, "5h 59m"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.in); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatQuota(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{5.0, "5"},
		{5.5, "5.5"},
		{0, "0"},
		{123.456, "123.456"},
		{100, "100"},
	}
	for _, tc := range cases {
		if got := formatQuota(tc.in); got != tc.want {
			t.Errorf("formatQuota(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatEntitlement(t *testing.T) {
	// Ordered alphabetically regardless of map iteration order.
	got := formatEntitlement(map[string]float64{"streak": 3, "base": 1, "referral": 1})
	if got != "base=1, referral=1, streak=3" {
		t.Errorf("formatEntitlement = %q, want sorted key list", got)
	}
	if got := formatEntitlement(map[string]float64{"a": 1.5}); got != "a=1.5" {
		t.Errorf("formatEntitlement single = %q, want a=1.5", got)
	}
	if got := formatEntitlement(nil); got != "" {
		t.Errorf("formatEntitlement(nil) = %q, want empty", got)
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("abcdefghij"); got != "abcdefgh…" {
		t.Errorf("shortID(long) = %q, want 8 chars + ellipsis", got)
	}
	if got := shortID("abcdefgh"); got != "abcdefgh" {
		t.Errorf("shortID(8) = %q, want unchanged", got)
	}
	if got := shortID(""); got != "" {
		t.Errorf("shortID(empty) = %q, want empty", got)
	}
}

// TestMetricsDataSampleCap pins the rolling window: sampling beyond the 120
// cap drops the oldest samples, and repeated sampling appends (the sparkline
// grows to the window width).
func TestMetricsDataSampleCap(t *testing.T) {
	d := testDashboard(t)
	for range maxMetricSamples + 5 {
		md := d.metricsData()
		if md.SampleCount > maxMetricSamples {
			t.Fatalf("SampleCount = %d, exceeded cap %d", md.SampleCount, maxMetricSamples)
		}
	}
	md := d.metricsData()
	if md.SampleCount != maxMetricSamples {
		t.Errorf("SampleCount = %d after %d samples, want capped at %d", md.SampleCount, maxMetricSamples+6, maxMetricSamples)
	}
	// The requests sparkline covers the full window width (last point x=260).
	if !strings.Contains(string(md.RequestsSpark), "260.0,") {
		t.Errorf("requests sparkline missing window-width point: %.60s", md.RequestsSpark)
	}
	if !strings.Contains(string(md.RetriesSpark), "<svg") {
		t.Error("retries sparkline missing")
	}
	if md.TransientRetries != 0 || md.FingerprintRotations != 0 || md.Models == 0 {
		t.Errorf("metrics aggregate fields wrong: retries=%d rotations=%d models=%d",
			md.TransientRetries, md.FingerprintRotations, md.Models)
	}
}

// TestMetricsDataRepeatedSampling pins the append behavior: two consecutive
// samples with a growing counter produce a two-point sparkline (not a reset).
func TestMetricsDataRepeatedSampling(t *testing.T) {
	d := testDashboard(t)
	d.metricsData()
	md := d.metricsData()
	if md.SampleCount != 2 {
		t.Fatalf("SampleCount = %d after two samples, want 2", md.SampleCount)
	}
	if !strings.Contains(string(md.RequestsSpark), "260.0,") {
		t.Errorf("two-point sparkline missing final point: %.60s", md.RequestsSpark)
	}
}

// TestCardFromSnapshotStanding pins the #96 standing mapping: the upstream
// standing block (level/label/score/nextLevelAt/nextLevel) lands on the
// token card fields, and a nil standing block leaves HasStanding false.
func TestCardFromSnapshotStanding(t *testing.T) {
	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	card := cardFromSnapshot(pool.TokenSnapshot{
		Token:     0,
		RiskLevel: "low",
		Standing: &upstream.SessionStanding{
			Level:       "established",
			Label:       "Established",
			Score:       62,
			NextLevelAt: at,
			NextLevel:   "core",
		},
	})
	if !card.HasStanding {
		t.Fatal("HasStanding = false, want true")
	}
	if card.StandingLevel != "established" || card.StandingLabel != "Established" {
		t.Errorf("level/label = %q/%q, want established/Established", card.StandingLevel, card.StandingLabel)
	}
	if card.StandingScore != 62 {
		t.Errorf("score = %v, want 62", card.StandingScore)
	}
	if card.StandingNextLevel != "core" {
		t.Errorf("nextLevel = %q, want core", card.StandingNextLevel)
	}
	if card.StandingNextLevelAt != at.Format(time.RFC3339) {
		t.Errorf("nextLevelAt = %q, want %q", card.StandingNextLevelAt, at.Format(time.RFC3339))
	}

	card = cardFromSnapshot(pool.TokenSnapshot{Token: 1, RiskLevel: "low"})
	if card.HasStanding {
		t.Error("HasStanding = true without a standing block, want false")
	}
	if card.StandingLevel != "" || card.StandingScore != 0 {
		t.Errorf("standing fields populated without a block: %q/%v", card.StandingLevel, card.StandingScore)
	}

	// Issue #140 P3d: cap + earn-back fields land on the card too.
	card = cardFromSnapshot(pool.TokenSnapshot{
		Token:     2,
		RiskLevel: "low",
		Standing: &upstream.SessionStanding{
			Level:        "verified",
			Label:        "Verified",
			Score:        30,
			CappedBy:     "third_party_client",
			CappedReason: "A foreign tool schema was seen on this account.",
			Blurb:        "Your account is capped at verified trust.",
			NextSteps: []upstream.StandingNextStep{
				{ID: "verify_email", Label: "Verify your email", Detail: "Adds 25 points.", Points: 25, Href: "/settings"},
			},
		},
	})
	if card.StandingCappedBy != "third_party_client" || card.StandingCappedReason == "" {
		t.Errorf("cappedBy/reason = %q/%q, want third_party_client/non-empty", card.StandingCappedBy, card.StandingCappedReason)
	}
	if card.StandingBlurb == "" {
		t.Error("blurb not carried to the card")
	}
	if len(card.StandingNextSteps) != 1 || card.StandingNextSteps[0].ID != "verify_email" || card.StandingNextSteps[0].Points != 25 {
		t.Errorf("nextSteps = %+v, want one verify_email step worth 25", card.StandingNextSteps)
	}
}

// TestCardFromSnapshotBanAndLocked pins the #198/#199 ban mapping: an active
// temporary ban lands ban_type + RFC3339 banned_until on the card, a hard
// ban carries only the type, and Locked is copied through (previously
// dropped, leaving pool-token lock state undefined in the UI). A snapshot
// without ban/lock state yields zero-valued fields.
func TestCardFromSnapshotBanAndLocked(t *testing.T) {
	until := time.Date(2026, 8, 22, 18, 0, 0, 0, time.UTC)
	card := cardFromSnapshot(pool.TokenSnapshot{
		Token:       0,
		RiskLevel:   "critical",
		Locked:      true,
		BanType:     "temporary",
		BannedUntil: until,
	})
	if !card.Locked {
		t.Error("Locked = false, want true")
	}
	if card.BanType != "temporary" {
		t.Errorf("BanType = %q, want temporary", card.BanType)
	}
	if card.BannedUntil != until.Format(time.RFC3339) {
		t.Errorf("BannedUntil = %q, want %q", card.BannedUntil, until.Format(time.RFC3339))
	}

	card = cardFromSnapshot(pool.TokenSnapshot{
		Token:     1,
		RiskLevel: "low",
		BanType:   "hard",
	})
	if card.BanType != "hard" || card.BannedUntil != "" {
		t.Errorf("hard ban card = %q/%q, want hard/empty", card.BanType, card.BannedUntil)
	}

	card = cardFromSnapshot(pool.TokenSnapshot{Token: 2, RiskLevel: "low"})
	if card.Locked || card.BanType != "" || card.BannedUntil != "" {
		t.Errorf("clean card = %v/%q/%q, want false/empty/empty", card.Locked, card.BanType, card.BannedUntil)
	}

	bc := bridgeCardFromSnapshot(pool.BridgeTokenSnapshot{
		Key:         "abcd1234efgh",
		BanType:     "temporary",
		BannedUntil: until,
	})
	if bc.BanType != "temporary" || bc.BannedUntil != until.Format(time.RFC3339) {
		t.Errorf("bridge card ban = %q/%q, want temporary/%s", bc.BanType, bc.BannedUntil, until.Format(time.RFC3339))
	}
}

// TestModelsDataServedGateOnly pins the served-model filter on modelsData:
// the vendor registry also carries god-only/eval rows (luna-es since
// snapshot 0603bc1) that must never appear in the dashboard models view,
// and Count must reflect the filtered set, not the raw registry size.
func TestModelsDataServedGateOnly(t *testing.T) {
	cfg := &config.Config{
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	if reg.ModelCount() <= len(registry.SupportedModelIDs) {
		t.Fatalf("precondition: fallback registry should exceed the served set (got %d)", reg.ModelCount())
	}
	d := New(func() *config.Config { return cfg }, nil, reg, nil, nil)
	md := d.modelsData()
	if md.Count != len(registry.SupportedModelIDs) {
		t.Errorf("Count = %d, want %d (served set)", md.Count, len(registry.SupportedModelIDs))
	}
	for _, row := range md.Models {
		if !registry.IsServedModel(row.ID) {
			t.Errorf("models view contains unserved model %q", row.ID)
		}
	}
}

// TestConfigDataEffectiveRows pins the Effective table contract: every key
// appears exactly once, SAFE_MODE is present (it was silently clobbered by a
// bad edit once), and secret list-valued keys render counts — never raw
// joins (DASH-EXPOSURE-005).
func TestConfigDataEffectiveRows(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := &config.Config{
		AuthTokens:      []string{"tok-a", "tok-b"},
		APIKeys:         []string{"sk-client-1", "sk-client-2"},
		SafeMode:        true,
		UpstreamBaseURL: "https://www.codebuff.com",
	}
	d := New(func() *config.Config { return cfg }, nil, nil, nil, nil)
	cd := d.configData()

	seen := map[string]int{}
	for _, kv := range cd.Effective {
		seen[kv.Key]++
		if kv.Key == "API_KEYS" && (strings.Contains(kv.Value, "sk-client") || strings.Contains(kv.Value, ",")) {
			t.Errorf("API_KEYS row leaks raw values: %q", kv.Value)
		}
		if kv.Key == "AUTH_TOKENS" && strings.Contains(kv.Value, "tok-") {
			t.Errorf("AUTH_TOKENS row leaks raw values: %q", kv.Value)
		}
	}
	for _, k := range []string{"SAFE_MODE", "API_KEYS", "AUTH_TOKENS", "ADMIN_TOKEN"} {
		if seen[k] != 1 {
			t.Errorf("Effective rows for %s = %d, want exactly 1", k, seen[k])
		}
	}
}
