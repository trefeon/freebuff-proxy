package store

import "testing"

func TestTokenMetaUpsertPreservesCreatedAt(t *testing.T) {
	s := openTest(t)
	if _, ok, err := s.GetTokenMeta("deadbeef"); err != nil {
		t.Fatalf("GetTokenMeta missing: %v", err)
	} else if ok {
		t.Fatal("GetTokenMeta on empty store returned ok=true")
	}
	if err := s.UpsertTokenMeta("deadbeef", "label-a", "active", `{"limit":100}`); err != nil {
		t.Fatalf("UpsertTokenMeta: %v", err)
	}
	first, ok, err := s.GetTokenMeta("deadbeef")
	if err != nil || !ok {
		t.Fatalf("GetTokenMeta = %+v,%v,%v, want row", first, ok, err)
	}
	if first.Label != "label-a" || first.Status != "active" || first.QuotaData != `{"limit":100}` {
		t.Fatalf("GetTokenMeta = %+v, want label-a/active/quota", first)
	}
	if first.CreatedAt <= 0 {
		t.Fatalf("CreatedAt = %d, want a positive millis stamp", first.CreatedAt)
	}
	if err := s.UpsertTokenMeta("deadbeef", "label-b", "banned", `{"limit":0}`); err != nil {
		t.Fatalf("UpsertTokenMeta overwrite: %v", err)
	}
	second, _, _ := s.GetTokenMeta("deadbeef")
	if second.Label != "label-b" || second.Status != "banned" {
		t.Fatalf("upsert kept %+v, want label-b/banned", second)
	}
	if second.CreatedAt != first.CreatedAt {
		t.Fatalf("CreatedAt moved %d -> %d, want it preserved", first.CreatedAt, second.CreatedAt)
	}
}

func TestTokenMetaListAndDelete(t *testing.T) {
	s := openTest(t)
	if err := s.UpsertTokenMeta("h1", "a", "active", ""); err != nil {
		t.Fatalf("upsert h1: %v", err)
	}
	if err := s.UpsertTokenMeta("h2", "b", "active", ""); err != nil {
		t.Fatalf("upsert h2: %v", err)
	}
	got, err := s.ListTokenMetas()
	if err != nil {
		t.Fatalf("ListTokenMetas: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListTokenMetas = %d rows, want 2", len(got))
	}
	if err := s.DeleteTokenMeta("h1"); err != nil {
		t.Fatalf("DeleteTokenMeta: %v", err)
	}
	if _, ok, _ := s.GetTokenMeta("h1"); ok {
		t.Fatal("deleted token still returned")
	}
	if got, _ := s.ListTokenMetas(); len(got) != 1 {
		t.Fatalf("ListTokenMetas after delete = %d rows, want 1", len(got))
	}
}

func TestTokenMetaRejectsEmptyHash(t *testing.T) {
	s := openTest(t)
	if err := s.UpsertTokenMeta("", "a", "active", ""); err == nil {
		t.Fatal("UpsertTokenMeta(\"\") accepted, want an error")
	}
	if _, _, err := s.GetTokenMeta(""); err == nil {
		t.Fatal("GetTokenMeta(\"\") accepted, want an error")
	}
}
