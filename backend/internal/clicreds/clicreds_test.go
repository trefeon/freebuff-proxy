package clicreds_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/clicreds"
)

// setFakeHome points os.UserHomeDir at a temp dir for the test (Windows uses
// USERPROFILE; the CLI login paths use $HOME/.config/... on the wire, so both
// are set).
func setFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	return home
}

func writeCreds(t *testing.T, home, rel, body string) {
	t.Helper()
	dir := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverTokenManicode(t *testing.T) {
	home := setFakeHome(t)
	writeCreds(t, home, ".config/manicode", `{"default": {"authToken": "cb_manicode", "email": "dev@example.com"}}`)

	token, email, path, ok := clicreds.DiscoverToken()
	if !ok {
		t.Fatal("DiscoverToken = not found, want cb_manicode")
	}
	if token != "cb_manicode" {
		t.Errorf("token = %q, want cb_manicode", token)
	}
	if email != "dev@example.com" {
		t.Errorf("email = %q, want dev@example.com", email)
	}
	if want := filepath.Join(home, ".config", "manicode", "credentials.json"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestDiscoverTokenCodebuffFallback(t *testing.T) {
	home := setFakeHome(t)
	writeCreds(t, home, ".config/codebuff", `{"default": {"authToken": "cb_codebuff", "email": "dev@example.com"}}`)

	token, _, _, ok := clicreds.DiscoverToken()
	if !ok || token != "cb_codebuff" {
		t.Fatalf("DiscoverToken = (%q, %v), want cb_codebuff", token, ok)
	}
}

func TestDiscoverTokenPrefersManicode(t *testing.T) {
	home := setFakeHome(t)
	writeCreds(t, home, ".config/manicode", `{"default": {"authToken": "cb_manicode"}}`)
	writeCreds(t, home, ".config/codebuff", `{"default": {"authToken": "cb_codebuff"}}`)

	token, _, path, _ := clicreds.DiscoverToken()
	if token != "cb_manicode" {
		t.Errorf("token = %q, want cb_manicode (manicode wins)", token)
	}
	if !strings.Contains(path, "manicode") {
		t.Errorf("path = %q, want manicode", path)
	}
}

func TestDiscoverTokenEmpty(t *testing.T) {
	home := setFakeHome(t)
	_ = home
	if _, _, _, ok := clicreds.DiscoverToken(); ok {
		t.Fatal("DiscoverToken = found with no credentials file, want not found")
	}
}

func TestDiscoverTokenStripsBOM(t *testing.T) {
	home := setFakeHome(t)
	writeCreds(t, home, ".config/manicode", "\xef\xbb\xbf"+`{"default": {"authToken": "cb_bom"}}`)

	token, _, _, ok := clicreds.DiscoverToken()
	if !ok || token != "cb_bom" {
		t.Fatalf("DiscoverToken = (%q, %v), want cb_bom (BOM must not break parsing)", token, ok)
	}
}
