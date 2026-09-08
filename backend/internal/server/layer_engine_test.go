package server

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestEngineLayerStaysBelowSurface pins the layering seam: the engine layer
// (engine*.go plus the shared streaming pipeline) must never name HTTP
// surface symbols (admin handlers, dashboard rendering, route wiring). The
// surface calls into the engine, never the reverse. Comments and string
// literals are stripped before matching so prose cannot trip the guard.
func TestEngineLayerStaysBelowSurface(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(self)
	files := []string{
		"engine.go",
		"engine_attempt.go",
		"engine_helpers.go",
		"engine_sse.go",
		"stream_shared.go",
	}
	surface := regexp.MustCompile(`\b(adminHandlers|registerAdminRoutes|handleToken[A-Za-z]*|handleModeSwitch|handleBridge[A-Za-z]*|RenderConfigResult|RenderTestResults)\b|dashboard\.`)
	for _, name := range files {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		code := stripCommentsAndStrings(string(raw))
		for i, line := range strings.Split(code, "\n") {
			if m := surface.FindString(line); m != "" {
				t.Errorf("%s:%d: engine layer names surface symbol %q", name, i+1, m)
			}
		}
	}
}

// stripCommentsAndStrings removes /* */ blocks, // tails, and quoted
// literals so the guard only sees code. Strings go first: a "//" inside a
// URL literal must not start a comment.
func stripCommentsAndStrings(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		switch {
		case s[i] == '"' || s[i] == '\'' || s[i] == '`':
			q := s[i]
			i++
			for i < len(s) {
				if q == '`' {
					if s[i] == '`' {
						i++
						break
					}
					i++
					continue
				}
				if s[i] == '\\' {
					i += 2
					continue
				}
				i++
				if s[i-1] == q {
					break
				}
			}
			b.WriteByte(' ')
		case i+1 < len(s) && s[i] == '/' && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				i = len(s)
			} else {
				i += end + 4
			}
			b.WriteByte(' ')
		case i+1 < len(s) && s[i] == '/' && s[i+1] == '/':
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				i = len(s)
			} else {
				i += end
			}
			b.WriteByte(' ')
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}
