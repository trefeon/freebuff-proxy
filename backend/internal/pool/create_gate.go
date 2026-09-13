package pool

// Session-create admission gate (issue #86): bounds concurrent in-flight
// session admissions so a burst of requests cannot hammer the upstream
// create endpoint. Mirrors reference/gateways/freebuff-reverse/internal/session/
// create_gate.go: per-model (and global) in-flight counters, wait-or-503 —
// an acquire parks on a broadcast channel until a slot frees or its context
// expires (the caller's deadline becomes the 503).
//
// 0 (or negative) means UNLIMITED on either dimension: no local concurrency
// cap applies and the upstream quota/429 is the natural brake. The config
// defaults are 0, so the effective default is unlimited — set a value to
// bound a burst locally.

import (
	"context"
	"errors"
	"sync"
)

// ErrCreateGateBusy is returned by createGate.acquire when the gate is at
// capacity and the caller's context expires while waiting (wait-or-503: the
// server maps the context deadline to a 503).
var ErrCreateGateBusy = errors.New("pool: too many concurrent session creations")

// createPermit is one held create slot; Release returns it to the gate.
type createPermit struct {
	gate *createGate
	id   uint64
	once sync.Once
}

// Release returns the permit to the gate, waking any parked waiters.
func (p *createPermit) Release() {
	if p == nil || p.gate == nil {
		return
	}
	p.once.Do(func() { p.gate.release(p.id) })
}

// createGate counts in-flight session creates globally and per model, with
// a broadcast wakeup for waiters.
type createGate struct {
	mu          sync.Mutex
	changed     chan struct{}
	maxGlobal   int
	maxPerModel int

	nextID  uint64
	global  int
	byModel map[string]int
	pending map[uint64]string // id → model
}

// newCreateGate builds the gate with the given caps. A cap <= 0 means
// UNLIMITED on that dimension (no local concurrency cap; the upstream
// quota/429 is the natural brake). The clamp only applies between two
// positive caps, so it can never resurrect a cap when unlimited.
func newCreateGate(maxGlobal, maxPerModel int) *createGate {
	if maxGlobal < 0 {
		maxGlobal = 0
	}
	if maxPerModel < 0 {
		maxPerModel = 0
	}
	if maxGlobal > 0 && maxPerModel > maxGlobal {
		maxPerModel = maxGlobal
	}
	return &createGate{
		changed:     make(chan struct{}),
		maxGlobal:   maxGlobal,
		maxPerModel: maxPerModel,
		byModel:     make(map[string]int),
		pending:     make(map[uint64]string),
	}
}

// setLimits updates the caps at runtime (config reload). A cap <= 0 means
// UNLIMITED on that dimension; the clamp only applies between two positive
// caps. Loosening to unlimited wakes parked waiters to re-check.
func (g *createGate) setLimits(maxGlobal, maxPerModel int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if maxGlobal < 0 {
		maxGlobal = 0
	}
	if maxPerModel < 0 {
		maxPerModel = 0
	}
	if maxGlobal > 0 && maxPerModel > maxGlobal {
		maxPerModel = maxGlobal
	}
	g.maxGlobal = maxGlobal
	g.maxPerModel = maxPerModel
	// A changed cap may now admit parked waiters (or nobody); wake them to
	// re-check.
	g.notifyLocked()
}

// acquire reserves one create slot for model, waiting until a slot frees or
// ctx expires. It returns a permit on success and ctx.Err() (surfaced as
// 503 by the server) when the wait exceeds the caller's deadline. When both
// caps are unlimited the grant is an untracked permit immediately (mirroring
// chatGate.acquire's cap <= 0 fast path); a single unlimited dimension
// simply never blocks that dimension in canAcquireLocked.
func (g *createGate) acquire(ctx context.Context, model string) (*createPermit, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if g.unlimited() {
		return &createPermit{}, nil
	}
	for {
		g.mu.Lock()
		if g.canAcquireLocked(model) {
			g.nextID++
			id := g.nextID
			g.global++
			g.byModel[model]++
			g.pending[id] = model
			g.mu.Unlock()
			return &createPermit{gate: g, id: id}, nil
		}
		changed := g.changed
		g.mu.Unlock()

		select {
		case <-changed:
			// A slot freed; re-check.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// canAcquireLocked reports whether a create for model fits the caps. A cap
// <= 0 is unlimited and never blocks its dimension.
func (g *createGate) canAcquireLocked(model string) bool {
	if g.maxGlobal > 0 && g.global >= g.maxGlobal {
		return false
	}
	if g.maxPerModel > 0 && g.byModel[model] >= g.maxPerModel {
		return false
	}
	return true
}

// unlimited reports whether both caps are unlimited (untracked grants).
func (g *createGate) unlimited() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxGlobal <= 0 && g.maxPerModel <= 0
}

// release returns a permit's slot and wakes every waiter.
func (g *createGate) release(id uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	model, ok := g.pending[id]
	if !ok {
		return
	}
	delete(g.pending, id)
	if g.global > 0 {
		g.global--
	}
	if n := g.byModel[model]; n > 1 {
		g.byModel[model] = n - 1
	} else {
		delete(g.byModel, model)
	}
	g.notifyLocked()
}

// notifyLocked wakes every waiter parked on changed. Caller holds g.mu.
func (g *createGate) notifyLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}
