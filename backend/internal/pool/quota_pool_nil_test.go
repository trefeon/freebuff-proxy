package pool

import (
	"testing"
	"time"

	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
)

func TestPoolPremiumQuotaNilWhenNoSession(t *testing.T) {
	emptyPool := &Pool{}
	if got := emptyPool.PremiumSnapshotForToken(0); got != nil {
		t.Errorf("PremiumSnapshotForToken on empty pool = %+v want nil", got)
	}
	if got := emptyPool.PremiumQuotaForToken(-1); got != nil {
		t.Errorf("PremiumQuotaForToken(-1) = %+v want nil", got)
	}
	if got := emptyPool.PremiumQuotaForBridge("any"); got != nil {
		t.Errorf("PremiumQuotaForBridge on empty pool = %+v want nil", got)
	}
	// Pool with token but no quota
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	if got := p.PremiumSnapshotForToken(0); got != nil {
		t.Errorf("PremiumSnapshotForToken with no quota = %+v want nil", got)
	}
	if got := p.PremiumSnapshotForToken(99); got != nil {
		t.Errorf("PremiumSnapshotForToken out of range = %+v want nil", got)
	}
	if got := p.PremiumQuotaForBridge("nonexistent"); got != nil {
		t.Errorf("PremiumQuotaForBridge nonexistent = %+v want nil", got)
	}
}

func TestPremiumSnapshotMathSpec(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	m := map[string]session.QuotaSnapshot{
		"openai/gpt-5.6-luna": {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 2, Period: "pacific_day", ResetAt: future},
	}
	premium := premiumSnapshotFromQuotaMap(m)
	if premium == nil {
		t.Fatal("premium nil")
	}
	if premium.Limit != 4 || premium.Used != 2 || premium.Remaining != 2 || premium.PercentUsed != 50 {
		t.Errorf("math 4/2 = %+v want limit4 used2 remaining2 percent50", premium)
	}
	if premium.Period != "pacific_day" {
		t.Errorf("period = %q want pacific_day", premium.Period)
	}
	if premium.Capped {
		t.Error("capped true want false")
	}
	// capped case
	past := time.Now().Add(-24 * time.Hour)
	m2 := map[string]session.QuotaSnapshot{
		"openai/gpt-5.6-luna": {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 4, Period: "pacific_day", ResetAt: future},
	}
	p2 := premiumSnapshotFromQuotaMap(m2)
	if !p2.Capped {
		t.Error("capped false want true for future reset")
	}
	m3 := map[string]session.QuotaSnapshot{
		"openai/gpt-5.6-luna": {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 4, Period: "pacific_day", ResetAt: past},
	}
	p3 := premiumSnapshotFromQuotaMap(m3)
	if p3.Capped {
		t.Error("capped true want false for past reset")
	}
	// glm-5.3-flash shares the premium pool, so luna wins when present
	m4 := map[string]session.QuotaSnapshot{
		"openai/gpt-5.6-luna": {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 1, Period: "pacific_day", ResetAt: future},
		"z-ai/glm-5.3-flash":  {Model: "z-ai/glm-5.3-flash", Limit: 4, RecentCount: 1, Period: "pacific_day", ResetAt: future},
	}
	pp := premiumSnapshotFromQuotaMap(m4)
	if pp == nil {
		t.Fatalf("premium nil: pp=%v", pp)
	}
	if pp.Limit != 4 || pp.Used != 1 {
		t.Errorf("glm shared pool = %+v want limit4 used1", pp)
	}
	// premiumPoolModels[0] (luna) wins when present
	m5 := map[string]session.QuotaSnapshot{
		"openai/gpt-5.6-luna":        {Model: "openai/gpt-5.6-luna", Limit: 4, RecentCount: 3, Period: "pacific_day", ResetAt: future},
		"deepseek/deepseek-v4-flash": {Model: "deepseek/deepseek-v4-flash", Limit: 100, RecentCount: 1, Period: "pacific_day", ResetAt: future},
	}
	p5 := premiumSnapshotFromQuotaMap(m5)
	if p5.Used != 3 {
		t.Errorf("priority: used=%v want 3 (luna)", p5.Used)
	}
}
