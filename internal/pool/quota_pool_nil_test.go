package pool

import (
	"testing"
	"time"

	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
)

func TestPoolPremiumQuotaNilWhenNoSession(t *testing.T) {
	emptyPool := &Pool{}
	if got := emptyPool.PremiumSnapshotForToken(0); got != nil {
		t.Errorf("PremiumSnapshotForToken on empty pool = %+v want nil", got)
	}
	if got := emptyPool.PremiumQuotaForToken(-1); got != nil {
		t.Errorf("PremiumQuotaForToken(-1) = %+v want nil", got)
	}
	if got := emptyPool.Glm53QuotaForToken(0); got != nil {
		t.Errorf("Glm53QuotaForToken on empty pool = %+v want nil", got)
	}
	if got := emptyPool.PremiumQuotaForBridge("any"); got != nil {
		t.Errorf("PremiumQuotaForBridge on empty pool = %+v want nil", got)
	}
	if got := emptyPool.Glm53QuotaForBridge("any"); got != nil {
		t.Errorf("Glm53QuotaForBridge on empty pool = %+v want nil", got)
	}
	// Pool with token but no quota
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	if got := p.PremiumSnapshotForToken(0); got != nil {
		t.Errorf("PremiumSnapshotForToken with no quota = %+v want nil", got)
	}
	if got := p.Glm53QuotaForToken(0); got != nil {
		t.Errorf("Glm53QuotaForToken with no quota = %+v want nil", got)
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
		"deepseek/deepseek-v4-flash": {Model: "deepseek/deepseek-v4-flash", Limit: 5, RecentCount: 2, Period: "pacific_day", ResetAt: future},
	}
	premium, glm := premiumSnapshotFromQuotaMap(m)
	if premium == nil {
		t.Fatal("premium nil")
	}
	if glm != nil {
		t.Fatalf("glm = %+v want nil", glm)
	}
	if premium.Limit != 5 || premium.Used != 2 || premium.Remaining != 3 || premium.PercentUsed != 40 {
		t.Errorf("math 5/2 = %+v want limit5 used2 remaining3 percent40", premium)
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
		"deepseek/deepseek-v4-flash": {Model: "deepseek/deepseek-v4-flash", Limit: 5, RecentCount: 5, Period: "pacific_day", ResetAt: future},
	}
	p2, _ := premiumSnapshotFromQuotaMap(m2)
	if !p2.Capped {
		t.Error("capped false want true for future reset")
	}
	m3 := map[string]session.QuotaSnapshot{
		"deepseek/deepseek-v4-flash": {Model: "deepseek/deepseek-v4-flash", Limit: 5, RecentCount: 5, Period: "pacific_day", ResetAt: past},
	}
	p3, _ := premiumSnapshotFromQuotaMap(m3)
	if p3.Capped {
		t.Error("capped true want false for past reset")
	}
	// glm53 lane independent
	m4 := map[string]session.QuotaSnapshot{
		"deepseek/deepseek-v4-flash": {Model: "deepseek/deepseek-v4-flash", Limit: 5, RecentCount: 1, Period: "pacific_day", ResetAt: future},
		"z-ai/glm-5.3-flash":         {Model: "z-ai/glm-5.3-flash", Limit: 2, RecentCount: 1, Period: "glm_v53_flash", ResetAt: future},
	}
	pp, gg := premiumSnapshotFromQuotaMap(m4)
	if pp == nil || gg == nil {
		t.Fatalf("premium/glm nil: pp=%v gg=%v", pp, gg)
	}
	if gg.Limit != 2 || gg.Used != 1 || gg.Remaining != 1 {
		t.Errorf("glm = %+v want limit2 used1 remaining1", gg)
	}
	// multiple premiums picks flash priority
	m5 := map[string]session.QuotaSnapshot{
		"deepseek/deepseek-v4-pro":   {Model: "deepseek/deepseek-v4-pro", Limit: 5, RecentCount: 4, Period: "pacific_day", ResetAt: future},
		"openai/gpt-5.6-luna":        {Model: "openai/gpt-5.6-luna", Limit: 5, RecentCount: 3, Period: "pacific_day", ResetAt: future},
		"deepseek/deepseek-v4-flash": {Model: "deepseek/deepseek-v4-flash", Limit: 5, RecentCount: 1, Period: "pacific_day", ResetAt: future},
	}
	p5, _ := premiumSnapshotFromQuotaMap(m5)
	if p5.Used != 1 {
		t.Errorf("priority: used=%d want 1 (flash)", p5.Used)
	}
}
