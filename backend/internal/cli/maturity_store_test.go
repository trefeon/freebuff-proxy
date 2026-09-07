package cli

import (
	"errors"
	"path/filepath"
	"testing"

	history "freebuff-proxy/backend/internal/store"
)

// The boot adapter round-trips through a real history store file keyed by
// token hash; a nil adapter (or nil inner store) is persistence-free.
func TestPoolMaturityStoreRoundTrip(t *testing.T) {
	st, err := history.Open(filepath.Join(t.TempDir(), "mat.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ad := &poolMaturityStore{st: st}
	state := `{"enabled":true,"target":7}`
	streak := []byte(`{"streak":3}`)
	if err := ad.SaveMaturity("abc", state, streak); err != nil {
		t.Fatalf("SaveMaturity: %v", err)
	}
	got, gotBlob, ok, err := ad.LoadMaturity("abc")
	if err != nil || !ok {
		t.Fatalf("LoadMaturity = ok:%v err:%v, want row", ok, err)
	}
	if got != state || string(gotBlob) != string(streak) {
		t.Errorf("round-trip = %q/%q, want %q/%q", got, gotBlob, state, streak)
	}
	if _, _, ok, err := ad.LoadMaturity("absent"); err != nil || ok {
		t.Errorf("absent load = ok:%v err:%v, want miss", ok, err)
	}
	// Nil adapter and nil inner store never touch the DB.
	var nilAd *poolMaturityStore
	if err := nilAd.SaveMaturity("abc", state, streak); err != nil {
		t.Errorf("nil adapter save = %v, want nil", err)
	}
	if _, _, ok, err := nilAd.LoadMaturity("abc"); err != nil || ok {
		t.Errorf("nil adapter load = ok:%v err:%v, want miss", ok, err)
	}
	empty := &poolMaturityStore{}
	if err := empty.SaveMaturity("abc", state, streak); err != nil {
		t.Errorf("nil store save = %v, want nil", err)
	}
	if _, _, ok, err := empty.LoadMaturity("abc"); err != nil || ok {
		t.Errorf("nil store load = ok:%v err:%v, want miss", ok, err)
	}
}

// fakeRestorePool fails RestoreMaturity on demand.
type fakeRestorePool struct {
	err    error
	called bool
}

func (f *fakeRestorePool) RestoreMaturity() error {
	f.called = true
	return f.err
}

// Restore is warn-only: store failure keeps the boot green, nil pool no-ops.
func TestRestoreMaturityFromStoreWarnOnly(t *testing.T) {
	p := &fakeRestorePool{}
	restoreMaturityFromStore(seedTestLogger(), p)
	if !p.called {
		t.Error("restore not attempted, want exactly one call")
	}
	failing := &fakeRestorePool{err: errors.New("db locked")}
	restoreMaturityFromStore(seedTestLogger(), failing) // must not panic
	restoreMaturityFromStore(seedTestLogger(), nil)     // must not panic
}
