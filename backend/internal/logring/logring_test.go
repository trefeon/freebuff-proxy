package logring

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"
	"unicode"

	"freebuff-proxy/backend/internal/telemetry"
)

// discarding is a sink that accepts everything and keeps nothing.
type discarding struct{}

func (discarding) Enabled(context.Context, slog.Level) bool  { return true }
func (discarding) Handle(context.Context, slog.Record) error { return nil }
func (d discarding) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discarding) WithGroup(string) slog.Handler           { return d }

func TestRingRetainsNewestFirst(t *testing.T) {
	h := NewHandler(discarding{}, 3)
	logger := slog.New(h)
	for i := range 5 {
		logger.Info("msg", "n", i)
	}
	recent := h.Recent(10)
	if len(recent) != 3 {
		t.Fatalf("Recent(10) = %d entries, want 3 (bounded capacity)", len(recent))
	}
	// Newest first: n=4, n=3, n=2.
	for i, want := range []string{"n=4", "n=3", "n=2"} {
		if recent[i].Message != "msg" {
			t.Fatalf("entry %d message = %q", i, recent[i].Message)
		}
		found := false
		for _, f := range recent[i].Fields {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("entry %d fields %v missing %q", i, recent[i].Fields, want)
		}
	}
}

func TestRingSubHandlersShareStore(t *testing.T) {
	h := NewHandler(discarding{}, 10)
	logger := slog.New(h)
	sub := logger.With("scope", "pool")
	sub.Info("from sub")
	recent := h.Recent(1)
	if len(recent) != 1 {
		t.Fatalf("Recent(1) = %d entries, want 1", len(recent))
	}
	if recent[0].Message != "from sub" {
		t.Errorf("message = %q, want %q", recent[0].Message, "from sub")
	}
	found := false
	for _, f := range recent[0].Fields {
		if f == "scope=pool" {
			found = true
		}
	}
	if !found {
		t.Errorf("bound attr not retained: %v", recent[0].Fields)
	}
}

func TestRingForwardsToNext(t *testing.T) {
	var got string
	next := slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		got += string(p)
		return len(p), nil
	}), nil)
	h := NewHandler(next, 5)
	slog.New(h).Info("hello")
	if got == "" {
		t.Error("record was not forwarded to the wrapped handler")
	}
}

// TestRingFlattenGroupKeys verifies grouped attrs keep the group key as a
// dotted prefix: slog.Group("http", slog.Int("status", 200)) must render as
// "http.status=200", not "status.status=200" (the child key must not replace
// the group key it extends).
func TestRingFlattenGroupKeys(t *testing.T) {
	h := NewHandler(discarding{}, 10)
	slog.New(h).Info("msg",
		slog.Group("http", slog.Int("status", 200)),
		slog.Group("nested", slog.Group("deep", slog.String("k", "v"))),
	)
	recent := h.Recent(1)
	if len(recent) != 1 {
		t.Fatalf("Recent(1) = %d entries, want 1", len(recent))
	}
	found := map[string]bool{}
	for _, f := range recent[0].Fields {
		found[f] = true
	}
	for _, want := range []string{"http.status=200", "nested.deep.k=v"} {
		if !found[want] {
			t.Errorf("fields %v missing %q", recent[0].Fields, want)
		}
	}
	if found["status=200"] {
		t.Errorf("fields %v contain %q: group key was dropped", recent[0].Fields, "status=200")
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// levelGate is a sink that only accepts records at or above its level.
type levelGate struct{ level slog.Level }

func (g levelGate) Enabled(_ context.Context, l slog.Level) bool { return l >= g.level }
func (g levelGate) Handle(context.Context, slog.Record) error    { return nil }
func (g levelGate) WithAttrs([]slog.Attr) slog.Handler           { return g }
func (g levelGate) WithGroup(string) slog.Handler                { return g }

// errorSink is a sink that always returns an error from Handle.
type errorSink struct{}

func (errorSink) Enabled(context.Context, slog.Level) bool  { return true }
func (errorSink) Handle(context.Context, slog.Record) error { return errors.New("sink boom") }
func (e errorSink) WithAttrs([]slog.Attr) slog.Handler      { return e }
func (e errorSink) WithGroup(string) slog.Handler           { return e }

// TestWithAttrsWithGroupFold verifies combined WithGroup+WithAttrs: the
// group name becomes the dotted prefix on BOTH bound attrs and the
// record's own grouped attrs (svc.node=n1, svc.http.status=200), and no
// double-prefixed or unprefixed variants leak through.
func TestWithAttrsWithGroupFold(t *testing.T) {
	h := NewHandler(discarding{}, 10)
	logger := slog.New(h).WithGroup("svc").With("node", "n1")
	logger.Info("msg", slog.Group("http", slog.Int("status", 200)))
	recent := h.Recent(1)
	if len(recent) != 1 {
		t.Fatalf("Recent(1) = %d entries, want 1", len(recent))
	}
	found := map[string]bool{}
	for _, f := range recent[0].Fields {
		found[f] = true
	}
	for _, want := range []string{"svc.node=n1", "svc.http.status=200"} {
		if !found[want] {
			t.Errorf("fields %v missing %q", recent[0].Fields, want)
		}
	}
	for _, bad := range []string{"node=n1", "http.status=200", "svc.svc.node=n1"} {
		if found[bad] {
			t.Errorf("fields %v contain %q (group fold wrong)", recent[0].Fields, bad)
		}
	}
}

// TestEnabledGating verifies gating is forwarded to the wrapped handler: a
// record the sink rejects at Enabled is never retained in the ring.
func TestEnabledGating(t *testing.T) {
	h := NewHandler(levelGate{level: slog.LevelInfo}, 10)
	logger := slog.New(h)
	logger.Debug("hidden")
	logger.Info("shown")
	recent := h.Recent(10)
	if len(recent) != 1 {
		t.Fatalf("Recent(10) = %d entries, want 1 (debug gated out)", len(recent))
	}
	if recent[0].Message != "shown" {
		t.Errorf("retained message = %q, want shown", recent[0].Message)
	}
}

// TestCapacityClamp verifies NewHandler clamps a sub-1 capacity to 1
// instead of building a zero-length buffer (which would panic on push).
func TestCapacityClamp(t *testing.T) {
	h := NewHandler(discarding{}, 0)
	logger := slog.New(h)
	logger.Info("a")
	logger.Info("b")
	recent := h.Recent(10)
	if len(recent) != 1 {
		t.Fatalf("Recent(10) = %d entries, want 1 (clamped capacity)", len(recent))
	}
	if recent[0].Message != "b" {
		t.Errorf("retained message = %q, want b (newest)", recent[0].Message)
	}
}

// testLogValuer is a slog.LogValuer used to pin FormatAttrValue's default branch.
type testLogValuer struct{ s string }

func (v testLogValuer) LogValue() slog.Value { return slog.StringValue(v.s) }

// TestFormatAttrKinds pins the shared value renderer across slog kinds:
// strings raw, numerics/bools via strconv, durations and times via their
// String / RFC3339 forms, and the default branch (KindAny incl. LogValuer)
// via slog.Value.String() — which does NOT resolve the LogValuer.
func TestFormatAttrKinds(t *testing.T) {
	utc := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		v    slog.Value
		want string
	}{
		{"string", slog.StringValue("abc"), "abc"},
		{"bool", slog.BoolValue(true), "true"},
		{"int64", slog.Int64Value(-7), "-7"},
		{"uint64", slog.Uint64Value(42), "42"},
		{"float64", slog.Float64Value(1.5), "1.5"},
		{"duration", slog.DurationValue(1500 * time.Millisecond), "1.5s"},
		{"time", slog.TimeValue(utc), "2026-08-17T12:00:00Z"},
		{"logvaluer", slog.AnyValue(testLogValuer{s: "resolved"}), "{resolved}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := telemetry.FormatAttrValue(tc.v); got != tc.want {
				t.Errorf("FormatAttrValue(%v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

// TestFormatAttrQuotesControlChars pins the log-injection guard under the
// shared telemetry quoting policy (issue #284): a string value containing a
// control character, a space, or a quote (e.g. a %0A/%0D-decoded URL path)
// is strconv.Quote'd so the ring entry cannot forge additional log lines in
// /admin/logs, and the rendered output must never contain a raw control
// character. The telemetry policy is deliberately WIDER than the old logring
// one: spaces and quotes are escaped too, which is the safer direction.
func TestFormatAttrQuotesControlChars(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantQuot bool
	}{
		{"newline", "path\nforged", true},
		{"carriage return", "path\rforged", true},
		{"tab", "a\tb", true},
		{"nul", "a\x00b", true},
		{"space", "two words", true},
		{"quote", `a"b`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := telemetry.FormatAttrValue(slog.StringValue(tc.in))
			if tc.wantQuot {
				if got != strconv.Quote(tc.in) {
					t.Errorf("FormatAttrValue(%q) = %q, want %q", tc.in, got, strconv.Quote(tc.in))
				}
				for _, r := range got {
					if r == '\n' || r == '\r' || unicode.IsControl(r) {
						t.Errorf("FormatAttrValue(%q) = %q contains a raw control character", tc.in, got)
					}
				}
			} else if got != tc.in {
				t.Errorf("FormatAttrValue(%q) = %q, want the raw string", tc.in, got)
			}
		})
	}
}

// TestRecentNegativeGuard is the S9 regression: Recent(-1) (a negative
// count) must return empty instead of panicking in make with a negative
// capacity. Zero behaves the same; positive counts still work.
func TestRecentNegativeGuard(t *testing.T) {
	h := NewHandler(discarding{}, 3)
	slog.New(h).Info("msg")
	if got := h.Recent(-1); len(got) != 0 {
		t.Errorf("Recent(-1) = %d entries, want 0 (S9 panic regression)", len(got))
	}
	if got := h.Recent(0); len(got) != 0 {
		t.Errorf("Recent(0) = %d entries, want 0", len(got))
	}
	if got := h.Recent(1); len(got) != 1 || got[0].Message != "msg" {
		t.Errorf("Recent(1) = %+v, want the single retained entry", got)
	}
}

// TestHandleErrorStillRetains verifies the push-before-forward order: when
// the wrapped sink's Handle returns an error, the error propagates to the
// caller but the record is still retained in the ring.
func TestHandleErrorStillRetains(t *testing.T) {
	h := NewHandler(errorSink{}, 5)
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "doomed", 0)
	err := h.Handle(context.Background(), rec)
	if err == nil {
		t.Fatal("Handle returned nil, want the wrapped sink's error")
	}
	recent := h.Recent(1)
	if len(recent) != 1 || recent[0].Message != "doomed" {
		t.Errorf("ring entry missing after forward error: %+v", recent)
	}
}

// TestCounts verifies the T20 counters: every handled record tallies by
// "level|msg" with the level lowercased, clones (WithAttrs/WithGroup) share
// the same counts, and the snapshot returned by Counts is independent of the
// live ring (mutating it never skews later counts).
func TestCounts(t *testing.T) {
	h := NewHandler(discarding{}, 10)
	logger := slog.New(h)
	logger.Info("request handled", "path", "/healthz")
	logger.Info("request handled", "path", "/metrics")
	logger.Warn("pool exhausted")
	logger.Error("upstream failed")
	// Clones share the ring and therefore the counts.
	logger.With("scope", "pool").Info("request handled")
	logger.WithGroup("svc").Debug("trace line")

	counts := h.Counts()
	want := map[string]int64{
		"info|request handled":  3,
		"warn|pool exhausted":   1,
		"error|upstream failed": 1,
		"debug|trace line":      1,
	}
	if len(counts) != len(want) {
		t.Fatalf("Counts() has %d keys %v, want %d (%v)", len(counts), counts, len(want), want)
	}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("Counts()[%q] = %d, want %d", k, counts[k], v)
		}
	}

	// Snapshot independence: mutating the returned map must not affect the
	// live ring, and a later Handle bumps only the live counter.
	counts["info|request handled"] = 999
	logger.Info("request handled")
	if got := h.Counts()["info|request handled"]; got != 4 {
		t.Errorf("Counts() after mutation+Handle = %d, want 4", got)
	}
}

// TestCountsConcurrent drives Handle from many goroutines and verifies no
// count is lost (the race detector owns the memory-safety side; this pins
// the arithmetic).
func TestCountsConcurrent(t *testing.T) {
	h := NewHandler(discarding{}, 100)
	logger := slog.New(h)
	const goroutines = 16
	const per = 50
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				logger.Info("worker done")
				logger.Warn("retry")
			}
		}()
	}
	wg.Wait()
	counts := h.Counts()
	if got := counts["info|worker done"]; got != goroutines*per {
		t.Errorf("Counts()[info|worker done] = %d, want %d", got, goroutines*per)
	}
	if got := counts["warn|retry"]; got != goroutines*per {
		t.Errorf("Counts()[warn|retry] = %d, want %d", got, goroutines*per)
	}
}

// TestRingEmptyGroupInlined is the empty-key group regression: a group with
// an empty key must be inlined (slog contract: "If a group's key is empty,
// inline the group's Attrs"), so its children keep the current prefix with
// no extra separator — "svc.status=200", never "svc..status=200".
func TestRingEmptyGroupInlined(t *testing.T) {
	cases := []struct {
		name      string
		withGroup string
		group     slog.Attr
		want      string
	}{
		{
			name:      "empty group under named prefix",
			withGroup: "svc",
			group:     slog.Group("", slog.Int("status", 200)),
			want:      "svc.status=200",
		},
		{
			name:      "nested empty group",
			withGroup: "svc",
			group:     slog.Group("", slog.Group("inner", slog.Int("status", 200))),
			want:      "svc.inner.status=200",
		},
		{
			name:      "consecutive empty groups",
			withGroup: "svc",
			group:     slog.Group("", slog.Group("", slog.Int("status", 200))),
			want:      "svc.status=200",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(discarding{}, 10)
			logger := slog.New(h)
			if tc.withGroup != "" {
				logger = logger.WithGroup(tc.withGroup)
			}
			logger.Info("msg", tc.group)
			recent := h.Recent(1)
			if len(recent) != 1 {
				t.Fatalf("Recent(1) = %d entries, want 1", len(recent))
			}
			// One record with one attr: Fields must equal the expected
			// single field exactly, so any malformed "svc..status=200"
			// shape fails the equality implicitly.
			if got := recent[0].Fields; len(got) != 1 || got[0] != tc.want {
				t.Errorf("fields = %v, want [%s]", got, tc.want)
			}
		})
	}
}
