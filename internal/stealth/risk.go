package stealth

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// RiskLevel classifies a computed ban-risk score.
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// RiskSample is one passive observation feeding the engine: egress geo from
// a probe result and ip-capping / privacy signals from an upstream session
// response. All fields are optional; the engine tolerates partial samples.
type RiskSample struct {
	At time.Time
	// EgressIP / Country come from the egress probe (the public IP and
	// 2-letter ISO country seen at the far end of the outbound path).
	EgressIP string
	Country  string
	// IPPrivacySignals is the upstream's own classification of the egress
	// IP (e.g. "vpn", "proxy", "tor", "hosting", "res_proxy", "datacenter")
	// from the session response body (ipPrivacySignals).
	IPPrivacySignals []string
	// ActiveUsersForIP / Limit come from the ip_capped admission response:
	// distinct users currently active on the egress IP versus the ceiling.
	ActiveUsersForIP int
	Limit            float64
}

// RiskState is the engine's verdict: a 0-100 score, a level, and the
// human-readable drivers. Read-only — the engine never takes action.
type RiskState struct {
	Score   int
	Level   RiskLevel
	Reasons []string
	// Samples is how many observations the verdict considered (0 = engine
	// has seen nothing yet).
	Samples int
	At      time.Time
}

// maxRiskSamples bounds the ring buffer so a long-lived process does not
// accumulate unbounded observations.
const maxRiskSamples = 64

// RiskEngine is a passive, thread-safe ban-risk predictor (issue #64). It
// consumes samples from the egress probe loop and upstream session/probe
// responses and computes a risk score with simple, explainable rules:
//
//   - ip_capped proximity: the closer activeUsersForIp/limit is to the
//     ceiling, the more contended the egress IP is (admission is per-IP).
//   - privacy signals: the upstream flagging the egress IP as vpn/proxy/
//     tor/hosting/datacenter is the strongest single ban signal
//     (reference/freebuff freebuff-trust.ts: those egress classes feed
//     ipPrivacySignals, and a live risk >= 75 applies the anonymous_network
//     cap — trust level floors at `verified` with restricted-cohort spend).
//     The full 4-level matrix (new/verified/established/core, thresholds
//     25/50/75, DB-failure fail-open to established) is server-side; this
//     engine approximates only its observable inputs.
//
// The engine only warns (Score/Level/Reasons); it never modifies routing.
// The shared DefaultRiskEngine is fed by the upstream client and the egress
// probe loop; server/dashboard code reads its Score().
type RiskEngine struct {
	mu      sync.Mutex
	samples []RiskSample // oldest first; capped at maxRiskSamples
}

// NewRiskEngine returns an empty engine.
func NewRiskEngine() *RiskEngine {
	return &RiskEngine{}
}

// DefaultRiskEngine is the process-wide risk sink fed by upstream session
// responses and egress probe results, and read by the health/dashboard
// surfaces. It is a plain *RiskEngine, safe for concurrent use.
var DefaultRiskEngine = NewRiskEngine()

// Observe records one sample. Samples older than the newest maxRiskSamples
// are dropped (ring buffer, oldest first).
func (e *RiskEngine) Observe(s RiskSample) {
	if e == nil {
		return
	}
	if s.At.IsZero() {
		s.At = time.Now()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.samples = append(e.samples, s)
	if len(e.samples) > maxRiskSamples {
		e.samples = append(e.samples[:0], e.samples[len(e.samples)-maxRiskSamples:]...)
	}
}

// Reset drops all observations (test/cleanup seam).
func (e *RiskEngine) Reset() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.samples = nil
}

// Score computes the current risk verdict from the retained samples. An
// engine with no samples reports Score 0 / low with an empty reason list.
func (e *RiskEngine) Score() RiskState {
	if e == nil {
		return RiskState{Level: RiskLow, At: time.Now()}
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if len(e.samples) == 0 {
		return RiskState{Level: RiskLow, At: time.Now()}
	}

	// Aggregate across the retained window: the peak ip-capped ratio and the
	// union of privacy signals seen on any sample win (a single bad sample
	// is a genuine signal — the upstream re-flags it every response).
	seen := make(map[string]bool)
	worstRatio := 0.0
	worstLimit := 0.0
	latest := e.samples[len(e.samples)-1].At
	for _, s := range e.samples {
		if s.Limit > 0 && s.ActiveUsersForIP > 0 {
			ratio := float64(s.ActiveUsersForIP) / s.Limit
			if ratio > worstRatio {
				worstRatio = ratio
				worstLimit = s.Limit
			}
		}
		for _, sig := range s.IPPrivacySignals {
			if sig = strings.ToLower(strings.TrimSpace(sig)); sig != "" {
				seen[sig] = true
			}
		}
	}

	score := 0
	var reasons []string

	// Rule 1: privacy signals. The upstream's own egress-IP classification
	// is the strongest ban predictor: freebuff-trust.ts treats vpn/proxy/
	// tor/res_proxy/hosting/datacenter egress as a hard block or limited
	// demotion, so a SINGLE such flag is already high-risk ("egress
	// proxy/tor/hosting = high"). Additional distinct signals add weight,
	// capped so a chatty list cannot drive the score alone.
	if len(seen) > 0 {
		sigs := make([]string, 0, len(seen))
		for sig := range seen {
			sigs = append(sigs, sig)
		}
		sort.Strings(sigs)
		egressClass := false
		for _, sig := range sigs {
			switch sig {
			case "vpn", "proxy", "tor", "hosting", "datacenter", "res_proxy", "spur-flagged":
				egressClass = true
			}
		}
		if egressClass {
			score += 40
		}
		extra := 10 * (len(sigs) - 1)
		if extra > 20 {
			extra = 20
		}
		score += extra
		reasons = append(reasons, "egress IP flagged by upstream privacy signals: "+strings.Join(sigs, ", "))
	}

	// Rule 2: ip_capped proximity. The ceiling is the server's configured
	// distinct-users-per-IP admission cap; sitting near it means the egress
	// IP is heavily contended (and one more account is a ban-adjacent event
	// for the whole IP).
	if worstLimit > 0 {
		switch {
		case worstRatio >= 0.7:
			score += 30
			reasons = append(reasons, "egress IP near session cap")
		case worstRatio >= 0.5:
			score += 20
			reasons = append(reasons, "egress IP moderately contended")
		case worstRatio >= 0.3:
			score += 10
			reasons = append(reasons, "egress IP somewhat contended")
		}
	}

	// Rule 3: egress from a proxy/relay country that the free tier treats
	// as restricted is only surfaced when the upstream itself flagged it —
	// the signals above already carry that; the country alone is not a
	// signal without the upstream's classification.

	if score > 100 {
		score = 100
	}
	level := RiskLow
	switch {
	case score >= 40:
		level = RiskHigh
	case score >= 30:
		level = RiskMedium
	}
	return RiskState{
		Score:   score,
		Level:   level,
		Reasons: reasons,
		Samples: len(e.samples),
		At:      latest,
	}
}
