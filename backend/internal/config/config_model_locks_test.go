package config

import (
	"os"
	"reflect"
	"testing"
)

func TestParseModelLocks(t *testing.T) {
	got, err := parseModelLocks("")
	if err != nil || got != nil {
		t.Fatalf("empty = %v, %v; want nil, nil", got, err)
	}
	got, err = parseModelLocks("0:z-ai/glm-5.2;1:deepseek/deepseek-v4-flash,mimo/mimo-v2.5")
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	want := map[int][]string{
		0: {"z-ai/glm-5.2"},
		1: {"deepseek/deepseek-v4-flash", "mimo/mimo-v2.5"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parse = %v, want %v", got, want)
	}
	for _, bad := range []string{
		"no-colon-here",
		"x:model-a",
		"-1:model-a",
		"0:",
		"0:  ",
	} {
		if _, err := parseModelLocks(bad); err == nil {
			t.Errorf("parse %q succeeded, want error", bad)
		}
	}
}

// TestModelLocksDotenv verifies MODEL_LOCKS flows from .env through Load
// and that malformed values reject the config instead of silently
// misrouting quota.
func TestModelLocksDotenv(t *testing.T) {
	clearEnv(t)

	content := "MODEL_LOCKS=0:z-ai/glm-5.2;1:deepseek/deepseek-v4-flash,mimo/mimo-v2.5\n"
	if err := os.WriteFile(".env", []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[int][]string{
		0: {"z-ai/glm-5.2"},
		1: {"deepseek/deepseek-v4-flash", "mimo/mimo-v2.5"},
	}
	if !reflect.DeepEqual(cfg.ModelLocks, want) {
		t.Errorf("ModelLocks = %v, want %v (from .env)", cfg.ModelLocks, want)
	}

	if err := os.WriteFile(".env", []byte("MODEL_LOCKS=banana\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(""); err == nil {
		t.Errorf("Load with malformed MODEL_LOCKS succeeded, want error")
	}
}
