package pool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// touchRunSatisfied is the run check: upstream advances streaks on agent-run
// message rows, not bare admission, so a touch counts only when it STARTed a
// run, sent at least one chat turn, and FINISHed the run. Probe-only and
// admit-only touches leave all three counters at zero.
func touchRunSatisfied(mock *testutil.MockUpstream) bool {
	return len(mock.StartedRunsSnapshot()) > 0 &&
		len(mock.RecordedChatBodiesSnapshot()) > 0 &&
		len(mock.FinishedRunsSnapshot()) > 0
}

// A live touch runs admit → one minimal turn → release: the stub sees the
// session create, the run START, the bound chat turn, the FINISH, and the
// session DELETE, while the Pacific-day activity ledger stays empty (the
// turn rides the upstream client directly, never Pool.Chat).
func TestMaturityLiveTouchAdmitsTurnReleases(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, false)
	now := windowNow()
	seedStreak(p, 0, 2, false, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))

	p.maturityTickAt(context.Background(), now)

	if !touchRunSatisfied(mock) {
		t.Fatalf("run check unsatisfied: starts=%d chats=%d finishes=%d, want all >=1 (admit-turn-release)",
			len(mock.StartedRunsSnapshot()), len(mock.RecordedChatBodiesSnapshot()), len(mock.FinishedRunsSnapshot()))
	}
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("SessionCreates = %d, want 1 (one admit)", got)
	}
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d, want 0 (live touch never probes)", got)
	}
	starts := mock.StartRequestsSnapshot()
	if len(starts) != 1 || starts[0].AgentID == "" {
		t.Errorf("START requests = %+v, want one with a resolved agent id", starts)
	}
	if len(starts) == 1 && len(starts[0].AncestorRunIDs) != 0 {
		t.Errorf("START ancestorRunIds = %v, want empty (fresh run)", starts[0].AncestorRunIDs)
	}
	// The turn binds the admitted instance and the fresh run, with effort
	// none (no reasoning_effort key at either level: the silent-turn
	// contract is key-absent).
	turn := mock.LastChatBody()
	var payload struct {
		Metadata map[string]any `json:"codebuff_metadata"`
	}
	if err := json.Unmarshal([]byte(turn), &payload); err != nil {
		t.Fatalf("turn body is not JSON: %v", err)
	}
	if got, _ := payload.Metadata["freebuff_instance_id"].(string); got != mock.InstanceID {
		t.Errorf("turn freebuff_instance_id = %q, want %q (admitted instance)", got, mock.InstanceID)
	}
	if got, _ := payload.Metadata["run_id"].(string); got != "run-0001" {
		t.Errorf("turn run_id = %q, want run-0001 (fresh run)", got)
	}
	if _, ok := payload.Metadata["freebuff_reasoning_effort"]; ok {
		t.Error("turn carries freebuff_reasoning_effort, want absent (effort none)")
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(turn), &raw); err != nil {
		t.Fatalf("turn body is not a JSON object: %v", err)
	}
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("turn carries top-level reasoning_effort, want absent (effort none)")
	}
	fin := mock.FinishedRunsSnapshot()
	if len(fin) != 1 || fin[0].RunID != "run-0001" || fin[0].Status != "completed" || fin[0].TotalSteps != 1 {
		t.Errorf("finished runs = %+v, want one completed run-0001 with totalSteps 1", fin)
	}
	if len(fin) == 1 && (len(fin[0].Steps) != 1 || fin[0].Steps[0].StepNumber != 1) {
		t.Errorf("finished steps = %+v, want the one completed turn step", fin[0].Steps)
	}
	if got := mock.SessionEndsSnapshot(); got < 1 {
		t.Errorf("SessionEnds = %d, want >=1 (touch releases its session)", got)
	}
	action, result := maturityResult(p, 0)
	if action != "admit" || result != "ok" {
		t.Errorf("last touch = %q/%q, want admit/ok", action, result)
	}
	// The touch rides the upstream client directly, never Pool.Chat: the
	// Pacific-day activity ledger stays empty so automation never self-skips
	// via skip:client-active.
	if got := p.dayRequestCount(0); got != 0 {
		t.Errorf("dayRequestCount = %d, want 0 (touch never feeds the activity ledger)", got)
	}
}

// A dry-run touch probes only: no run starts, no turn, no finish — the run
// check stays unsatisfied and the activity ledger stays empty.
func TestMaturityProbeTouchSatisfiesNoRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, true)
	now := windowNow()
	seedStreak(p, 0, 2, false, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))

	p.maturityTickAt(context.Background(), now)

	if touchRunSatisfied(mock) {
		t.Error("probe touch satisfies the run check, want no run without admit-turn-release")
	}
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes = %d, want 1 (dry-run probe)", got)
	}
	if got := p.dayRequestCount(0); got != 0 {
		t.Errorf("dayRequestCount = %d, want 0 (probe never feeds the activity ledger)", got)
	}
	action, result := maturityResult(p, 0)
	if action != "probe" || result != "ok" {
		t.Errorf("last touch = %q/%q, want probe/ok", action, result)
	}
}

// Bare admission leaves no agent-run row: admitting without a turn does not
// satisfy the run check. This pins the premise the triple is built on.
func TestMaturityBareAdmissionSatisfiesNoRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, false)

	toks := p.roster.Load()
	if _, err := (*toks)[0].session.EnsureSessionForModel(context.Background(), modelB); err != nil {
		t.Fatalf("admit: %v", err)
	}

	if touchRunSatisfied(mock) {
		t.Error("bare admission satisfies the run check, want no run without a turn")
	}
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("SessionCreates = %d, want 1 (admitted, nothing more)", got)
	}
}

// A touch whose release receipt reports a pending refund replays the DELETE
// with the same instance until it settles, through the session manager's
// real refund handler: two DELETEs, pending cleared, lastRefund recorded.
func TestMaturityTouchReplaysPendingRefund(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, false)
	now := windowNow()
	seedStreak(p, 0, 2, false, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))

	deletes := 0
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
			w.Header().Set("Content-Type", "application/json")
			if deletes == 1 {
				_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefundPending":true}`)
			} else {
				_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefund":1.5}`)
			}
			return
		}
		// Minimal active-session shape (mirrors the mock's default create):
		// the touch only needs an admitted instance to bind its turn to.
		expiresAt := time.Now().Add(30 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","expiresAt":"`+expiresAt+`"}`)
	}

	p.maturityTickAt(context.Background(), now)

	if !touchRunSatisfied(mock) {
		t.Fatal("run check unsatisfied on the refund-replay touch, want admit-turn-release first")
	}
	if deletes != 2 {
		t.Errorf("deletes = %d, want 2 (release + one pending-refund replay)", deletes)
	}
	toks := p.roster.Load()
	snap := (*toks)[0].sessionMgr().Snapshot()
	if snap.PendingRefund != "" {
		t.Errorf("PendingRefund = %q, want cleared after settle", snap.PendingRefund)
	}
	if snap.LastRefund == nil || *snap.LastRefund != 1.5 {
		t.Errorf("LastRefund = %+v, want 1.5 (settled replay receipt)", snap.LastRefund)
	}
	action, result := maturityResult(p, 0)
	if action != "admit" || result != "ok" {
		t.Errorf("last touch = %q/%q, want admit/ok (replay is warn-only)", action, result)
	}
	if got := p.dayRequestCount(0); got != 0 {
		t.Errorf("dayRequestCount = %d, want 0 (touch never feeds the activity ledger)", got)
	}
}
