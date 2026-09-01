package config_test

// Regression tests for the 2026-08-31 review P3 (config → telemetry layering
// inversion): config is a bottom-layer package and must not import telemetry
// for LOG_LEVEL validation. The level table lives in loglevel.go in this
// package; telemetry forwards to it.

import (
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/telemetry"
)

// TestParseLevelGrammar pins the LOG_LEVEL grammar at its canonical home.
// Pre-fix this API did not exist in config (validation called
// telemetry.ParseLevel from the bottom layer).
func TestParseLevelGrammar(t *testing.T) {
	cases := []struct {
		in     string
		want   slog.Level
		wantOK bool
	}{
		{"", 0, false},
		{"debug", slog.LevelDebug, true},
		{"INFO", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"ERROR", slog.LevelError, true},
		{"trace", config.LevelTrace, true},
		{"TRACE", config.LevelTrace, true},
		{"Trace", config.LevelTrace, true},
		{"bogus", 0, false},
	}
	for _, tc := range cases {
		lv, ok := config.ParseLevel(tc.in)
		if ok != tc.wantOK || lv != tc.want {
			t.Errorf("config.ParseLevel(%q) = (%v, %v), want (%v, %v)", tc.in, lv, ok, tc.want, tc.wantOK)
		}
	}
	if config.LevelTrace >= slog.LevelDebug {
		t.Errorf("config.LevelTrace = %v, want strictly below LevelDebug (%v)", config.LevelTrace, slog.LevelDebug)
	}
}

// TestTelemetryForwardsParseLevel pins the delegation: telemetry must not
// re-implement the mapping, so both entry points agree on every input class.
func TestTelemetryForwardsParseLevel(t *testing.T) {
	for _, s := range []string{"", "debug", "INFO", "warn", "error", "trace", "TRACE", "Trace", "bogus"} {
		wantLv, wantOK := config.ParseLevel(s)
		gotLv, gotOK := telemetry.ParseLevel(s)
		if gotOK != wantOK || gotLv != wantLv {
			t.Errorf("telemetry.ParseLevel(%q) = (%v, %v), want config.ParseLevel's (%v, %v)", s, gotLv, gotOK, wantLv, wantOK)
		}
	}
	if telemetry.LevelTrace != config.LevelTrace {
		t.Errorf("telemetry.LevelTrace = %v, want config.LevelTrace (%v)", telemetry.LevelTrace, config.LevelTrace)
	}
}

// TestConfigDoesNotImportTelemetry scans the package's non-test sources for
// internal imports: config is the bottom layer, so production files must not
// depend on telemetry (or any other internal package). Pre-fix,
// config_validate.go imported telemetry for ParseLevel.
func TestConfigDoesNotImportTelemetry(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(path, "freebuff-proxy/backend/internal/") {
				t.Errorf("%s imports %s; config is a bottom-layer package and must not import internal packages", name, path)
			}
		}
	}
}
