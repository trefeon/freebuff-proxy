// chat_gate.go - per-(token,model) in-flight chat lease cap (burst queue).
//
// A burst of concurrent chat requests must not fan out upstream on one
// account: the gate bounds how many granted leases may hold the same
// token+model lane at once. It mirrors createGate (per-key in-flight
// counters, broadcast wakeup, park until a slot frees or the caller context
// expires) — the caller's deadline becomes the 503, exactly like the create
// gate's wait-or-503.
//
// The cap is chosen per grant from CHAT_MAX_INFLIGHT_METERED vs
// CHAT_MAX_INFLIGHT_UNMETERED via the Freebucks price table (a model with a
// price is metered, nil price = unmetered). Cap <= 0 means unlimited: the
// grant is a no-op permit and nothing is tracked, so pools running with the
// knobs off behave exactly as before.
//
// The lane key is the entry POINTER plus model, never a roster index: a
// dashboard reorder/removal mid-wait must not merge or split lanes, and
// release always lands through the lease (LeaseRelease/LeaseAbandon), the
// same retired-entry discipline as the run release.
package pool

import (
	"context"
	"sync"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/session"
)

// chatDispatchJitterMax bounds the dispatch jitter applied AFTER a waiter is
// granted a slot: a free slot grants immediately (no sleep on idle), but a
// waiter that parked spreads its upstream dispatch so a released slot does
// not re-fire the whole herd in lockstep.
const chatDispatchJitterMax = 25 * time.Millisecond

// chatGateKey identifies one gated lane: the owning entry plus the served
// model. owner is *tokenEntry (pooled) or *bridgeEntry (bridge mode).
type chatGateKey struct {
	owner any
	model string
}

// chatPermit is one held chat-lane slot; Release returns it to the gate.
// A zero permit (nil gate) is the unlimited-cap fast path: Release no-ops.
type chatPermit struct {
	gate *chatGate
	id   uint64
	once sync.Once
}

// Release returns the permit to the gate, waking any parked waiters.
// Nil-safe: leases without a lane (unlimited caps, synthetic leases) no-op.
func (p *chatPermit) Release() {
	if p == nil || p.gate == nil {
		return
	}
	p.once.Do(func() { p.gate.release(p.id) })
}

// chatGate counts in-flight chat leases per (entry, model) lane, with a
// broadcast wakeup for waiters. Caps are supplied per acquire call (they ride
// the per-request config load), so the gate itself stores no limits and needs
// no reload wiring.
type chatGate struct {
	mu      sync.Mutex
	changed chan struct{}

	nextID   uint64
	inFlight map[chatGateKey]int
	pending  map[uint64]chatGateKey
	// peak records the highest simultaneous hold per lane (regression
	// observability: the serialize tests pin peak == cap).
	peak map[chatGateKey]int
	// parked counts goroutines currently waiting on changed.
	parked int
}

// newChatGate builds an empty gate.
func newChatGate() *chatGate {
	return &chatGate{
		changed:  make(chan struct{}),
		inFlight: make(map[chatGateKey]int),
		pending:  make(map[uint64]chatGateKey),
		peak:     make(map[chatGateKey]int),
	}
}

// acquire reserves one chat-lane slot for owner+model, waiting until a slot
// frees or ctx expires. It returns the permit, whether the caller parked
// before the grant (dispatch jitter applies), and ctx.Err() when the wait
// exceeds the caller's deadline — the same wait-or-503 contract as the
// create gate. cap <= 0 (knobs off) grants an untracked permit immediately.
func (g *chatGate) acquire(ctx context.Context, owner any, model string, cap int) (*chatPermit, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cap <= 0 {
		return &chatPermit{}, false, nil
	}
	key := chatGateKey{owner: owner, model: model}
	waited := false
	for {
		g.mu.Lock()
		if g.inFlight[key] < cap {
			g.nextID++
			id := g.nextID
			g.inFlight[key]++
			if g.inFlight[key] > g.peak[key] {
				g.peak[key] = g.inFlight[key]
			}
			g.pending[id] = key
			g.mu.Unlock()
			if waited {
				// Jitter only on dispatch-after-wait: a free slot grants
				// immediately, a woken waiter spreads its dispatch. The
				// slot stays held across the sleep; a context expiring
				// mid-jitter releases it back (zero leaked inflight).
				if err := sleepChatJitter(ctx); err != nil {
					g.release(id)
					return nil, true, err
				}
			}
			return &chatPermit{gate: g, id: id}, waited, nil
		}
		changed := g.changed
		g.parked++
		g.mu.Unlock()

		select {
		case <-changed:
			g.mu.Lock()
			g.parked--
			g.mu.Unlock()
			waited = true
		case <-ctx.Done():
			g.mu.Lock()
			g.parked--
			g.mu.Unlock()
			return nil, waited, ctx.Err()
		}
	}
}

// release returns a permit's slot and wakes every waiter.
func (g *chatGate) release(id uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key, ok := g.pending[id]
	if !ok {
		return
	}
	delete(g.pending, id)
	if n := g.inFlight[key]; n > 1 {
		g.inFlight[key] = n - 1
	} else {
		delete(g.inFlight, key)
	}
	g.notifyLocked()
}

// notifyLocked wakes every waiter parked on changed. Caller holds g.mu.
func (g *chatGate) notifyLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// held reports the current in-flight count for one lane.
func (g *chatGate) held(key chatGateKey) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlight[key]
}

// peakHeld reports the highest simultaneous hold observed for one lane.
func (g *chatGate) peakHeld(key chatGateKey) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak[key]
}

// totalHeld reports the in-flight count across all lanes.
func (g *chatGate) totalHeld() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, v := range g.inFlight {
		n += v
	}
	return n
}

// parkedWaiters reports how many goroutines are currently parked on the gate.
func (g *chatGate) parkedWaiters() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.parked
}

// sleepChatJitter sleeps up to chatDispatchJitterMax (pool jitter source),
// honoring ctx cancellation.
func sleepChatJitter(ctx context.Context) error {
	d := time.Duration(sessionRand() % uint64(chatDispatchJitterMax+1))
	if d == 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// chatCap selects the in-flight chat cap for one grant: the metered knob
// when the model carries a Freebucks price, the unmetered knob otherwise.
// Read off the per-request config load (p.cfg.Load()), never cached.
func chatCap(cfg *config.Config, metered bool) int {
	if metered {
		return cfg.ChatMaxInflightMetered
	}
	return cfg.ChatMaxInflightUnmetered
}

// chatMetered reports whether model is metered for the pooled token: it has
// a Freebucks price (nil price = unmetered). Reuses the acquire-order price
// lookup so the two can never disagree on classification.
func chatMetered(tok *tokenEntry, model string) bool {
	_, ok := freebucksBalance(tok, model)
	return ok
}

// chatBridgeMetered is the bridge-mode analog over the entry snapshot: a
// model with a price is metered, nil price (or no Freebucks block) is
// unmetered.
func chatBridgeMetered(snap session.SessionSnapshot, model string) bool {
	fb := snap.Freebucks
	if fb == nil {
		return false
	}
	_, ok := fb.Prices[model]
	return ok
}
