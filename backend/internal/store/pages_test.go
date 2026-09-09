package store

import "testing"

func TestPageStateRoundTrip(t *testing.T) {
	s := openTest(t)
	if _, ok, err := s.GetPageState("overview"); err != nil {
		t.Fatalf("GetPageState missing: %v", err)
	} else if ok {
		t.Fatal("GetPageState on empty store returned ok=true")
	}
	if err := s.PutPageState("overview", `{"cards":["a"]}`); err != nil {
		t.Fatalf("PutPageState: %v", err)
	}
	v, ok, err := s.GetPageState("overview")
	if err != nil || !ok || v != `{"cards":["a"]}` {
		t.Fatalf("GetPageState = %q,%v,%v, want cards,true,nil", v, ok, err)
	}
	if err := s.PutPageState("overview", `{"cards":["b"]}`); err != nil {
		t.Fatalf("PutPageState overwrite: %v", err)
	}
	if v, _, _ := s.GetPageState("overview"); v != `{"cards":["b"]}` {
		t.Fatalf("overwrite kept %q, want b", v)
	}
}

func TestPageStateRejectsEmptyID(t *testing.T) {
	s := openTest(t)
	if err := s.PutPageState("", "x"); err == nil {
		t.Fatal("PutPageState(\"\") accepted, want an error")
		return
	}
	if _, _, err := s.GetPageState(""); err == nil {
		t.Fatal("GetPageState(\"\") accepted, want an error")
		return
	}
}
