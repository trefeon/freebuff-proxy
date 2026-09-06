package pool

import (
	"sync"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// recordingSink captures maturity history events for assertions.
type recordingSink struct {
	mu  sync.Mutex
	got []MaturityHistoryEvent
}

func (s *recordingSink) RecordMaturity(e MaturityHistoryEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, e)
}

func (s *recordingSink) kinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.got))
	for _, e := range s.got {
		out = append(out, e.Kind)
	}
	return out
}

// Config changes emit history; a cleared sink stays silent.
func TestHistorySinkConfigEvents(t *testing.T) {
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	p := newMaturityPool(t, mock, true)
	sink := &recordingSink{}
	p.SetHistorySink(sink)

	if err := p.SetMaturity(0, true, 7, MaturityModeUnmetered); err != nil {
		t.Fatalf("SetMaturity: %v", err)
	}
	if err := p.SetMaturity(0, false, 0, ""); err != nil {
		t.Fatalf("SetMaturity disable: %v", err)
	}
	kinds := sink.kinds()
	if len(kinds) != 2 || kinds[0] != "config" || kinds[1] != "config" {
		t.Fatalf("kinds = %v, want [config config]", kinds)
	}
	if sink.got[0].TokenIdx != 0 || sink.got[0].Detail != "enabled target=7 mode=unmetered" {
		t.Errorf("enable event = %+v", sink.got[0])
	}
	if sink.got[1].Detail != "disabled" {
		t.Errorf("disable event = %+v", sink.got[1])
	}

	p.SetHistorySink(nil)
	if err := p.SetMaturity(0, true, 7, MaturityModeUnmetered); err != nil {
		t.Fatalf("SetMaturity: %v", err)
	}
	if len(sink.kinds()) != 2 {
		t.Fatalf("kinds after clear = %v, want no new events", sink.kinds())
	}
}
