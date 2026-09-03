package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/runs"
	"freebuff-proxy/backend/internal/upstream"
)

// fakeAttemptBackend adapts the chatBackend interface for tests: nil hooks
// are no-ops, and the recorded hooks mirror the closure table chatAttempt
// used to take (issue #255).
type fakeAttemptBackend struct {
	acquire         func(ctx context.Context, model string) (*pool.Lease, error)
	chat            func(ctx context.Context, l *pool.Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error)
	invalidate      func(l *pool.Lease)
	supersede       func(l *pool.Lease)
	invalidateRun   func(l *pool.Lease, agentID string)
	cooldownAuth    func(l *pool.Lease)
	cooldownBan     func(l *pool.Lease, be *upstream.BanError)
	cooldownRate    func(l *pool.Lease, rle *upstream.RateLimitError)
	cooldownIP      func(l *pool.Lease, ice *upstream.IpCappedError)
	cooldownCountry func(l *pool.Lease, cbe *upstream.CountryBlockedError)
}

func (b *fakeAttemptBackend) Acquire(ctx context.Context, model string) (*pool.Lease, error) {
	if b.acquire == nil {
		return nil, errors.New("no acquire")
	}
	return b.acquire(ctx, model)
}
func (b *fakeAttemptBackend) Chat(ctx context.Context, l *pool.Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error) {
	return b.chat(ctx, l, opts, body)
}
func (b *fakeAttemptBackend) InvalidateSession(l *pool.Lease)           { b.invalidate(l) }
func (b *fakeAttemptBackend) InvalidateSessionSuperseded(l *pool.Lease) { b.supersede(l) }
func (b *fakeAttemptBackend) InvalidateRun(l *pool.Lease, agentID string) {
	b.invalidateRun(l, agentID)
}
func (b *fakeAttemptBackend) CooldownAuth(l *pool.Lease)                       { b.cooldownAuth(l) }
func (b *fakeAttemptBackend) CooldownBan(l *pool.Lease, be *upstream.BanError) { b.cooldownBan(l, be) }
func (b *fakeAttemptBackend) CooldownRateLimit(l *pool.Lease, rle *upstream.RateLimitError) {
	b.cooldownRate(l, rle)
}
func (b *fakeAttemptBackend) CooldownIpCapped(l *pool.Lease, ice *upstream.IpCappedError) {
	b.cooldownIP(l, ice)
}
func (b *fakeAttemptBackend) CooldownCountry(l *pool.Lease, cbe *upstream.CountryBlockedError) {
	b.cooldownCountry(l, cbe)
}
func (b *fakeAttemptBackend) LeaseRelease(*pool.Lease)          {}
func (b *fakeAttemptBackend) LeaseAbandon(*pool.Lease)          {}
func (b *fakeAttemptBackend) MarkRunFailed(*pool.Lease)         {}
func (b *fakeAttemptBackend) RecordRunStep(*pool.Lease, string) {}
func (b *fakeAttemptBackend) RecordSpend(*pool.Lease, int64)    {}

// runChatAttemptRetry drives chatAttempt through one failed chat + the
// retry-once re-acquire, recording every upstream envelope and which
// invalidation/cooldown hooks fired (issue #255 backend interface). The
// synthetic leases carry no pool backing, so the no-op lease methods are
// fine, and zero AcquiredAt keeps the success path off
// ClearModelUnfitBefore.
func runChatAttemptRetry(t *testing.T, firstLease, retryLease *pool.Lease, failFirst error) (captured []upstream.ChatOptions, invalidated []string) {
	t.Helper()
	acquires := 0
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	backend := &fakeAttemptBackend{
		acquire: func(ctx context.Context, model string) (*pool.Lease, error) {
			acquires++
			if acquires == 1 {
				return firstLease, nil
			}
			return retryLease, nil
		},
		chat: func(ctx context.Context, l *pool.Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error) {
			captured = append(captured, opts)
			if len(captured) == 1 {
				return nil, failFirst
			}
			return io.NopCloser(strings.NewReader("ok")), nil
		},
		invalidate:      func(l *pool.Lease) { t.Error("invalidateSession must not fire") },
		supersede:       func(l *pool.Lease) { t.Error("invalidateSessionSuperseded must not fire") },
		invalidateRun:   func(l *pool.Lease, agentID string) { invalidated = append(invalidated, agentID) },
		cooldownAuth:    func(l *pool.Lease) { t.Error("cooldownAuth must not fire") },
		cooldownBan:     func(l *pool.Lease, be *upstream.BanError) { t.Error("cooldownBan must not fire") },
		cooldownRate:    func(l *pool.Lease, rle *upstream.RateLimitError) { t.Error("cooldownRate must not fire") },
		cooldownIP:      func(l *pool.Lease, ice *upstream.IpCappedError) { t.Error("cooldownIpCapped must not fire") },
		cooldownCountry: func(l *pool.Lease, cbe *upstream.CountryBlockedError) { t.Error("cooldownCountry must not fire") },
	}
	up, _, err := s.chatAttempt(context.Background(), "deepseek/deepseek-v4-flash", []byte(`{}`), &chatTraceState{reqID: "req-rf"}, backend)
	if err != nil {
		t.Fatalf("chatAttempt returned error: %v", err)
	}
	if up == nil {
		t.Fatal("chatAttempt returned nil body on success")
	}
	_ = up.Close()
	if acquires != 2 {
		t.Errorf("acquires = %d, want 2 (one retry)", acquires)
	}
	if len(captured) != 2 {
		t.Fatalf("chat attempts captured = %d, want 2", len(captured))
	}
	return captured, invalidated
}

// TestChatAttemptRetryCarriesFreshRunIdentity pins the review-2026-08-31
// P3 fix: after a run-invalid retry re-acquires a lease on a NEW run, the
// upstream envelope carries the fresh run's TraceSessionID/ClientID/AgentID
// and its step counter — not the dead run's identity.
func TestChatAttemptRetryCarriesFreshRunIdentity(t *testing.T) {
	// Runs are constructed per subtest: StepCount persists on a shared
	// *runs.Run, and the identity assertions must not depend on test order.
	t.Run("fresh run refreshes trace identity", func(t *testing.T) {
		runA := &runs.Run{RunID: "run-1", TraceSessionID: "trace-1", ClientID: "client-1", AgentID: "agent-1"}
		runB := &runs.Run{RunID: "run-2", TraceSessionID: "trace-2", ClientID: "client-2", AgentID: "agent-2"}
		captured, invalidated := runChatAttemptRetry(t,
			&pool.Lease{Model: "deepseek/deepseek-v4-flash", AgentID: "agent-1", Run: runA, SessionInstanceID: "inst-1"},
			&pool.Lease{Model: "deepseek/deepseek-v4-flash", AgentID: "agent-2", Run: runB, SessionInstanceID: "inst-2"},
			upstream.ErrRunInvalid)

		if got := invalidated; len(got) != 1 || got[0] != "agent-1" {
			t.Errorf("invalidateRun calls = %v, want [agent-1]", got)
		}
		want := upstream.ChatOptions{
			Model:             "deepseek/deepseek-v4-flash",
			RunID:             "run-2",
			SessionInstanceID: "inst-2",
			TraceSessionID:    "trace-2",
			ClientID:          "client-2",
			AgentID:           "agent-2",
			RequestID:         "req-rf",
			StepNumber:        1, // the fresh run's first step
		}
		if !reflect.DeepEqual(captured[1], want) {
			t.Errorf("retry envelope = %+v, want %+v (fresh run identity)", captured[1], want)
		}
	})

	t.Run("same run keeps trace identity and step", func(t *testing.T) {
		// Same RunID on the retry lease (transient-error path): the
		// identity and the step number must stay untouched — the retry
		// repeats the SAME step of the SAME run.
		runA := &runs.Run{RunID: "run-1", TraceSessionID: "trace-1", ClientID: "client-1", AgentID: "agent-1"}
		runA2 := &runs.Run{RunID: "run-1", TraceSessionID: "trace-1", ClientID: "client-1", AgentID: "agent-1"}
		captured, invalidated := runChatAttemptRetry(t,
			&pool.Lease{Model: "deepseek/deepseek-v4-flash", AgentID: "agent-1", Run: runA, SessionInstanceID: "inst-1"},
			&pool.Lease{Model: "deepseek/deepseek-v4-flash", AgentID: "agent-1", Run: runA2, SessionInstanceID: "inst-1"},
			errors.New("transient transport failure"))

		if got := invalidated; len(got) != 0 {
			t.Errorf("invalidateRun calls = %v, want none on a transient error", got)
		}
		if !reflect.DeepEqual(captured[1], captured[0]) {
			t.Errorf("same-run retry envelope changed: %+v -> %+v (want identical)", captured[0], captured[1])
		}
	})
}

func TestChatAttemptRateLimitFailover(t *testing.T) {
	t.Run("rate limit fails over to next token when enabled", func(t *testing.T) {
		runA := &runs.Run{RunID: "run-1", TraceSessionID: "trace-1", ClientID: "client-1", AgentID: "agent-1"}
		runB := &runs.Run{RunID: "run-2", TraceSessionID: "trace-2", ClientID: "client-2", AgentID: "agent-2"}
		firstLease := &pool.Lease{Token: 0, Model: "deepseek/deepseek-v4-flash", AgentID: "agent-1", Run: runA, SessionInstanceID: "inst-1"}
		retryLease := &pool.Lease{Token: 1, Model: "deepseek/deepseek-v4-flash", AgentID: "agent-2", Run: runB, SessionInstanceID: "inst-2"}

		rle := &upstream.RateLimitError{Model: "deepseek/deepseek-v4-flash"}
		cooldownRateCalled := false

		acquires := 0
		s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		s.cfg.Store(&config.Config{RateLimitFailover: true})
		backend := &fakeAttemptBackend{
			acquire: func(ctx context.Context, model string) (*pool.Lease, error) {
				acquires++
				if acquires == 1 {
					return firstLease, nil
				}
				return retryLease, nil
			},
			chat: func(ctx context.Context, l *pool.Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error) {
				if l.Token == 0 {
					return nil, rle
				}
				return io.NopCloser(strings.NewReader("ok from token 1")), nil
			},
			cooldownRate: func(l *pool.Lease, err *upstream.RateLimitError) {
				cooldownRateCalled = true
			},
		}

		up, finalLease, err := s.chatAttempt(context.Background(), "deepseek/deepseek-v4-flash", []byte(`{}`), &chatTraceState{reqID: "req-rl"}, backend)
		if err != nil {
			t.Fatalf("chatAttempt failed: %v", err)
		}
		if finalLease.Token != 1 {
			t.Errorf("finalLease token = %d, want 1", finalLease.Token)
		}
		if !cooldownRateCalled {
			t.Error("cooldownRate was not called on failed token")
		}
		_ = up.Close()
	})

	t.Run("rate limit returns immediately when failover disabled", func(t *testing.T) {
		runA := &runs.Run{RunID: "run-1", TraceSessionID: "trace-1", ClientID: "client-1", AgentID: "agent-1"}
		firstLease := &pool.Lease{Token: 0, Model: "deepseek/deepseek-v4-flash", AgentID: "agent-1", Run: runA, SessionInstanceID: "inst-1"}

		rle := &upstream.RateLimitError{Model: "deepseek/deepseek-v4-flash"}
		acquires := 0
		s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		s.cfg.Store(&config.Config{RateLimitFailover: false})
		backend := &fakeAttemptBackend{
			acquire: func(ctx context.Context, model string) (*pool.Lease, error) {
				acquires++
				return firstLease, nil
			},
			chat: func(ctx context.Context, l *pool.Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error) {
				return nil, rle
			},
			cooldownRate: func(l *pool.Lease, err *upstream.RateLimitError) {},
		}

		_, _, err := s.chatAttempt(context.Background(), "deepseek/deepseek-v4-flash", []byte(`{}`), &chatTraceState{reqID: "req-rl2"}, backend)
		if !errors.Is(err, rle) {
			t.Fatalf("expected rle error, got: %v", err)
		}
		if acquires != 1 {
			t.Errorf("acquires = %d, want 1 (no retry)", acquires)
		}
	})
}
