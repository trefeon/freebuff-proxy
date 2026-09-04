package server_test

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

func TestAdminTokensSwap(t *testing.T) {
	t.Chdir(t.TempDir())

	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" },
		testutil.NewMock(), testutil.NewMock(), testutil.NewMock())
	cookie := authedCookie(t, ts)

	// Swap token 0 and 1 (promote tok-1 to index 0)
	resp := postJSON(t, ts.URL, cookie, "/admin/tokens/swap", `{"from":0,"to":1}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("swap status = %d, want 200", resp.StatusCode)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, "swapped") {
		t.Errorf("swap response = %q, want mention of swapped", body)
	}

	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	if !strings.Contains(string(env), "AUTH_TOKENS=tok-1,tok-0,tok-2") {
		t.Errorf("swapped .env = %s, want tok-1 first", string(env))
	}

	// Directional swap: move token 2 up (swap 2 and 1)
	resp = postJSON(t, ts.URL, cookie, "/admin/tokens/swap", `{"index":2,"direction":"up"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("swap up status = %d, want 200", resp.StatusCode)
	}

	env, _ = os.ReadFile(".env")
	if !strings.Contains(string(env), "AUTH_TOKENS=tok-1,tok-2,tok-0") {
		t.Errorf("swapped .env = %s, want tok-2 at index 1", string(env))
	}

	// Move action: move token at index 2 to index 0: [tok-1, tok-2, tok-0] -> [tok-0, tok-1, tok-2]
	resp = postJSON(t, ts.URL, cookie, "/admin/tokens/swap", `{"from":2,"to":0,"action":"move"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("move status = %d, want 200", resp.StatusCode)
	}
	env, _ = os.ReadFile(".env")
	if !strings.Contains(string(env), "AUTH_TOKENS=tok-0,tok-1,tok-2") {
		t.Errorf("moved .env = %s, want tok-0 first", string(env))
	}

	// Out of bounds check
	resp = postJSON(t, ts.URL, cookie, "/admin/tokens/swap", `{"from":0,"to":10}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("out-of-bounds status = %d, want 400", resp.StatusCode)
	}
}
