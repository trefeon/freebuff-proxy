package pool

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// TestFixedTokenBannedQuarantined pins the terminal-ban quarantine (anti-ban
// contract): a fixed pooled token whose admission returns a 403 banned is
// marked permanently ineligible — further Acquires skip it (no re-admission
// attempts), the state is surfaced in the pool stats (per-token label +
// pool-wide counter), and exactly one slog.Warn is logged.
func TestFixedTokenBannedQuarantined(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true

	var buf bytes.Buffer
	testLogger := slog.New(slog.NewTextHandler(&buf, nil))
	p := newTestPool(t, mock)
	p.logger = testLogger // internal test: capture the pool's logger

	// First acquire: the token's admission hits 403 banned.
	_, err := p.Acquire(context.Background(), modelA)
	var be *upstream.BanError
	if !errors.As(err, &be) {
		t.Fatalf("want *upstream.BanError, got %v", err)
	}

	// The token is quarantined: the state surfaces in the per-token snapshot
	// and the pool-wide counter.
	snap := p.Snapshot()[0]
	if !snap.Quarantined {
		t.Fatal("Snapshot().Quarantined = false after a banned refusal")
	}
	if snap.QuarantineReason != "banned" {
		t.Errorf("QuarantineReason = %q, want banned", snap.QuarantineReason)
	}
	if got := p.PoolSnapshot().Quarantined; got != 1 {
		t.Errorf("PoolSnapshot().Quarantined = %d, want 1", got)
	}

	// Exactly one slog.Warn per token.
	if got := strings.Count(buf.String(), "pool: token quarantined"); got != 1 {
		t.Errorf("quarantine warn count = %d, want exactly 1 (buf=%q)", got, buf.String())
	}

	// A further Acquire skips the quarantined token: it surfaces the
	// remembered 403 banned and does NOT re-hit upstream.
	before := mock.RequestCount()
	_, err = p.Acquire(context.Background(), modelA)
	if !errors.As(err, &be) {
		t.Fatalf("second acquire: want *upstream.BanError, got %v", err)
	}
	if !errors.Is(err, upstream.ErrBanned) {
		t.Errorf("second acquire errors.Is(ErrBanned) = false")
	}
	if after := mock.RequestCount(); after != before {
		t.Errorf("upstream requests after quarantine = %d, want %d (no re-admission)", after, before)
	}
	// Compare-and-swap guards the single fire, so the second acquire does not
	// re-log.
	if got := strings.Count(buf.String(), "pool: token quarantined"); got != 1 {
		t.Errorf("quarantine warn count after second acquire = %d, want 1", got)
	}
}

// TestRateLimitedCooldownOnlyNotQuarantined pins the cooldown-only refusal:
// a 429 rate_limited cools the token down but must NOT quarantine it, and
// the token is still used after the window (it is not a dead account).
func TestRateLimitedCooldownOnlyNotQuarantined(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimit = true
	mock.RateLimitRetryAfterMs = 100 // short window so the token proves reusable
	p := newTestPool(t, mock)

	_, err := p.Acquire(context.Background(), modelA)
	if !errors.Is(err, upstream.ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}

	// Cooldown only, never quarantined.
	if snap := p.Snapshot()[0]; snap.Quarantined {
		t.Fatal("Snapshot().Quarantined = true after a rate_limited refusal (cooldown only)")
	}
	if got := p.PoolSnapshot().Quarantined; got != 0 {
		t.Errorf("PoolSnapshot().Quarantined = %d, want 0", got)
	}

	// The token is not dead: once the refusal clears it is used again.
	mock.RateLimit = false
	eventually(t, "rate-limited token recovers", func() bool {
		_, err := p.Acquire(context.Background(), modelA)
		return err == nil
	})
}

// TestBridgeTokenBannedNoQuarantine pins the bridge-mode semantics: a
// per-request bridge token's 403 ban surfaces to the client as today and is
// NOT quarantined (only fixed pooled tokens qualify for quarantine), so no
// fixed-token quarantine bookkeeping is created.
func TestBridgeTokenBannedNoQuarantine(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true
	p := newTestPoolCfg(t, func(c *config.Config) { c.UpstreamBaseURL = mock.URL() })

	_, err := p.AcquireBridge(context.Background(), "client-token", modelA)
	if !errors.Is(err, upstream.ErrBanned) {
		t.Fatalf("bridge acquire: want ErrBanned, got %v", err)
	}
	if got := p.PoolSnapshot().Quarantined; got != 0 {
		t.Errorf("PoolSnapshot().Quarantined = %d, want 0 (bridge never quarantines)", got)
	}
	if snaps := p.Snapshot(); len(snaps) > 0 && snaps[0].Quarantined {
		t.Error("fixed-token snapshot shows a quarantine although only the bridge refused")
	}
}

// TestQuarantineResetsOnConfigMemberChange pins the rebuild semantics: a
// quarantine survives across Acquire calls (the token stays skipped) and is
// gone once AUTH_TOKENS changes at that slot, because the slot is REBUILT
// for the new account — the old entry (with its terminal state) is retired
// and a fresh entry constructed for the new token. The dead account is
// never re-admitted, and the marker never leaks onto the replacement.
func TestQuarantineResetsOnConfigMemberChange(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true
	p := newTestPool(t, mock)

	_, err := p.Acquire(context.Background(), modelA)
	if !errors.Is(err, upstream.ErrBanned) {
		t.Fatalf("first acquire: want ErrBanned, got %v", err)
	}
	if !p.Snapshot()[0].Quarantined {
		t.Fatal("token not quarantined after a ban")
	}
	old := (*p.roster.Load())[0]

	// Operator edits AUTH_TOKENS: replace the account at slot 0. Copy the
	// config slice so the live pool config is untouched; rebuild the entry
	// client against the mock (the pool config keeps the production URL).
	newCfg := *p.cfg.Load()
	newCfg.AuthTokens = append([]string(nil), newCfg.AuthTokens...)
	newCfg.AuthTokens[0] = "tok-replaced"
	newCfg.UpstreamBaseURL = mock.URL()
	p.SetConfig(&newCfg)

	cur := (*p.roster.Load())[0]
	if cur == old {
		t.Fatal("slot not rebuilt: same entry object after AUTH_TOKENS change")
	}
	if cur.token != "tok-replaced" {
		t.Errorf("rebuilt entry token = %q, want tok-replaced", cur.token)
	}
	if cur.quarantine.Load() != nil {
		t.Error("rebuilt entry carries the old account's quarantine")
	}
	if q := p.Snapshot()[0]; q.Quarantined {
		t.Error("quarantine still surfacing after the slot was rebuilt")
	}
	// Pool-wide counters agree.
	if got := p.PoolSnapshot().Quarantined; got != 0 {
		t.Errorf("PoolSnapshot().Quarantined = %d, want 0", got)
	}
}

// TestSetConfigRebuildsDrainsReplacedEntry pins the end-to-end rebuild: a
// slot whose token changed on reload gets a FRESH entry (built like
// AddToken) while the old account's run is FINISHed and its admitted
// session is ended — no upstream activity is left behind, and the replaced
// token serves traffic immediately.
func TestSetConfigRebuildsDrainsReplacedEntry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	if got := mock.StartedRunsSnapshot(); len(got) != 1 {
		t.Fatalf("started runs = %v, want 1", got)
	}
	if mock.SessionEnds != 0 {
		t.Fatalf("session ends = %d before rebuild, want 0", mock.SessionEnds)
	}
	old := (*p.roster.Load())[0]

	newCfg := *p.cfg.Load()
	newCfg.AuthTokens = append([]string(nil), newCfg.AuthTokens...)
	newCfg.AuthTokens[0] = "tok-replaced"
	newCfg.UpstreamBaseURL = mock.URL()
	p.SetConfig(&newCfg)

	// The old account's run was FINISHed and its session ended
	// (synchronous inside SetConfig when no lease is in flight).
	if got := mock.FinishedRunsSnapshot(); len(got) != 1 || got[0].Status != "completed" {
		t.Errorf("finished runs = %v, want 1 completed (old entry drained)", got)
	}
	if mock.SessionEnds != 1 {
		t.Errorf("session ends = %d, want 1 (old entry's session ended)", mock.SessionEnds)
	}
	// The old entry is retired, not serving.
	p.retiredMu.Lock()
	_, parked := p.retired[old]
	p.retiredMu.Unlock()
	if parked {
		t.Error("old entry still parked after synchronous drain")
	}

	// The replacement token serves traffic immediately through the new
	// client (same mock upstream).
	lease, err = p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("acquire after rebuild: %v", err)
	}
	p.LeaseRelease(lease)
	if got := mock.StartedRunsSnapshot(); len(got) != 2 {
		t.Errorf("started runs = %v, want 2 (one per token generation)", got)
	}
}

// TestSetConfigKeepsQuarantineWhenTokenUnchanged pins the inverse of the
// rebuild: a reload that does NOT change the slot's token must leave the
// entry (and its terminal quarantine) untouched — no automatic revival.
func TestSetConfigKeepsQuarantineWhenTokenUnchanged(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true
	p := newTestPool(t, mock)

	_, err := p.Acquire(context.Background(), modelA)
	if !errors.Is(err, upstream.ErrBanned) {
		t.Fatalf("first acquire: want ErrBanned, got %v", err)
	}
	if !p.Snapshot()[0].Quarantined {
		t.Fatal("token not quarantined after a ban")
	}

	newCfg := *p.cfg.Load()
	p.SetConfig(&newCfg)
	if !p.Snapshot()[0].Quarantined {
		t.Error("quarantine cleared by an unchanged reload")
	}
	if cur := (*p.roster.Load())[0]; cur.token != "tok-0" {
		t.Errorf("entry token = %q, want tok-0 (unchanged reload must not rebuild)", cur.token)
	}
}

// TestQuarantineClearedByUnlockToken pins the operator override: the
// dashboard unlock (explicit action after appealing the account upstream)
// clears a terminal quarantine so the token is acquirable again.
func TestQuarantineClearedByUnlockToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true
	p := newTestPool(t, mock)

	_, err := p.Acquire(context.Background(), modelA)
	if !errors.Is(err, upstream.ErrBanned) {
		t.Fatalf("first acquire: want ErrBanned, got %v", err)
	}
	if !p.Snapshot()[0].Quarantined {
		t.Fatal("token not quarantined after a ban")
	}

	if err := p.UnlockToken(0); err != nil {
		t.Fatalf("UnlockToken: %v", err)
	}
	if p.Snapshot()[0].Quarantined {
		t.Error("UnlockToken did not clear the quarantine")
	}
}
