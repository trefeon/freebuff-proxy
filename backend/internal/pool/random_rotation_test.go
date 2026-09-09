package pool

import (
	"context"
	"math/rand/v2"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// TestRandomRotationMode exercises the TOKEN_ROTATION=random path (issue #218,
// #224). It verifies that (a) acquisition succeeds, (b) the distribution is
// roughly uniform across tokens, and (c) no panic occurs under -race (the
// mutex guard that was missing before the fix).
func TestRandomRotationMode(t *testing.T) {
	ctx := context.Background()
	mocks := make([]*testutil.MockUpstream, 4)
	for i := range mocks {
		mocks[i] = testutil.NewMock()
	}
	p := newTestPoolCfg(t, func(cfg *config.Config) {
		cfg.TokenRotation = "random"
		cfg.AuthTokens = []string{"tok-0", "tok-1", "tok-2", "tok-3"}
	}, mocks...)

	// Seed the PRNG deterministically so the test is reproducible.
	p.randMu.Lock()
	p.randGen = rand.New(rand.NewPCG(1, 1))
	p.randMu.Unlock()

	const N = 200
	tokenCounts := make(map[int]int)
	for i := 0; i < N; i++ {
		lease, err := p.Acquire(ctx, "deepseek/deepseek-v4-flash")
		if err != nil {
			t.Fatalf("iteration %d: Acquire: %v", i, err)
		}
		if lease == nil {
			t.Fatalf("iteration %d: nil lease", i)
			return
		}
		if lease.Token < 0 || lease.Token >= 4 {
			t.Errorf("iteration %d: token index %d out of range [0,4)", i, lease.Token)
		}
		tokenCounts[lease.Token]++
		p.LeaseRelease(lease)
	}

	for idx := 0; idx < 4; idx++ {
		if tokenCounts[idx] == 0 {
			t.Errorf("token %d was never picked in %d iterations (random mode); very unlikely", idx, N)
		}
	}
	t.Logf("token distribution: %v", tokenCounts)
}

// TestRandomRotationModeConcurrent verifies that the random path does not
// panic when called concurrently (the fix for issue #218). Runs under -race.
func TestRandomRotationModeConcurrent(t *testing.T) {
	ctx := context.Background()
	mocks := make([]*testutil.MockUpstream, 4)
	for i := range mocks {
		mocks[i] = testutil.NewMock()
	}
	p := newTestPoolCfg(t, func(cfg *config.Config) {
		cfg.TokenRotation = "random"
		cfg.AuthTokens = []string{"tok-0", "tok-1", "tok-2", "tok-3"}
	}, mocks...)
	p.randMu.Lock()
	p.randGen = rand.New(rand.NewPCG(42, 1))
	p.randMu.Unlock()

	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() {
			lease, err := p.Acquire(ctx, "deepseek/deepseek-v4-flash")
			if err != nil {
				errs <- err
				return
			}
			if lease != nil {
				p.LeaseRelease(lease)
			}
			errs <- nil
		}()
	}
	for i := 0; i < 20; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent acquire: %v", err)
		}
	}
}
