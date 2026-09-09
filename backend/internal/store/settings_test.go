package store

import "testing"

func TestSettingsRoundTrip(t *testing.T) {
	s := openTest(t)
	if _, ok, err := s.GetSetting("theme"); err != nil {
		t.Fatalf("GetSetting missing: %v", err)
	} else if ok {
		t.Fatal("GetSetting on empty store returned ok=true")
	}
	if err := s.SetSetting("theme", `{"mode":"dark"}`); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	v, ok, err := s.GetSetting("theme")
	if err != nil || !ok || v != `{"mode":"dark"}` {
		t.Fatalf("GetSetting = %q,%v,%v, want dark,true,nil", v, ok, err)
	}
	if err := s.SetSetting("theme", `{"mode":"light"}`); err != nil {
		t.Fatalf("SetSetting overwrite: %v", err)
	}
	if v, _, _ := s.GetSetting("theme"); v != `{"mode":"light"}` {
		t.Fatalf("overwrite kept %q, want light", v)
	}
}

func TestSettingsRejectsEmptyKey(t *testing.T) {
	s := openTest(t)
	if err := s.SetSetting("", "x"); err == nil {
		t.Fatal("SetSetting(\"\") accepted, want an error")
		return
	}
	if _, _, err := s.GetSetting(""); err == nil {
		t.Fatal("GetSetting(\"\") accepted, want an error")
		return
	}
}

func TestSettingsDeleteAndList(t *testing.T) {
	s := openTest(t)
	if err := s.SetSetting("config:SAFE_MODE", "false"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := s.SetSetting("theme", `{"mode":"dark"}`); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	all, err := s.ListSettings()
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	if len(all) != 2 || all["config:SAFE_MODE"] != "false" {
		t.Fatalf("ListSettings = %v, want 2 rows with the overlay", all)
	}
	if err := s.DeleteSetting("config:SAFE_MODE"); err != nil {
		t.Fatalf("DeleteSetting: %v", err)
	}
	if _, ok, _ := s.GetSetting("config:SAFE_MODE"); ok {
		t.Fatal("GetSetting after delete returned ok=true")
	}
	// Missing-row delete is a no-op, never an error.
	if err := s.DeleteSetting("config:SAFE_MODE"); err != nil {
		t.Fatalf("DeleteSetting missing: %v", err)
	}
	if err := s.DeleteSetting(""); err == nil {
		t.Fatal("DeleteSetting(\"\") accepted, want an error")
		return
	}
}
