package session

// Wave-3 store tests: run persistence (issue #40) — SaveRun/LoadRun/
// RemoveRun round-trip through the sessions_persist backend.

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRunPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	fb := newFakeSessionBackend()
	s := NewStoreWithBackend(path, fb)

	pr := PersistedRun{
		RunID:          "run-abc",
		AgentID:        "agent-x",
		TraceSessionID: "trace-1",
		StartedAt:      time.Now().Add(-time.Minute),
		Requests:       3,
	}
	s.SaveRun("tokhash", "agent-x", pr)
	if got := s.LoadRun("tokhash", "agent-x"); got == nil || got.RunID != "run-abc" || got.TraceSessionID != "trace-1" || got.Requests != 3 {
		t.Fatalf("LoadRun = %+v, want run-abc", got)
	}
	if got := s.LoadRun("tokhash", "other-agent"); got != nil {
		t.Fatalf("LoadRun(other agent) = %+v, want nil", got)
	}

	// A fresh store over the same backend (restart) sees the run.
	s2 := NewStoreWithBackend(path, fb)
	if got := s2.LoadRun("tokhash", "agent-x"); got == nil || got.RunID != "run-abc" {
		t.Fatalf("restart LoadRun = %+v, want run-abc", got)
	}

	s2.RemoveRun("tokhash", "agent-x")
	if got := s2.LoadRun("tokhash", "agent-x"); got != nil {
		t.Fatalf("LoadRun after RemoveRun = %+v, want nil", got)
	}
	// Removing the last agent drops the token's run map (no empty residue):
	// with no session blob either, the backend row is gone entirely.
	if _, _, found := fb.lookup(t, "tokhash"); found {
		t.Error("backend row survives last-agent RemoveRun with no session, want row deleted")
	}
	if cs := s2.Load("tokhash"); cs != nil {
		t.Errorf("Load after last-agent RemoveRun = %+v, want nil", cs)
	}
}

func TestStoreRunRejectsEmptyRunID(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "state.json"))
	s.SaveRun("tokhash", "agent-x", PersistedRun{RunID: "", AgentID: "agent-x"})
	if got := s.LoadRun("tokhash", "agent-x"); got != nil {
		t.Fatalf("empty-run-id persisted: %+v", got)
	}
}

func TestStoreRunAndSessionCoexist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	fb := newFakeSessionBackend()
	s := NewStoreWithBackend(path, fb)
	s.Save("tokhash", &cachedState{status: "active", instanceID: "inst-1", model: "m", expiresAt: time.Now().Add(time.Hour), gracePeriodEndsAt: time.Now().Add(2 * time.Hour)})
	s.SaveRun("tokhash", "agent-x", PersistedRun{RunID: "run-1", AgentID: "agent-x", TraceSessionID: "t", StartedAt: time.Now()})

	s2 := NewStoreWithBackend(path, fb)
	if cs := s2.Load("tokhash"); cs == nil || cs.instanceID != "inst-1" {
		t.Fatalf("session lost with runs in backend: %+v", cs)
	}
	if pr := s2.LoadRun("tokhash", "agent-x"); pr == nil || pr.RunID != "run-1" {
		t.Fatalf("run lost with session in backend: %+v", pr)
	}
}
