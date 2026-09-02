// Package testmock implements the simulated upstream for dummy/mock tokens
// (issue #271). The simulation (mockSessionState and its per-token quota
// ledger) lives here, outside the production wire client; the wire client
// branches on the narrow upstream.MockUpstream interface (c.mock), never on
// the token prefix. This package registers the dummy-token simulator via
// upstream.NewMockWire so the constructor can auto-wire it without the wire
// client importing the simulation.
package testmock

import (
	"fmt"
	"sync"
	"time"

	"freebuff-proxy/backend/internal/modelcat"
	"freebuff-proxy/backend/internal/upstream"
)

func init() {
	upstream.NewMockWire = func(token string) upstream.MockUpstream {
		if upstream.IsDummyToken(token) {
			return &mockUpstream{}
		}
		return nil
	}
}

// mockUpstream implements upstream.MockUpstream for dummy/mock tokens. The
// per-token quota state is tracked globally (getMockState), so a single
// stateless instance serves every dummy token.
type mockUpstream struct{}

func (m *mockUpstream) CreateSession(token, model string) (*upstream.SessionState, error) {
	return mockSessionState(token, model, true), nil
}

func (m *mockUpstream) GetSession(token, instanceID string) (*upstream.SessionState, error) {
	return mockSessionState(token, "", false), nil
}

func (m *mockUpstream) Probe(token string) (*upstream.SessionState, error) {
	return mockSessionState(token, "", false), nil
}

func (m *mockUpstream) EndSession(token string) error { return nil }

func (m *mockUpstream) StartRun(token, agentID string) (string, error) {
	return fmt.Sprintf("run-%s-%d", token, time.Now().UnixMilli()), nil
}

func (m *mockUpstream) FinishRun(token string) error { return nil }

// mockTokenState tracks per-token usage for dummy tokens so the mock behaves
// like a real upstream session: recentCount rises per model, standing score
// degrades on cooldown, and quota resets at Pacific midnight.
type mockTokenState struct {
	mu           sync.Mutex
	recentCounts map[string]float64 // model -> recentCount
	standing     float64            // current standing score (0-100)
}

var mockStates sync.Map // token -> *mockTokenState

// getMockState returns the (lazily created) usage state for a dummy token.
func getMockState(token string) *mockTokenState {
	v, _ := mockStates.LoadOrStore(token, &mockTokenState{
		recentCounts: map[string]float64{},
		standing:     95.0,
	})
	return v.(*mockTokenState)
}

// mockQuotaLimit mirrors the real tier caps for the served models (derived
// from modelcat: shared premium pool 4/day (luna, solar-pro4; glm-5.3-flash
// left it 2026-08-28 and is unmetered; solar's 1/day per-model cap closed
// 2026-09-01, upstream 051fd4d9), glm-5.2 promo 1/day, everything else
// unmetered). Paused models
// are never admitted so their cap is irrelevant — return 9999.
func mockQuotaLimit(model string) float64 {
	if model == "" {
		model = modelcat.DefaultModelID
	}
	switch model {
	case modelcat.Glm52ModelID:
		return 1
	default:
		if limit, _ := modelcat.PerModelCap(model); limit > 0 {
			return float64(limit)
		}
		if modelcat.IsPremium(model) {
			return modelcat.PremiumSessionLimit
		}
		return 9999 // mimo, fable, deepseek-flash: unmetered
	}
}

// mockSessionExpiry returns the session TTL for the mock: 1 hour for GLM
// 5.2 (upstream FREEBUFF_GLM_V52_SESSION_LENGTH_MS), 24 hours for everything
// else.
func mockSessionExpiry(model string) time.Duration {
	if model == modelcat.Glm52ModelID {
		return modelcat.GLMSessionLength
	}
	return 24 * time.Hour
}

// pacificLoc returns America/Los_Angeles, falling back to a fixed PDT zone
// when the tz database is unavailable.
var pacificLoc = sync.OnceValue(func() *time.Location {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return loc
	}
	return time.FixedZone("PDT", -7*60*60)
})

func mockSessionState(token string, requestedModel string, consume bool) *upstream.SessionState {
	if requestedModel == "" {
		requestedModel = modelcat.FallbackModelID
	}
	now := time.Now()
	pacific := pacificLoc()
	pacNow := now.In(pacific)
	pacMidnight := time.Date(pacNow.Year(), pacNow.Month(), pacNow.Day(), 0, 0, 0, 0, pacific)
	if !pacNow.Before(pacMidnight) {
		pacMidnight = pacMidnight.AddDate(0, 0, 1)
	}

	st := getMockState(token)
	st.mu.Lock()
	defer st.mu.Unlock()

	limit := mockQuotaLimit(requestedModel)
	if consume {
		st.recentCounts[requestedModel]++
	}
	recent := st.recentCounts[requestedModel]
	score := st.standing

	// Status transitions: active while quota remains; cooldown once recent >= limit.
	status := "active"
	var retryAfterMs int64
	if limit <= 9999 && recent >= limit {
		status = "cooldown"
		retryAfterMs = int64(time.Until(pacMidnight).Seconds() * 1000)
		if score > 60 {
			score -= 10
			st.standing = score
		}
	}
	if status == "active" && score < 95 {
		score++
		if score > 95 {
			score = 95
		}
		st.standing = score
	}

	instanceID := fmt.Sprintf("a%08x-b%04x-4%03x-8%03x-e%012x",
		now.UnixMilli()&0xFFFFFFFF, now.UnixMilli()&0xFFFF,
		now.UnixMilli()&0x0FFF, now.UnixMilli()&0x0FFF,
		now.UnixMilli()&0xFFFFFFFFFFFF)

	unlimited := float64(9999)

	state := &upstream.SessionState{
		Status:          status,
		InstanceID:      instanceID,
		Model:           requestedModel,
		CurrentModel:    requestedModel,
		RequestedModel:  requestedModel,
		ExpiresAt:       now.Add(mockSessionExpiry(requestedModel)),
		AdmittedAt:      now,
		Position:        0,
		QueueDepth:      0,
		EstimatedWaitMs: 0,
		PollAt:          now.Add(30 * time.Second),
		Limit:           limit,
		RecentCount:     recent,
		ResetAt:         pacMidnight,
		RetryAfterMs:    retryAfterMs,
		RateLimitsByModel: map[string]upstream.ModelQuota{
			"openai/gpt-5.6-luna": {
				Model:       modelcat.DefaultModelID,
				Limit:       modelcat.PremiumSessionLimit,
				RecentCount: st.recentCounts["openai/gpt-5.6-luna"],
				ResetAt:     pacMidnight,
				Period:      "pacific_day",
				Pool:        "premium",
				PoolLabel:   "Premium",
				Entitlement: map[string]float64{"base": modelcat.PremiumSessionLimit},
			},
			"upstage/solar-pro4": {
				Model:       modelcat.SolarPro4ModelID,
				Limit:       modelcat.PremiumSessionLimit,
				RecentCount: st.recentCounts["upstage/solar-pro4"],
				ResetAt:     pacMidnight,
				Period:      "pacific_day",
				Pool:        "premium",
				PoolLabel:   "Premium",
				Entitlement: map[string]float64{"base": modelcat.PremiumSessionLimit},
			},
			"z-ai/glm-5.3-flash": {
				Model:       modelcat.Glm53ModelID,
				Limit:       unlimited,
				RecentCount: st.recentCounts["z-ai/glm-5.3-flash"],
				ResetAt:     pacMidnight,
				Period:      "pacific_day",
				Pool:        "unlimited",
				PoolLabel:   "Unlimited",
			},
			"mimo/mimo-v2.5": {
				Model:       modelcat.FallbackModelID,
				Limit:       unlimited,
				RecentCount: st.recentCounts["mimo/mimo-v2.5"],
				ResetAt:     pacMidnight,
				Period:      "pacific_day",
				Pool:        "unlimited",
				PoolLabel:   "Unlimited",
			},
			"deepseek/deepseek-v4-flash": {
				Model:       "deepseek/deepseek-v4-flash",
				Limit:       unlimited,
				RecentCount: st.recentCounts["deepseek/deepseek-v4-flash"],
				ResetAt:     pacMidnight,
				Period:      "pacific_day",
				Pool:        "unlimited",
				PoolLabel:   "Unlimited",
			},
			"z-ai/glm-5.2": {
				Model:       modelcat.Glm52ModelID,
				Limit:       1,
				RecentCount: st.recentCounts["z-ai/glm-5.2"],
				ResetAt:     pacMidnight,
				Period:      "promo",
				Pool:        "glm-promo",
				PoolLabel:   "GLM Referral",
				Entitlement: map[string]float64{"referral": 1},
			},
		},
		Standing: &upstream.SessionStanding{
			Level:       "trusted",
			Label:       "Trusted",
			Score:       score,
			NextLevel:   "",
			CappedBy:    "third_party_client",
			Blurb:       "Your account is in good standing. Full access to all models.",
			NextLevelAt: time.Time{},
		},
		Referral: &upstream.SessionReferral{
			Code:                    "CB-MOCK0",
			ReferrerName:            "",
			QualifiedCount:          2,
			WeeklySessionsRemaining: 3,
			ResetAt:                 pacMidnight,
			GithubLinked:            true,
		},
	}

	if requestedModel == "z-ai/glm-5.2" {
		state.GlmPromo = fmt.Sprintf("{\"dailySessions\":%d,\"endsAt\":%q}",
			int(limit), pacMidnight.UTC().Format(time.RFC3339))
	}

	return state
}
