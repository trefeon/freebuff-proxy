package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// legacyV2TokensSchema is the v2 tokens table (before the maturity columns).
// A v2 file crafted from it must open cleanly and migrate: existing rows
// preserved, maturity_json + streak_blob added, version stamped v3.
const legacyV2TokensSchema = `
CREATE TABLE tokens(
  id INTEGER PRIMARY KEY,
  value_hash TEXT NOT NULL UNIQUE,
  label TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '',
  quota_data TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0
);
`

func TestOpenMigratesV2ToV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(legacyV2TokensSchema); err != nil {
		t.Fatalf("v2 schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO tokens(value_hash, label) VALUES('abc', 'kept')`); err != nil {
		t.Fatalf("v2 seed: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version=2`); err != nil {
		t.Fatalf("v2 stamp: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on v2 file: %v (want in-place migration, not rejection)", err)
	}
	defer func() { _ = s.Close() }()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	cols := map[string]bool{}
	rows, err := s.db.Query(`PRAGMA table_info(tokens)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt interface{}
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	if !cols["maturity_json"] || !cols["streak_blob"] {
		t.Fatalf("tokens columns = %v, want maturity_json + streak_blob", cols)
	}
	// The v2 row survives with empty maturity state.
	got, blob, ok, err := s.LoadTokenMaturity("abc")
	if err != nil {
		t.Fatalf("LoadTokenMaturity after migrate: %v", err)
	}
	if !ok {
		t.Fatal("v2 row lost in migration (want preserved with empty state)")
	}
	if got != "" || len(blob) != 0 {
		t.Errorf("migrated state = %q/%q, want empty", got, blob)
	}
}

// Maturity state round-trips per token hash; raw tokens never touch disk.
func TestTokenMaturityRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "mat.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, _, ok, err := s.LoadTokenMaturity("abc"); err != nil || ok {
		t.Fatalf("absent load = ok:%v err:%v, want ok:false err:nil", ok, err)
	}
	state := `{"enabled":true,"target":7,"mode":"unmetered","touch_model":"mimo/mimo-v2.5"}`
	streak := []byte(`{"streak":3,"todayUsed":false,"timeZone":"America/New_York"}`)
	if err := s.SaveTokenMaturity("abc", state, streak); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, gotBlob, ok, err := s.LoadTokenMaturity("abc")
	if err != nil || !ok {
		t.Fatalf("load = ok:%v err:%v, want row", ok, err)
	}
	if got != state || string(gotBlob) != string(streak) {
		t.Errorf("round-trip = %q/%q, want %q/%q", got, gotBlob, state, streak)
	}
	// Refresh replaces wholesale; empty hash rejects.
	if err := s.SaveTokenMaturity("abc", `{}`, nil); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got, _, _, _ := s.LoadTokenMaturity("abc"); got != `{}` {
		t.Errorf("refreshed state = %q, want {}", got)
	}
	if err := s.SaveTokenMaturity("", state, streak); err == nil {
		t.Error("empty hash accepted, want error")
	}
}
