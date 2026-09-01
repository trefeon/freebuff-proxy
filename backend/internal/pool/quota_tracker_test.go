package pool

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
)

func TestPremiumSnapshotFromQuotaMap(t *testing.T) {
	now := time.Now()
	future := now.Add(2 * time.Hour)
	past := now.Add(-2 * time.Hour)

	tests := []struct {
		name       string
		m          map[string]session.QuotaSnapshot
		wantP      bool
		wantCapped bool
	}{
		{
			name: "empty map nil",
			m:    map[string]session.QuotaSnapshot{},
		},
		{
			name: "nil map nil",
			m:    nil,
		},
		{
			name: "single premium flash",
			m: map[string]session.QuotaSnapshot{
				"openai/gpt-5.6-luna": {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 2, Period: "pacific_day", ResetAt: future},
			},
			wantP: true,
		},
		{
			name: "premium pro fallback when flash absent",
			m: map[string]session.QuotaSnapshot{
				"openai/gpt-5.6-luna": {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 1, Period: "pacific_day", ResetAt: future},
				"mimo/mimo-v2.5":      {Model: "mimo/mimo-v2.5", Limit: 100, RecentCount: 10, Period: "pacific_day", ResetAt: future},
			},
			wantP: true,
		},
		{
			name: "only mimo no premium",
			m: map[string]session.QuotaSnapshot{
				"mimo/mimo-v2.5": {Model: "mimo/mimo-v2.5", Limit: 100, RecentCount: 10, Period: "pacific_day", ResetAt: future},
			},
		},
		{
			name: "premium via shared pool member",
			m: map[string]session.QuotaSnapshot{
				"openai/gpt-5.6-luna": {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 1, Period: "pacific_day", ResetAt: future},
			},
			wantP: true,
		},
		{
			name: "both premium members",
			m: map[string]session.QuotaSnapshot{
				"openai/gpt-5.6-luna": {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 3, Period: "pacific_day", ResetAt: future},
				"upstage/solar-pro4":  {Model: "upstage/solar-pro4", Limit: 4, RecentCount: 2, Period: "pacific_day", ResetAt: future},
			},
			wantP: true,
		},
		{
			name: "capped detection future reset",
			m: map[string]session.QuotaSnapshot{
				"openai/gpt-5.6-luna": {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 4, Period: "pacific_day", ResetAt: future},
			},
			wantP:      true,
			wantCapped: true,
		},
		{
			name: "not capped when reset past",
			m: map[string]session.QuotaSnapshot{
				"openai/gpt-5.6-luna": {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 4, Period: "pacific_day", ResetAt: past},
			},
			wantP: true,
		},
		{
			name: "prefer luna over sorted",
			m: map[string]session.QuotaSnapshot{
				"openai/gpt-5.6-luna":        {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 2, Period: "pacific_day", ResetAt: future},
				"deepseek/deepseek-v4-flash": {Model: "deepseek/deepseek-v4-flash", Limit: 100, RecentCount: 4, Period: "pacific_day", ResetAt: future},
			},
			wantP: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			premium := premiumSnapshotFromQuotaMap(tc.m)
			if tc.wantP && premium == nil {
				t.Fatalf("expected premium snapshot, got nil")
			}
			if !tc.wantP && premium != nil {
				t.Fatalf("expected premium nil, got %+v", premium)
			}
			if premium != nil {
				if premium.PercentUsed < 0 || premium.PercentUsed > 100 {
					t.Errorf("percent_used out of range: %d", premium.PercentUsed)
				}
				if tc.wantCapped && !premium.Capped {
					t.Errorf("expected capped true, got false: %+v", premium)
				}
				if !tc.wantCapped && tc.name == "not capped when reset past" && premium.Capped {
					t.Errorf("expected capped false when reset past")
				}
				if premium.Period == "" {
					t.Errorf("period empty")
				}
				if premium.Remaining != premium.Limit-premium.Used && premium.Remaining != 0 {
					// remaining is max(0, limit-used)
					if premium.Remaining < 0 {
						t.Errorf("remaining negative: %d", premium.Remaining)
					}
				}
			}
		})
	}
}

func TestPremiumSnapshotPreferLuna(t *testing.T) {
	future := time.Now().Add(time.Hour)
	m := map[string]session.QuotaSnapshot{
		"openai/gpt-5.6-luna":      {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 2, Period: "pacific_day", ResetAt: future},
		"deepseek/deepseek-v4-pro": {Model: "deepseek/deepseek-v4-pro", Limit: 5, RecentCount: 4, Period: "pacific_day", ResetAt: future},
	}
	premium := premiumSnapshotFromQuotaMap(m)
	if premium == nil {
		t.Fatal("premium nil")
	}
	if premium.Used != 2 {
		t.Errorf("prefer flash: used=%d want 2", premium.Used)
	}
}

func TestPremiumSnapshotSortedFallback(t *testing.T) {
	future := time.Now().Add(time.Hour)
	m := map[string]session.QuotaSnapshot{
		"openai/gpt-5.6-luna": {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 1, Period: "pacific_day", ResetAt: future},
	}
	premium := premiumSnapshotFromQuotaMap(m)
	if premium == nil {
		t.Fatal("premium nil")
	}
	if premium.Used != 1 {
		t.Errorf("sorted fallback: used=%d want 1", premium.Used)
	}
	if premium.Limit != 4 {
		t.Errorf("limit = %d, want 4", premium.Limit)
	}
}

func TestBuildPremiumSnapshotFields(t *testing.T) {
	future := time.Now().Add(time.Hour)
	q := session.QuotaSnapshot{
		Model:       "deepseek/deepseek-v4-flash",
		Limit:       5,
		RecentCount: 3,
		Period:      "pacific_day",
		ResetAt:     future,
		Entitlement: map[string]float64{"base": 5},
	}
	snap := buildPremiumSnapshot(q)
	if snap.Limit != 5 || snap.Used != 3 || snap.Remaining != 2 || snap.PercentUsed != 60 {
		t.Errorf("fields mismatch: %+v", snap)
	}
	if !snap.Entitled {
		t.Error("expected entitled")
	}
	if snap.Capped {
		t.Error("unexpected capped")
	}
	// capped case
	q2 := session.QuotaSnapshot{Limit: 4, RecentCount: 4, Period: "pacific_day", ResetAt: future}
	snap2 := buildPremiumSnapshot(q2)
	if !snap2.Capped || snap2.Remaining != 0 || snap2.PercentUsed != 100 {
		t.Errorf("capped fields mismatch: %+v", snap2)
	}
}

func TestIsPremiumModel(t *testing.T) {
	if !isPremiumModel("openai/gpt-5.6-luna") {
		t.Error("expected premium for luna")
	}
	if isPremiumModel("z-ai/glm-5.3-flash") {
		t.Error("glm-5.3-flash left the premium pool on 2026-08-28 (unmetered)")
	}
	if isPremiumModel("mimo/mimo-v2.5") {
		t.Error("unexpected premium for mimo")
	}
	if isPremiumModel("z-ai/glm-5.2") {
		t.Error("glm-5.2 not premium pool")
	}
}

func TestSnapshotPremiumQuotaIntegration(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		"openai/gpt-5.6-luna": map[string]any{
			"model":       "openai/gpt-5.6-luna",
			"limit":       4,
			"recentCount": 2,
			"period":      "pacific_day",
			"resetAt":     "2026-08-27T07:00:00.000Z",
		},
		"z-ai/glm-5.3-flash": map[string]any{
			"model":       "z-ai/glm-5.3-flash",
			"limit":       4,
			"recentCount": 1,
			"period":      "pacific_day",
			"resetAt":     "2026-08-27T07:00:00.000Z",
		},
		"mimo/mimo-v2.5": map[string]any{
			"model":       "mimo/mimo-v2.5",
			"limit":       100,
			"recentCount": 10,
			"period":      "pacific_day",
			"resetAt":     "2026-08-27T07:00:00.000Z",
		},
	}
	p := newTestPool(t, mock)
	lease, err := p.Acquire(context.Background(), "openai/gpt-5.6-luna")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	_ = lease
	snaps := p.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("snapshots %d", len(snaps))
	}
	snap := snaps[0]
	if snap.PremiumQuota == nil {
		t.Fatal("PremiumQuota nil, want 5/day")
	}
	if snap.PremiumQuota.Limit != 4 || snap.PremiumQuota.Used != 2 || snap.PremiumQuota.Remaining != 2 || snap.PremiumQuota.PercentUsed != 50 {
		t.Errorf("premium mismatch: %+v", snap.PremiumQuota)
	}
	// helper alias
	if got := p.PremiumQuotaForToken(0); got == nil || got.Limit != 4 {
		t.Errorf("PremiumQuotaForToken mismatch: %+v", got)
	}
	if got := p.PremiumSnapshotForToken(0); got == nil || got.Limit != 4 {
		t.Errorf("PremiumSnapshotForToken mismatch: %+v", got)
	}
	// JSON omitempty check
	b, _ := json.Marshal(snap)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("json %v", err)
	}
	if _, ok := m["premium_quota"]; !ok {
		t.Error("json missing premium_quota")
	}
	if _, ok := m["glm53flash_quota"]; ok {
		t.Error("json should not contain glm53flash_quota")
	}
	pq := m["premium_quota"].(map[string]any)
	if int(pq["limit"].(float64)) != 4 {
		t.Errorf("json limit %v", pq["limit"])
	}
}

func TestSnapshotPremiumQuotaNilWhenNoPremium(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		"mimo/mimo-v2.5": map[string]any{
			"model":       "mimo/mimo-v2.5",
			"limit":       100,
			"recentCount": 10,
			"period":      "pacific_day",
			"resetAt":     "2026-08-27T07:00:00.000Z",
		},
	}
	p := newTestPool(t, mock)
	lease, err := p.Acquire(context.Background(), "mimo/mimo-v2.5")
	if err != nil {
		t.Fatalf("Acquire %v", err)
	}
	_ = lease
	snap := p.Snapshot()[0]
	if snap.PremiumQuota != nil {
		t.Errorf("expected nil premium, got %+v", snap.PremiumQuota)
	}
	b, _ := json.Marshal(snap)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["premium_quota"]; ok {
		t.Error("premium_quota should be omitted when nil")
	}
	if got := p.PremiumQuotaForToken(999); got != nil {
		t.Error("out of range should be nil")
	}
}

func TestBridgeSnapshotPremium(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		"openai/gpt-5.6-luna": map[string]any{
			"model":       "openai/gpt-5.6-luna",
			"limit":       4,
			"recentCount": 4,
			"period":      "pacific_day",
			"resetAt":     "2026-08-27T07:00:00.000Z",
		},
	}
	p := newBridgePool(t, mock)
	lease, err := p.AcquireBridge(context.Background(), "cb_test_bridge_token", "openai/gpt-5.6-luna")
	if err != nil {
		t.Fatalf("AcquireBridge %v", err)
	}
	_ = lease
	snaps := p.BridgeSnapshot()
	if len(snaps) != 1 {
		t.Fatalf("bridge snaps %d", len(snaps))
	}
	if snaps[0].PremiumQuota == nil || snaps[0].PremiumQuota.Limit != 4 {
		t.Errorf("bridge premium %+v", snaps[0].PremiumQuota)
	}
	if got := p.PremiumQuotaForBridge(snaps[0].Key); got == nil || got.Used != 4 {
		t.Errorf("PremiumQuotaForBridge %+v", got)
	}
	// raw token also works (hashed lookup fallback)
	if got := p.PremiumQuotaForBridge("cb_test_bridge_token"); got == nil {
		t.Error("raw token lookup failed")
	}
}

// TestPremiumSnapshotForBridge pins the bridge-lease premium quota view
// (alias of PremiumQuotaForBridge) for both the hashed key and the raw
// token, and its nil-for-unknown-key behavior.
func TestPremiumSnapshotForBridge(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		"openai/gpt-5.6-luna": map[string]any{
			"model":       "openai/gpt-5.6-luna",
			"limit":       4,
			"recentCount": 2,
			"period":      "pacific_day",
			"resetAt":     "2026-08-27T07:00:00.000Z",
		},
	}
	p := newBridgePool(t, mock)
	lease, err := p.AcquireBridge(context.Background(), "premium_bridge_token", "openai/gpt-5.6-luna")
	if err != nil {
		t.Fatalf("AcquireBridge %v", err)
	}
	_ = lease

	snaps := p.BridgeSnapshot()
	if len(snaps) != 1 {
		t.Fatalf("bridge snaps %d", len(snaps))
	}
	if ps := p.PremiumSnapshotForBridge(snaps[0].Key); ps == nil || ps.Limit != 4 || ps.Used != 2 {
		t.Errorf("PremiumSnapshotForBridge(key) = %+v, want limit 4 used 2", ps)
	}
	if ps := p.PremiumSnapshotForBridge("premium_bridge_token"); ps == nil || ps.Used != 2 {
		t.Errorf("PremiumSnapshotForBridge(raw) = %+v, want used 2", ps)
	}
	if ps := p.PremiumSnapshotForBridge("no-such-key"); ps != nil {
		t.Errorf("PremiumSnapshotForBridge(unknown) = %+v, want nil", ps)
	}
}
