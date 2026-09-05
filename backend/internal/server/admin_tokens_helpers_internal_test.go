package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseTokenIndex(t *testing.T) {
	newReq := func(contentType, body string) *http.Request {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "/admin/tokens/remove", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		return req
	}
	rec := httptest.NewRecorder()
	cases := []struct {
		name    string
		req     *http.Request
		wantIdx int
		wantOK  bool
	}{
		{"absent stays legacy last-token", newReq("", ""), -1, true},
		{"form index", newReq("application/x-www-form-urlencoded", url.Values{"token": []string{"1"}}.Encode()), 1, true},
		{"json number", newReq("application/json", `{"token":0}`), 0, true},
		{"json string", newReq("application/json", `{"token":"1"}`), 1, true},
		{"json index key", newReq("application/json", `{"index":1}`), 1, true},
		{"out of range", newReq("application/json", `{"token":5}`), -1, false},
		{"negative", newReq("application/x-www-form-urlencoded", url.Values{"token": []string{"-1"}}.Encode()), -1, false},
		{"garbage", newReq("application/json", `{"token":"abc"}`), -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotIdx, gotOK := parseTokenIndex(rec, tc.req, []string{"token", "index"}, 2)
			if gotIdx != tc.wantIdx || gotOK != tc.wantOK {
				t.Errorf("parseTokenIndex = (%d,%v), want (%d,%v)", gotIdx, gotOK, tc.wantIdx, tc.wantOK)
			}
		})
	}
}
func TestSpawnModelFromRequest(t *testing.T) {
	newReq := func(contentType, body string) *http.Request {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "/admin/tokens/0/session", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		return req
	}
	cases := []struct {
		name string
		req  *http.Request
		want string
	}{
		{"empty stays empty", newReq("", ""), ""},
		{"form model", newReq("application/x-www-form-urlencoded", url.Values{"model": []string{"mimo/mimo-v2.5"}}.Encode()), "mimo/mimo-v2.5"},
		{"json model", newReq("application/json", `{"model":"openai/gpt-5.6-luna"}`), "openai/gpt-5.6-luna"},
		{"json trims space", newReq("application/json", `{"model":"  z-ai/glm-5.3-flash  "}`), "z-ai/glm-5.3-flash"},
		{"json empty", newReq("application/json", `{"model":""}`), ""},
		{"garbage json", newReq("application/json", `not-json`), ""},
		{"wrong key", newReq("application/json", `{"foo":"x"}`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := spawnModelFromRequest(tc.req); got != tc.want {
				t.Errorf("spawnModelFromRequest = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRemoveAtCopyKeepsInput(t *testing.T) {
	src := []string{"a", "b", "c"}
	got := removeAtCopy(src, 0)
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("removeAtCopy = %q, want [b c]", got)
	}
	if len(src) != 3 || src[0] != "a" || src[1] != "b" {
		t.Errorf("input mutated to %q, want untouched [a b c]", src)
	}
}
