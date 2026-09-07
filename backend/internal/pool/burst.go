// burst.go — burst load-balance (ADR-0023, opt-in).
//
// Drain is the safest default: one account exhausts before rotating,
// mimicking single-user behavior. But a model hammered as a subagent engine
// hits one account's throttle while siblings with the same model sit idle.
// Burst balance is the middle ground: while one model's sliding-window
// admission count exceeds BURST_THRESHOLD, selection for THAT model switches
// to least_used among healthy tokens, capped at BURST_MAX_TOKENS distinct
// accounts. Every other model — and the disabled state — follows the
// configured strategy untouched.
//
// State is in-memory only (a restart starts un-tripped) and pruned on the
// maintain tick. Episode entry/exit is WARN-logged per model.
package pool

import (
	"sort"
	"time"

	"freebuff-proxy/backend/internal/config"
)

// Burst defaults mirror the config defaults. The pool normalizes
// zero-values instead of trusting them: tests construct Config literals
// directly, bypassing Load (config CONTRACT).
const (
	defaultBurstWindow    = time.Minute
	defaultBurstThreshold = 20
	defaultBurstMaxTokens = 2
)

// burstHit is one granted pooled lease for burst accounting.
type burstHit struct {
	at  time.Time
	tok int
}

// burstPlan is the order-time snapshot for one model: whether the burst
// override applies plus the spread set the reorder needs. Pure data —
// acquireOrder applies it without touching burst state.
type burstPlan struct {
	active    bool
	maxTokens int
	members   map[int]bool
}

// burstLimits normalizes the live knobs (zero = unset → default).
func burstLimits(cfg *config.Config) (window time.Duration, threshold, maxTokens int) {
	window, threshold, maxTokens = cfg.BurstWindow, cfg.BurstThreshold, cfg.BurstMaxTokens
	if window <= 0 {
		window = defaultBurstWindow
	}
	if threshold <= 0 {
		threshold = defaultBurstThreshold
	}
	if maxTokens < 2 {
		maxTokens = defaultBurstMaxTokens
	}
	return window, threshold, maxTokens
}

// burstPlanForModel snapshots the burst decision for model at now: active
// exactly when the kill-switch is on and the live-window admission count
// exceeds the threshold. The spread set is derived fresh from the window
// hits (most-admissions first, first-seen tie-break, trimmed to maxTokens),
// so a cooled token that stops serving slides out of the set on its own —
// no grant-time membership bookkeeping to drift.
func (p *Pool) burstPlanForModel(cfg *config.Config, model string, now time.Time) burstPlan {
	if cfg == nil || !cfg.BurstBalanceEnabled {
		return burstPlan{}
	}
	window, threshold, maxTokens := burstLimits(cfg)
	cutoff := now.Add(-window)
	p.burstMu.Lock()
	defer p.burstMu.Unlock()
	counts := make(map[int]int)
	var firstSeen []int
	total := 0
	for _, h := range p.burstHits[model] {
		if h.at.After(cutoff) {
			total++
			if counts[h.tok] == 0 {
				firstSeen = append(firstSeen, h.tok)
			}
			counts[h.tok]++
		}
	}
	if total <= threshold {
		return burstPlan{}
	}
	sort.SliceStable(firstSeen, func(i, j int) bool {
		return counts[firstSeen[i]] > counts[firstSeen[j]]
	})
	if len(firstSeen) > maxTokens {
		firstSeen = firstSeen[:maxTokens]
	}
	members := make(map[int]bool, len(firstSeen))
	for _, tok := range firstSeen {
		members[tok] = true
	}
	return burstPlan{active: true, maxTokens: maxTokens, members: members}
}

// apply reorders a least_used ranking so episode members come first, then
// up to (maxTokens - members-serving) newcomers, then the demoted
// remainder. Demoted tokens stay in the order: failover still reaches them
// when the spread set cannot serve — the cap bounds healthy-case spreading,
// never availability.
func (plan burstPlan) apply(ranked []int) []int {
	if !plan.active {
		return ranked
	}
	var in, out []int
	for _, idx := range ranked {
		if plan.members[idx] {
			in = append(in, idx)
		} else {
			out = append(out, idx)
		}
	}
	fill := plan.maxTokens - len(in)
	if fill < 0 {
		fill = 0
	}
	if fill > len(out) {
		fill = len(out)
	}
	return append(append(in, out[:fill]...), out[fill:]...)
}

// burstRecord accounts one granted pooled lease for model. No-op unless the
// kill-switch is on, so the disabled state keeps zero burst state. The
// crossing grant WARN-logs the entry edge once per episode (burstOn flag);
// the exit edge fires from the maintain-tick prune.
func (p *Pool) burstRecord(model string, idx int) {
	p.burstRecordAt(model, idx, time.Now())
}

// burstRecordAt is burstRecord with the clock injected (deterministic
// window-expiry tests).
func (p *Pool) burstRecordAt(model string, idx int, now time.Time) {
	cfg := p.cfg.Load()
	if cfg == nil || !cfg.BurstBalanceEnabled {
		return
	}
	window, threshold, maxTokens := burstLimits(cfg)
	cutoff := now.Add(-window)
	p.burstMu.Lock()
	defer p.burstMu.Unlock()
	if p.burstHits == nil {
		p.burstHits = make(map[string][]burstHit)
	}
	if p.burstOn == nil {
		p.burstOn = make(map[string]bool)
	}
	p.burstHits[model] = append(p.burstHits[model], burstHit{at: now, tok: idx})
	total := 0
	for _, h := range p.burstHits[model] {
		if h.at.After(cutoff) {
			total++
		}
	}
	if total > threshold && !p.burstOn[model] {
		p.burstOn[model] = true
		p.logger.Warn("pool: burst balance engaged (spreading load across accounts)",
			"model", model, "admissions", total, "threshold", threshold,
			"window", window.String(), "max_tokens", maxTokens)
	}
}

// burstPruneAt drops out-of-window hits (memory hygiene) and fires the exit
// edge for episodes whose live-window count recovered to threshold. Called
// from maintainTick on every pass — the ADR-0023 prune site.
func (p *Pool) burstPruneAt(now time.Time) {
	cfg := p.cfg.Load()
	window, threshold, _ := defaultBurstWindow, defaultBurstThreshold, defaultBurstMaxTokens
	if cfg != nil {
		window, threshold, _ = burstLimits(cfg)
	}
	cutoff := now.Add(-window)
	p.burstMu.Lock()
	defer p.burstMu.Unlock()
	for model, hits := range p.burstHits {
		kept := hits[:0]
		for _, h := range hits {
			if h.at.After(cutoff) {
				kept = append(kept, h)
			}
		}
		// Zero the dropped tail so pruned entries cannot linger past the
		// slice reuse.
		for i := len(kept); i < len(hits); i++ {
			hits[i] = burstHit{}
		}
		if len(kept) == 0 {
			delete(p.burstHits, model)
		} else {
			p.burstHits[model] = kept
		}
		if p.burstOn[model] && len(kept) <= threshold {
			delete(p.burstOn, model)
			p.logger.Warn("pool: burst balance recovered (steady selection)",
				"model", model, "admissions", len(kept), "threshold", threshold)
		}
	}
}
