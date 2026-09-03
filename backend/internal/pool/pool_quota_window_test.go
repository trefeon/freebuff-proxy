// pool_quota_window_test.go — quota-window audit regression tests (item #2):
// lift-aware quarantine (temporary vs hard bans), hard-ban risk labeling,
// mismatch-window cleanup on token removal, the shared quota-window
// implementation (pooled == bridge), and usage/spend index alignment after
// by-index removal followed by AddToken.
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/notify"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// TestTemporaryBanQuarantineLiftsAfterResumesAt pins the lift-aware
// quarantine: a ban with a FUTURE resumes_at quarantines the token only for
// the ban window; once resumes_at passes (the upstream unban is automatic)
// the marker clears by itself — no operator unlock — and the token is
// acquirable again.
func TestTemporaryBanQuarantineLiftsAfterResumesAt(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	p.CooldownTokenBan(0, &upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(50 * time.Millisecond)})

	// During the window: quarantined + temporary ban view.
	snap := p.Snapshot()[0]
	if !snap.Quarantined {
		t.Fatal("temporary ban window: token not quarantined")
	}
	if snap.BanType != "temporary" {
		t.Errorf("BanType = %q, want temporary", snap.BanType)
	}

	// After the lift (no Snapshot in between, so the clear must happen on
	// the acquire path itself): the marker is gone and the token serves.
	time.Sleep(120 * time.Millisecond)
	if lease, err := p.Acquire(context.Background(), modelA); err != nil {
		t.Fatalf("acquire after lifted temporary ban: %v (want success)", err)
	} else {
		p.LeaseRelease(lease)
	}
	snap = p.Snapshot()[0]
	if snap.Quarantined || snap.QuarantineReason != "" {
		t.Errorf("quarantine = %v/%q after lift, want cleared", snap.Quarantined, snap.QuarantineReason)
	}
	if got := p.PoolSnapshot().Quarantined; got != 0 {
		t.Errorf("PoolSnapshot().Quarantined = %d after lift, want 0", got)
	}
}

// TestHardBanQuarantinePermanent pins the inverse: a hard ban (no
// resumes_at) is a permanent terminal state — the marker never self-clears,
// the risk label stays "critical" (a permanent ban has no timed
// BannedUntil), and further Acquires surface the remembered ban without any
// repeated upstream contact.
func TestHardBanQuarantinePermanent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	p.CooldownTokenBan(0, &upstream.BanError{Body: "banned"})

	time.Sleep(80 * time.Millisecond) // a temporary ban window would have lifted by now
	snap := p.Snapshot()[0]
	if !snap.Quarantined {
		t.Fatal("hard ban quarantine self-cleared, want permanent")
	}
	if snap.BanType != "hard" {
		t.Errorf("BanType = %q, want hard", snap.BanType)
	}
	if snap.RiskLevel != "critical" {
		t.Errorf("RiskLevel = %q, want critical (hard ban is permanently live)", snap.RiskLevel)
	}
	if got := p.PoolSnapshot().Quarantined; got != 1 {
		t.Errorf("PoolSnapshot().Quarantined = %d, want 1", got)
	}
	before := mock.RequestCount()
	_, err := p.Acquire(context.Background(), modelA)
	var be *upstream.BanError
	if !errors.As(err, &be) {
		t.Fatalf("acquire on hard-banned token = %v, want *upstream.BanError", err)
	}
	if after := mock.RequestCount(); after != before {
		t.Errorf("upstream requests after hard-ban quarantine = %d, want %d (no re-admission)", after, before)
	}
}

// TestRemoveLastTokenDropsMismatchWindow pins the mismatch-track cleanup: a
// removed last token must drop its escalation window so a later AddToken at
// the same slot never inherits a stale hit history.
func TestRemoveLastTokenDropsMismatchWindow(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	addTokens(t, p, "cb_two") // 2 tokens; last slot key = 2

	rle := &upstream.RateLimitError{Status: "free_mode_invalid_agent_model"}
	p.recordMismatchEscalation(2, rle)
	p.roster.mu.Lock()
	_, present := p.roster.mismatch[2]
	p.roster.mu.Unlock()
	if !present {
		t.Fatal("seed mismatch window for slot 2 missing")
	}

	if err := p.RemoveLastToken(); err != nil {
		t.Fatalf("RemoveLastToken: %v", err)
	}
	p.roster.mu.Lock()
	_, present = p.roster.mismatch[2]
	p.roster.mu.Unlock()
	if present {
		t.Error("mismatch window for removed token still present")
	}
}

// TestRemoveAllTokensDropsMismatchWindows pins the pool-empty cleanup: after
// RemoveAllTokens no pooled escalation window survives as debris.
func TestRemoveAllTokensDropsMismatchWindows(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	addTokens(t, p, "cb_two")

	rle := &upstream.RateLimitError{Status: "free_mode_invalid_agent_model"}
	p.recordMismatchEscalation(1, rle)
	p.recordMismatchEscalation(2, rle)

	p.RemoveAllTokens(context.Background())
	p.roster.mu.Lock()
	left := len(p.roster.mismatch)
	p.roster.mu.Unlock()
	if left != 0 {
		t.Errorf("mismatch windows after RemoveAllTokens = %d, want 0", left)
	}
}

// TestQuotaStateWindowSemantics pins the shared quota-window semantics: a
// live window (recent < limit) is known + remaining; a capped window
// (recent >= limit with a future reset) is capped; a past/absent ResetAt
// means the window rolled and the quota is treated as fresh (never capped);
// referral-gated models without entitlement are excluded.
func TestQuotaStateWindowSemantics(t *testing.T) {
	future := time.Now().Add(6 * time.Hour)
	past := time.Now().Add(-time.Hour)

	cases := []struct {
		name    string
		model   string
		snap    session.SessionSnapshot
		wantKn  bool
		wantRem float64
		wantCap bool
	}{
		{
			name:  "known live window",
			model: modelB,
			snap: session.SessionSnapshot{QuotaByModel: map[string]session.QuotaSnapshot{
				modelB: {Limit: 5, RecentCount: 4, ResetAt: future, Period: "pacific_day"},
			}},
			wantKn: true, wantRem: 1,
		},
		{
			name:  "capped window future reset",
			model: modelB,
			snap: session.SessionSnapshot{QuotaByModel: map[string]session.QuotaSnapshot{
				modelB: {Limit: 5, RecentCount: 5, ResetAt: future, Period: "pacific_day"},
			}},
			wantCap: true,
		},
		{
			name:  "rolled window never capped",
			model: modelB,
			snap: session.SessionSnapshot{QuotaByModel: map[string]session.QuotaSnapshot{
				modelB: {Limit: 5, RecentCount: 5, ResetAt: past, Period: "pacific_day"},
			}},
			wantKn: false, wantCap: false,
		},
		{
			name:   "no quota entry unknown",
			model:  modelB,
			snap:   session.SessionSnapshot{},
			wantKn: false, wantCap: false,
		},
		{
			name:    "referral gated no entitlement capped",
			model:   ReferralGatedModel,
			snap:    session.SessionSnapshot{},
			wantCap: true,
		},
		{
			name:  "referral gated entitled live quota",
			model: ReferralGatedModel,
			snap: session.SessionSnapshot{QuotaByModel: map[string]session.QuotaSnapshot{
				ReferralGatedModel: {Limit: 2, RecentCount: 1, ResetAt: future},
			}},
			wantKn: true, wantRem: 1,
		},
		{
			name:  "referral gated entitled window rolled assumed 1",
			model: ReferralGatedModel,
			snap: session.SessionSnapshot{QuotaByModel: map[string]session.QuotaSnapshot{
				ReferralGatedModel: {Limit: 2, RecentCount: 2, ResetAt: past},
			}},
			wantKn: true, wantRem: 1, wantCap: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kn, rem, cap := quotaStateForSnapshot(tc.snap, tc.model)
			if kn != tc.wantKn || rem != tc.wantRem || cap != tc.wantCap {
				t.Errorf("quotaStateForSnapshot = (%v, %v, %v), want (%v, %v, %v)",
					kn, rem, cap, tc.wantKn, tc.wantRem, tc.wantCap)
			}
		})
	}
}

// TestBridgeQuotaMirrorsPooled pins the single-implementation contract: for
// identical quota state the pooled and bridge quota views agree (both
// delegate to quotaStateForSnapshot), so the window semantics cannot drift
// between the two modes.
func TestBridgeQuotaMirrorsPooled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		modelA: map[string]any{
			"model":       modelA,
			"limit":       3,
			"recentCount": 3,
			"period":      "pacific_day",
			"resetAt":     time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339),
		},
	}

	p := newTestPool(t, mock)
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	pKn, pRem, pCap := quotaRemaining((*p.roster.Load())[0], modelA)

	pb := newBridgePool(t, mock)
	blease, err := pb.AcquireBridge(context.Background(), "parity-client", modelA)
	if err != nil {
		t.Fatal(err)
	}
	pb.LeaseRelease(blease)
	bKn, bRem, bCap := quotaRemaining(blease.Bridge, modelA)

	if pKn != bKn || pRem != bRem || pCap != bCap {
		t.Errorf("pooled vs bridge quota = (%v,%v,%v) vs (%v,%v,%v), want equal",
			pKn, pRem, pCap, bKn, bRem, bCap)
	}
	if !pCap || !bCap {
		t.Errorf("expected both views capped (limit 3, recent 3): pooled=%v bridge=%v", pCap, bCap)
	}
}

// TestMismatchEscalationModelUsesRefusedModel pins the #140 webhook Model
// field (the refused MODEL, falling back to the refusal code) and the
// 1-based TokenIndex convention: pooled token 0 (key 1) never shares the
// escalation window with the bridge entries (key 0).
func TestMismatchEscalationModelUsesRefusedModel(t *testing.T) {
	var posts atomic.Int64
	var gotModel atomic.Value
	var gotIdx atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err == nil {
			gotModel.Store(ev.Model)
			gotIdx.Store(int64(ev.TokenIndex))
		}
		posts.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	addTokens(t, p, "cb_two") // key 1 = token 0, key 2 = token 1
	p.SetNotifier(notify.New(srv.URL, nil))

	// Model carried in the body: the event must carry it, indexed 1-based.
	rle := &upstream.RateLimitError{Status: "free_mode_invalid_agent_model", Model: modelA,
		RetryAfter: upstream.InvalidModelCooldown}
	p.recordMismatchEscalation(1, rle)
	p.recordMismatchEscalation(1, rle)
	p.recordMismatchEscalation(1, rle)
	testutil.WaitFor(t, 3*time.Second, func() bool { return gotIdx.Load() == 1 },
		fmt.Sprintf("mismatch-escalation webhook posts = %d, want 1", posts.Load()))
	if got := gotModel.Load().(string); got != modelA {
		t.Errorf("event Model = %q, want %q", got, modelA)
	}
	if got := gotIdx.Load(); got != 1 {
		t.Errorf("event TokenIndex = %d, want 1 (1-based pooled index)", got)
	}

	// A BRIDGE-keyed storm (key 0) must not merge with the pooled token-0
	// window: it fires its own alert with TokenIndex 0.
	gotIdx.Store(0)
	p.recordMismatchEscalation(0, rle)
	p.recordMismatchEscalation(0, rle)
	// The notify throttle is per event TYPE (notify.go throttle): the
	// bridge-keyed storm shares the pooled storm's event type, so it must
	// NOT produce a second POST inside the window. Age past any
	// fire-and-forget delivery and assert the silence.
	time.Sleep(time.Second)
	if posts.Load() != 1 {
		t.Errorf("bridge-keyed storm posted again: posts = %d, want 1 (throttled per event type)", posts.Load())
	}
	if got := gotIdx.Load(); got != 0 {
		t.Errorf("bridge-keyed event TokenIndex = %d, want 0 (bridge shared window)", got)
	}

	// Model absent from the body: fall back to the refusal code. The
	// throttle is per event TYPE (5m), so the second storm on a DIFFERENT
	// key will not POST — a fresh sender proves the fallback path.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err == nil {
			gotModel.Store(ev.Model)
		}
		w.WriteHeader(200)
	}))
	defer srv2.Close()
	p.SetNotifier(notify.New(srv2.URL, nil))
	p.recordMismatchEscalation(2, &upstream.RateLimitError{Status: "free_mode_invalid_agent_model",
		RetryAfter: upstream.InvalidModelCooldown})
	p.recordMismatchEscalation(2, &upstream.RateLimitError{Status: "free_mode_invalid_agent_model",
		RetryAfter: upstream.InvalidModelCooldown})
	p.recordMismatchEscalation(2, &upstream.RateLimitError{Status: "free_mode_invalid_agent_model",
		RetryAfter: upstream.InvalidModelCooldown})
	testutil.WaitFor(t, 3*time.Second, func() bool {
		return gotModel.Load().(string) == "free_mode_invalid_agent_model"
	}, "code-fallback webhook never posted a model")
	if got := gotModel.Load().(string); got != "free_mode_invalid_agent_model" {
		t.Errorf("event Model without body model = %q, want code fallback", got)
	}
}

// TestRemoveTokenAtThenAddKeepsUsageSpendAligned pins the by-index removal
// alignment: after RemoveTokenAt(1) + AddToken the usage/spend slices are
// re-synced 1:1 with the token list, and the surviving entries keep their
// original per-token history (the removed slot's data does not bleed into
// the shifted entries).
func TestRemoveTokenAtThenAddKeepsUsageSpendAligned(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := sizedPool(t, mock)
	addTokens(t, p, "cb_one", "cb_two", "cb_three") // indices 0..3

	// Distinct usage/spend history per index (travels with each entry).
	p.roster.mu.Lock()
	(*p.roster.Load())[0].ledger.usage = []time.Time{time.Now()}                    // token 0: 1 msg
	(*p.roster.Load())[3].ledger.usage = []time.Time{time.Now(), time.Now().Add(1)} // token 3: 2 msgs
	(*p.roster.Load())[0].ledger.spend.add(100, time.Now())
	(*p.roster.Load())[3].ledger.spend.add(200, time.Now())
	p.roster.mu.Unlock()

	if err := p.RemoveTokenAt(1); err != nil {
		t.Fatalf("RemoveTokenAt(1): %v", err)
	}
	if _, err := p.AddToken("cb_four"); err != nil {
		t.Fatalf("AddToken: %v", err)
	}

	p.roster.mu.Lock()
	us := len(*p.roster.Load())
	u0 := len((*p.roster.Load())[0].ledger.usage)
	u2 := len((*p.roster.Load())[2].ledger.usage)
	sp := len(*p.roster.Load())
	d0 := (*p.roster.Load())[0].ledger.spend.rolling24h(time.Now())
	d2 := (*p.roster.Load())[2].ledger.spend.rolling24h(time.Now())
	p.roster.mu.Unlock()

	if got := len(*p.roster.Load()); got != 4 {
		t.Fatalf("token count = %d, want 4", got)
	}
	if us != 4 || sp != 4 {
		t.Fatalf("usage/spend arrays = %d/%d, want 4/4 (index-aligned with tokens)", us, sp)
	}
	if u0 != 1 {
		t.Errorf("usage[0] = %d after removal, want 1 (original token-0 history)", u0)
	}
	// Old index 3's two messages moved to index 2 after the middle removal.
	if u2 != 2 {
		t.Errorf("usage[2] = %d after removal, want 2 (old token-3 history shifted)", u2)
	}
	if d0 != 100 {
		t.Errorf("spend[0] day = %d, want 100 (original token-0 spend)", d0)
	}
	if d2 != 200 {
		t.Errorf("spend[2] day = %d, want 200 (old token-3 spend shifted)", d2)
	}
}

// TestFreebucksCappedRetryWalletShape pins the issue #321 Freebucks wire
// shape: the capped retry derives from the daily pool refill and the plan
// wallet bonus — the pre-drift weekly/monthly binding windows are gone.
// Balance (server-computed spendable = daily.remaining + wallet.balance)
// below price caps the token; earliest future refill wins; no signal → 0.
func TestFreebucksCappedRetryWalletShape(t *testing.T) {
	mkSnap := func(fb *upstream.FreebucksInfo) session.SessionSnapshot {
		return session.SessionSnapshot{Freebucks: fb}
	}
	fb := func(balance float64, dailyReset, bonusAt time.Time) *upstream.FreebucksInfo {
		return &upstream.FreebucksInfo{
			Balance: balance,
			Daily:   upstream.FreebucksWindow{Limit: 20, Spent: 19, Remaining: 1, ResetAt: dailyReset},
			Wallet:  upstream.FreebucksWallet{Balance: 0, MonthlyBonus: 10, NextBonusAt: bonusAt},
			Prices:  map[string]float64{"openai/gpt-5.6-luna": 2},
		}
	}
	// Balance covers the price → not capped.
	capped, _ := freebucksCappedForSnapshot(mkSnap(fb(5, time.Now().Add(time.Hour), time.Time{})), "openai/gpt-5.6-luna")
	if capped {
		t.Error("capped with balance 5 >= price 2, want not capped")
	}
	// Capped: daily refill in 1h, bonus in 24h → retry ≈ daily reset.
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	capped, retry := freebucksCappedForSnapshot(mkSnap(fb(0.5, reset, reset.Add(23*time.Hour))), "openai/gpt-5.6-luna")
	if !capped {
		t.Fatal("not capped with balance 0.5 < price 2")
	}
	if retry < 59*time.Minute || retry > time.Hour+time.Minute {
		t.Errorf("retry = %v, want ≈1h (daily refill, not the 24h bonus)", retry)
	}
	// Capped with the daily reset already past: plan bonus is the signal.
	capped, retry = freebucksCappedForSnapshot(mkSnap(fb(0.5, time.Now().Add(-time.Hour), reset.Add(24*time.Hour))), "openai/gpt-5.6-luna")
	if !capped {
		t.Fatal("not capped with past daily reset")
	}
	if retry <= 0 {
		t.Errorf("retry = %v, want the plan bonus instant", retry)
	}
	// No model price → never capped.
	capped, _ = freebucksCappedForSnapshot(mkSnap(fb(0, reset, time.Time{})), "unknown/model")
	if capped {
		t.Error("capped for unpriced model, want not capped")
	}
}
