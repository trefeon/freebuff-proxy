// quota_bootseed.go — quota view boot seeding + first-probe recovery
// (ADR-0024).
//
// Two restart holes: nothing seeds the live quota view from the persisted
// quota_snapshots on boot (cards render empty until the next probe), and the
// ADR-0022 scheduler skips unknown-reset tokens — which after a restart is
// every token. This file closes both:
//
//   - SeedQuotaSnapshot pushes the latest persisted row per (token, model)
//     into the live view (CLI orchestrates: it queries the store and calls
//     this setter; the pool never imports the store). Seeded rows carry
//     their probe timestamp, show as last-known immediately, and teach the
//     scheduler their reset_at. The push is idempotent and never downgrades
//     fresher live data (the session manager compares source times).
//   - Unknown-reset tokens the seed never filled (truly never probed) get
//     one staggered boot probe: a token-hash spread slot inside the first
//     5m after Start, session-less ProbeToken, warn-only, once per process.
//
// QUOTA_AUTO_PROBE=false disables the boot probe too (the tick returns
// before reaching either path); the seed display still applies — showing
// persisted data is never probing.
package pool

import (
	"context"
	"hash/fnv"
	"strconv"
	"time"

	"freebuff-proxy/backend/internal/upstream"
)

// quotaBootProbeWindow is the post-boot window virgin-token probes stagger
// over (ADR-0024): one session-less GET per unprobed token per boot, spread
// so a fleet restart never fires as a burst.
const quotaBootProbeWindow = 5 * time.Minute

// QuotaSeedRow is one latest-per-(token,model) persisted quota sample for
// the boot seed. Token is the pool token index; ProbedAt is the row's probe
// timestamp (TS column); Entitlement is the decoded entitlements object
// (nil when the row carries none).
type QuotaSeedRow struct {
	Token       int
	Model       string
	Limit       float64
	Recent      float64
	ResetAt     time.Time
	ProbedAt    time.Time
	Entitlement map[string]float64
}

// SeedQuotaSnapshot pushes persisted quota rows into the live quota view.
// Rows for unknown token indexes or empty models are dropped. A token that
// absorbs at least one row is marked seeded, so the scheduler treats it as
// reset-known (normal ADR-0022 slots) instead of boot-probe eligible.
// Idempotent: re-pushing the same rows changes nothing.
//
// Call once at boot before Start; it touches no locks beyond the session
// managers' own.
func (p *Pool) SeedQuotaSnapshot(rows []QuotaSeedRow) {
	toks := p.roster.Load()
	if toks == nil {
		return
	}
	applied := 0
	for _, r := range rows {
		if r.Token < 0 || r.Token >= len(*toks) || r.Model == "" {
			continue
		}
		q := upstream.ModelQuota{
			Model:       r.Model,
			Limit:       r.Limit,
			RecentCount: r.Recent,
			ResetAt:     r.ResetAt,
			Entitlement: r.Entitlement,
		}
		if (*toks)[r.Token].session.SeedQuota(q, r.ProbedAt) {
			(*toks)[r.Token].quotaSeeded = true
			applied++
		}
	}
	if applied > 0 {
		p.logger.Debug("pool: quota boot seed applied", "rows", applied)
	}
}

// quotaBootProbeSlot returns the staggered boot-probe slot for token idx:
// boot plus an FNV-1a hash spread over the first 5m. Pure (no clock, no pool
// state) so tests pin determinism and spread directly. The hash covers the
// token index only — the spread separates tokens within one boot, which is
// the herd this staggers.
func quotaBootProbeSlot(idx int, boot time.Time) time.Time {
	h := fnv.New64a()
	_, _ = h.Write([]byte("boot-probe|" + strconv.Itoa(idx)))
	offset := time.Duration(h.Sum64() % uint64(quotaBootProbeWindow))
	return boot.Add(offset)
}

// quotaBootProbeDue is the pure firing predicate for the recovery probe: a
// real boot time (zero = pool never Started, e.g. unit tests — never due),
// not already probed this process, and now at/past the staggered slot.
func quotaBootProbeDue(idx int, bootProbed bool, boot, now time.Time) bool {
	if bootProbed || boot.IsZero() {
		return false
	}
	return !now.Before(quotaBootProbeSlot(idx, boot))
}

// quotaFireProbe issues one session-less ProbeToken with a 10s bound,
// warn-only on failure. The caller marks the day accounting first, so the
// attempt counts exactly once per token per Pacific day (success or
// failure alike). action names the log verb ("auto-probe", "boot probe").
func (p *Pool) quotaFireProbe(ctx context.Context, i int, tok *tokenEntry, action string) {
	label := tokenEntryLabel(tok)
	fire, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := p.ProbeToken(fire, i)
	cancel()
	if err != nil {
		p.logger.Warn("pool: quota "+action+" failed", "token", i+1, "token_label", label, "err", err)
		return
	}
	p.logger.Debug("pool: quota "+action+" refreshed", "token", i+1, "token_label", label)
}
