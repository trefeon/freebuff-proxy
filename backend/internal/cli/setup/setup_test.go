package setup

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSetupAiderConfigPreservesUserConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".aider.conf.yml")
	original := "theme: dracula\neditor: vim\nmodel: gpt-4o\n# my custom comment\nopenai-api-base: http://old.example/v1\nsome-other-setting: true\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupAiderConfig(cfgPath) {
		t.Fatal("setupAiderConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	for _, want := range []string{
		"theme: dracula",
		"editor: vim",
		"# my custom comment",
		"some-other-setting: true",
		"openai-api-base: http://localhost:3457/v1",
		"openai-api-key: not-needed",
		"model: openai/deepseek/deepseek-v4-flash",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}

	if strings.Contains(got, "http://old.example/v1") {
		t.Errorf("old openai-api-base value was not replaced:\n%s", got)
	}
	if strings.Contains(got, "gpt-4o") {
		t.Errorf("old model value was not replaced:\n%s", got)
	}
	for _, key := range []string{"openai-api-base:", "openai-api-key:", "model:"} {
		if n := strings.Count(got, key); n != 1 {
			t.Errorf("expected exactly one %q line, got %d:\n%s", key, n, got)
		}
	}
}

func TestSetupAiderConfigAppendsMissingKeys(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".aider.conf.yml")
	original := "theme: dracula\ncustom-setting: keep-me\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupAiderConfig(cfgPath) {
		t.Fatal("setupAiderConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	for _, want := range []string{
		"theme: dracula",
		"custom-setting: keep-me",
		"openai-api-base: http://localhost:3457/v1",
		"openai-api-key: not-needed",
		"model: openai/deepseek/deepseek-v4-flash",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), "model: openai/deepseek/deepseek-v4-flash") {
		t.Errorf("missing proxy keys should be appended at the end; got:\n%s", got)
	}
}

func TestSetupAiderConfigFreshFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".aider.conf.yml")

	if !setupAiderConfig(cfgPath) {
		t.Fatal("setupAiderConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if n := strings.Count(got, "\n"); n != 3 {
		t.Errorf("expected 3 lines for a fresh config, got %d lines:\n%s", n, got)
	}
	for _, want := range []string{
		"openai-api-base: http://localhost:3457/v1",
		"openai-api-key: not-needed",
		"model: openai/deepseek/deepseek-v4-flash",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

func TestSetupAiderConfigShortCircuitsWhenAlreadyConfigured(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".aider.conf.yml")
	original := "theme: dracula\nopenai-api-base: http://localhost:3457/v1\nopenai-api-key: not-needed\nmodel: openai/deepseek/deepseek-v4-flash\ncustom: keep-me\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupAiderConfig(cfgPath) {
		t.Fatal("setupAiderConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != original {
		t.Errorf("already-configured file was modified:\n%s", string(out))
	}
}

func TestMergeAiderConfigPreservesLineEndings(t *testing.T) {
	original := "theme: dracula\r\nmodel: gpt-4o\r\n"
	lines := []string{
		"openai-api-base: http://localhost:3457/v1",
		"openai-api-key: not-needed",
		"model: openai/deepseek/deepseek-v4-flash",
	}
	got := mergeAiderConfig(original, lines)
	if !strings.Contains(got, "\r\n") {
		t.Errorf("expected CRLF line endings preserved, got:\n%q", got)
	}
	if !strings.Contains(got, "theme: dracula\r\n") {
		t.Errorf("unrelated CRLF line not preserved:\n%q", got)
	}
	if !strings.Contains(got, "model: openai/deepseek/deepseek-v4-flash\r\n") {
		t.Errorf("replaced model line missing or wrong line ending:\n%q", got)
	}
}

func TestSetupContinueYamlConfigMergesIntoExistingModels(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	original := "name: My Workspace\nversion: 1.0\nmodels:\n  - title: \"Existing Model\"\n    provider: \"openai\"\n    model: \"gpt-4o\"\n    apiBase: \"http://existing.example/v1\"\n    apiKey: \"secret\"\n# trailing comment\nagents:\n  - name: \"Agent\"\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupContinueYamlConfig(cfgPath) {
		t.Fatal("setupContinueYamlConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	// Exactly one top-level models: key — the fix for the duplicate-key bug.
	if n := strings.Count(got, "models:"); n != 1 {
		t.Errorf("expected exactly one top-level models: key, got %d:\n%s", n, got)
	}
	for _, want := range []string{
		"name: My Workspace",
		"version: 1.0",
		`  - title: "Existing Model"`,
		`    model: "gpt-4o"`,
		`    apiBase: "http://existing.example/v1"`,
		`    apiKey: "secret"`,
		"# trailing comment",
		"agents:",
		`  - name: "Agent"`,
		`  - title: "FreeBuff DeepSeek Flash"`,
		`    model: "deepseek/deepseek-v4-flash"`,
		`    apiBase: "http://localhost:3457/v1"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}

	// The FreeBuff item goes at the END of the existing models: list: after
	// the user's last model, before the next top-level key/comment.
	idxFree := strings.Index(got, `  - title: "FreeBuff DeepSeek Flash"`)
	idxExisting := strings.Index(got, `  - title: "Existing Model"`)
	idxAgents := strings.Index(got, "agents:")
	idxComment := strings.Index(got, "# trailing comment")
	if idxFree < 0 || idxExisting < 0 || idxAgents < 0 || idxComment < 0 {
		t.Fatalf("expected markers not found:\n%s", got)
	}
	if idxFree < idxExisting {
		t.Errorf("FreeBuff model inserted before the existing model:\n%s", got)
	}
	if idxFree > idxAgents {
		t.Errorf("FreeBuff model inserted after the next top-level key agents::\n%s", got)
	}
	if idxFree > idxComment {
		t.Errorf("FreeBuff model inserted after the top-level comment:\n%s", got)
	}
}

func TestSetupContinueYamlConfigAppendsWhenNoModelsKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	original := "name: My Workspace\nversion: 1.0\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupContinueYamlConfig(cfgPath) {
		t.Fatal("setupContinueYamlConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	if n := strings.Count(got, "models:"); n != 1 {
		t.Errorf("expected exactly one top-level models: key, got %d:\n%s", n, got)
	}
	for _, want := range []string{
		"name: My Workspace",
		"version: 1.0",
		`  - title: "FreeBuff DeepSeek Flash"`,
		`    apiBase: "http://localhost:3457/v1"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

func TestSetupContinueYamlConfigShortCircuitsWhenAlreadyConfigured(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	original := "name: My Workspace\nmodels:\n  - title: \"Old\"\n    apiBase: \"http://localhost:3457/v1\"\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupContinueYamlConfig(cfgPath) {
		t.Fatal("setupContinueYamlConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != original {
		t.Errorf("already-configured file was modified:\n%s", string(out))
	}
}

func TestSetupOpencodeConfigJSONCCommentsLeaveFileUntouched(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "opencode.json")
	// opencode.json allows // comments (JSONC); Go's json package does not.
	// The config must never be rewritten from scratch in that case — the
	// user's providers/agents/MCPs (and API keys) would be deleted.
	original := `{
  // opencode.json accepts JSONC comments; Go's encoding/json does not.
  "provider": {
    "anthropic": {
      "options": { "apiKey": "sk-ant-secret" }
    }
  },
  "agent": { "build": { "prompt": "hi" } }
}
`
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if setupOpencodeConfig(cfgPath) {
		t.Fatal("setupOpencodeConfig should return false for an unparseable JSONC config")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != original {
		t.Errorf("JSONC config was modified; must be left untouched:\n%s", string(out))
	}
}

func TestSetupOpencodeConfigAddsFreebuffProvider(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "opencode.json")
	original := `{"providers": {"anthropic": {"options": {"apiKey": "sk-ant-secret"}}}}`
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupOpencodeConfig(cfgPath) {
		t.Fatal("setupOpencodeConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("rewritten opencode.json is not valid JSON: %v\n%s", err, out)
	}
	providers, ok := cfg["providers"].(map[string]any)
	if !ok {
		t.Fatalf("rewritten opencode.json lost the providers key:\n%s", out)
	}
	if _, ok := providers["freebuff"]; !ok {
		t.Errorf("freebuff provider missing from rewritten config:\n%s", out)
	}
	if _, ok := providers["anthropic"]; !ok {
		t.Errorf("existing anthropic provider was deleted from rewritten config:\n%s", out)
	}
}

// continueItems is the FreeBuff list item used by the pure
// mergeContinueYamlModels tests (setupContinueYamlConfig builds its own).
var continueItems = []string{
	`  - title: "FreeBuff DeepSeek Flash"`,
	`    model: "deepseek/deepseek-v4-flash"`,
}

var continueSnippet = "\nmodels:\n" + strings.Join(continueItems, "\n") + "\n"

// TestMergeContinueYamlModelsFlowStyleRejected pins the flow-style guard: a
// top-level "models:" line carrying an inline value (e.g. "models: []")
// cannot be merged line-wise, so ok=false leaves the file untouched.
func TestMergeContinueYamlModelsFlowStyleRejected(t *testing.T) {
	for _, existing := range []string{
		"models: []\n",
		"name: w\nmodels: [a, b]\n",
		"models: {}\n",
	} {
		if got, ok := mergeContinueYamlModels(existing, continueItems, continueSnippet); ok {
			t.Errorf("merge(%q) ok=true, want false (flow-style inline value)", existing)
			if got != "" {
				t.Errorf("merge(%q) returned modified text %q, want untouched", existing, got)
			}
		}
	}
}

// TestMergeContinueYamlModelsEmptyInput pins the no-models-key path on
// empty input: the whole snippet is appended verbatim.
func TestMergeContinueYamlModelsEmptyInput(t *testing.T) {
	got, ok := mergeContinueYamlModels("", continueItems, continueSnippet)
	if !ok {
		t.Fatal("merge(empty) ok=false, want true")
	}
	if got != continueSnippet {
		t.Errorf("merge(empty) = %q, want snippet %q", got, continueSnippet)
	}
}

// TestMergeContinueYamlModelsCRLF pins the line-ending preservation: a CRLF
// file keeps CRLF everywhere, including the inserted item lines.
func TestMergeContinueYamlModelsCRLF(t *testing.T) {
	existing := "name: w\r\nmodels:\r\n  - title: \"Old\"\r\n    model: \"gpt-4o\"\r\n"
	got, ok := mergeContinueYamlModels(existing, continueItems, continueSnippet)
	if !ok {
		t.Fatal("merge ok=false, want true")
	}
	if strings.Count(got, "models:") != 1 {
		t.Errorf("expected exactly one top-level models: key, got %d:\n%q", strings.Count(got, "models:"), got)
	}
	if !strings.Contains(got, "name: w\r\n") {
		t.Errorf("existing CRLF line not preserved:\n%q", got)
	}
	for _, item := range continueItems {
		if !strings.Contains(got, item+"\r\n") {
			t.Errorf("item %q missing with CRLF ending:\n%q", item, got)
		}
	}
}

// TestMergeContinueYamlModelsTrailingBlanks pins the insertion point with a
// blank-terminated list: the item goes after the last non-blank sibling,
// leaving the trailing blank lines below it.
func TestMergeContinueYamlModelsTrailingBlanks(t *testing.T) {
	existing := "models:\n  - title: \"Old\"\n\n\n"
	got, ok := mergeContinueYamlModels(existing, continueItems, continueSnippet)
	if !ok {
		t.Fatal("merge ok=false, want true")
	}
	// The FreeBuff item directly follows the last model — no blank between.
	if !strings.Contains(got, `  - title: "Old"`+"\n  - title: \"FreeBuff DeepSeek Flash\"\n") {
		t.Errorf("item not inserted right after the last model:\n%q", got)
	}
	// The trailing blank lines stay at the end, below the inserted item.
	if !strings.HasSuffix(got, "\n\n\n") {
		t.Errorf("trailing blanks not preserved at EOF:\n%q", got)
	}
}

// TestMergeContinueYamlModelsTopLevelCommentAfterModels pins the insertion
// point when a column-0 comment closes the models: list: the item goes
// before the comment.
func TestMergeContinueYamlModelsTopLevelCommentAfterModels(t *testing.T) {
	existing := "models:\n  - title: \"Old\"\n# comment\nagents:\n"
	got, ok := mergeContinueYamlModels(existing, continueItems, continueSnippet)
	if !ok {
		t.Fatal("merge ok=false, want true")
	}
	idxItem := strings.Index(got, `  - title: "FreeBuff DeepSeek Flash"`)
	idxOld := strings.Index(got, `  - title: "Old"`)
	idxComment := strings.Index(got, "# comment")
	idxAgents := strings.Index(got, "agents:")
	if idxItem < 0 || idxOld < 0 || idxComment < 0 || idxAgents < 0 {
		t.Fatalf("expected markers missing:\n%q", got)
	}
	if idxItem < idxOld || idxItem > idxComment || idxItem > idxAgents {
		t.Errorf("item misplaced (old=%d item=%d comment=%d agents=%d):\n%q", idxOld, idxItem, idxComment, idxAgents, got)
	}
}

// TestMergeContinueYamlModelsLeadingSpaceNotMatched pins the known
// limitation: an INDENTED "models:" line is not a top-level key, so the
// snippet is appended verbatim (a duplicate top-level key results). The
// line-wise merge only ever matches column-0 keys.
func TestMergeContinueYamlModelsLeadingSpaceNotMatched(t *testing.T) {
	existing := "  models:\n  - title: \"Old\"\n"
	got, ok := mergeContinueYamlModels(existing, continueItems, continueSnippet)
	if !ok {
		t.Fatal("merge ok=false, want true")
	}
	if got != existing+continueSnippet {
		t.Errorf("indented models: not recognized as top-level; got %q, want append %q", got, existing+continueSnippet)
	}
}

// TestBackupFile pins backupFile: a .bak is created with the source content
// and (regression S3) the source's permission bits — floored at 0600, so a
// secret-bearing 0600 config never leaves a world-readable backup. A
// missing source or an unreadable path is a silent no-op.
func TestBackupFile(t *testing.T) {
	t.Run("creates backup with content", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "config.yaml")
		content := []byte("apiKey: secret\nmodel: x\n")
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		backupFile(p)
		got, err := os.ReadFile(p + ".bak")
		if err != nil {
			t.Fatalf("backup not created: %v", err)
		}
		if string(got) != string(content) {
			t.Errorf("backup = %q, want %q", got, content)
		}
	})

	t.Run("no-op when source missing", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "missing.yaml")
		backupFile(p)
		if _, err := os.Stat(p + ".bak"); !os.IsNotExist(err) {
			t.Errorf("backup created for a missing source (err=%v)", err)
		}
	})

	t.Run("no-op on read failure", func(t *testing.T) {
		// A directory passes the exists check but fails the read.
		dir := t.TempDir()
		backupFile(dir)
		if _, err := os.Stat(dir + ".bak"); !os.IsNotExist(err) {
			t.Errorf("backup created for an unreadable path (err=%v)", err)
		}
	})

	t.Run("preserves source permission bits", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX permission bits are not meaningful on Windows")
		}
		dir := t.TempDir()
		for _, tc := range []struct{ src, want os.FileMode }{
			{0o600, 0o600}, // secret config: never 0644
			{0o644, 0o644}, // already-world-readable source: preserved
			{0o400, 0o600}, // below-0600 source: floored at 0600
		} {
			p := filepath.Join(dir, "cfg")
			if err := os.WriteFile(p, []byte("apiKey: secret\n"), tc.src); err != nil {
				t.Fatal(err)
			}
			backupFile(p)
			fi, err := os.Stat(p + ".bak")
			if err != nil {
				t.Fatalf("backup stat: %v", err)
			}
			if got := fi.Mode().Perm(); got != tc.want {
				t.Errorf("backup mode = %v, want %v (source %v)", got, tc.want, tc.src)
			}
			_ = os.Remove(p + ".bak")
			_ = os.Remove(p)
		}
	})
}

// TestSetupContinueConfigMalformedJSONAborts is the S2 regression: a
// truncated/unparseable Continue config.json must abort (return false) and
// leave the file byte-identical — never rebuilt from scratch with only the
// FreeBuff model, which would silently delete the user's models/apiKeys.
func TestSetupContinueConfigMalformedJSONAborts(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	original := `{ "models": [ { "title": "Keep Me", "provider": "anthropic", "model": "claude-x", "apiKey": "sk-secret" },` // truncated
	if err := os.WriteFile(p, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if setupContinueConfig(p) {
		t.Fatal("setupContinueConfig must return false on malformed JSON (S2 data-loss regression)")
	}
	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != original {
		t.Errorf("malformed JSON config was rewritten; must be left untouched:\n%s", out)
	}
}

// TestSetupContinueConfigPreservesParsableConfig pins the happy path of the
// S2 fix: a VALID JSON config still gets the FreeBuff model merged in.
func TestSetupContinueConfigPreservesParsableConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	original := `{"models":[{"title":"Existing","provider":"anthropic","model":"claude-x","apiKey":"sk-secret"}]}`
	if err := os.WriteFile(p, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if !setupContinueConfig(p) {
		t.Fatal("setupContinueConfig returned false on valid JSON")
	}
	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("rewritten config.json is not valid JSON: %v\n%s", err, out)
	}
	models, ok := cfg["models"].([]any)
	if !ok || len(models) != 2 {
		t.Fatalf("models = %v, want 2 (existing + freebuff)", cfg["models"])
	}
	if _, ok := models[0].(map[string]any)["apiKey"]; !ok {
		t.Errorf("existing model's apiKey lost:\n%s", out)
	}
	if !strings.Contains(string(out), "deepseek/deepseek-v4-flash") {
		t.Errorf("freebuff model missing:\n%s", out)
	}
}

// TestSetupOpencodeConfigNonMapProviders pins the non-map providers
// handling: a providers value that is not an object (invalid for opencode,
// but possible in a hand-edited file) is replaced with a fresh map holding
// the freebuff provider — the rest of the config survives.
func TestSetupOpencodeConfigNonMapProviders(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "opencode.json")
	original := `{"providers": ["not", "a", "map"], "agent": {"build": {"prompt": "hi"}}}`
	if err := os.WriteFile(p, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if !setupOpencodeConfig(p) {
		t.Fatal("setupOpencodeConfig returned false")
	}
	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("rewritten opencode.json is not valid JSON: %v\n%s", err, out)
	}
	providers, ok := cfg["providers"].(map[string]any)
	if !ok {
		t.Fatalf("providers = %T, want map (the array was replaced)", cfg["providers"])
	}
	if _, ok := providers["freebuff"]; !ok {
		t.Errorf("freebuff provider missing after replacement:\n%s", out)
	}
	if cfg["agent"] == nil {
		t.Errorf("unrelated agent config was dropped:\n%s", out)
	}
}

// TestSetupAiderConfigUnreadableAborts is the S4 regression: an existing
// but unreadable aider config path (here: a directory, which passes the
// exists check but fails ReadFile on every platform) must abort — the
// fresh-file fallback must never clobber a file it could not read.
func TestSetupAiderConfigUnreadableAborts(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if setupAiderConfig(blocked) {
		t.Fatal("setupAiderConfig must abort on an unreadable existing path (S4 clobber regression)")
	}
	fi, err := os.Stat(blocked)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Error("unreadable path was replaced with a fresh file")
	}
}

// TestMergeAiderConfigColonlessKey pins the colon-less-key edge of the
// merge helper: a key without ':' produces the empty prefix, which matches
// every line and replaces the FIRST line — the key is then considered found
// and never appended. (Unreachable from setupAiderConfig's fixed key list,
// but the helper is directly testable.)
func TestMergeAiderConfigColonlessKey(t *testing.T) {
	existing := "theme: dracula\nmodel: gpt-4o\n"
	lines := []string{"openai-api-key", "model: openai/deepseek/deepseek-v4-flash"}
	got := mergeAiderConfig(existing, lines)
	want := "openai-api-key\nmodel: openai/deepseek/deepseek-v4-flash\n"
	if got != want {
		t.Errorf("mergeAiderConfig = %q, want %q", got, want)
	}
}

// TestMergeAiderConfigDuplicateKey pins the duplicate-key edge: only the
// FIRST occurrence of an existing key is replaced; later duplicates stay.
func TestMergeAiderConfigDuplicateKey(t *testing.T) {
	existing := "model: gpt-4o\nmodel: gpt-4o-2\n"
	lines := []string{"model: openai/deepseek/deepseek-v4-flash"}
	got := mergeAiderConfig(existing, lines)
	want := "model: openai/deepseek/deepseek-v4-flash\nmodel: gpt-4o-2\n"
	if got != want {
		t.Errorf("mergeAiderConfig = %q, want %q", got, want)
	}
}

// TestPromptYesNo pins the confirmation reader: y/yes accept
// case-insensitively, anything else (n, empty, garbage, EOF) declines, and
// autoYes returns true WITHOUT reading stdin at all.
func TestPromptYesNo(t *testing.T) {
	cases := []struct {
		name, input string
		want        bool
	}{
		{"y", "y\n", true},
		{"Y uppercase", "Y\n", true},
		{"yes", "yes\n", true},
		{"mixed case", "YeS\n", true},
		{"n", "n\n", false},
		{"empty line", "\n", false},
		{"garbage", "maybe\n", false},
		{"EOF", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := promptYesNo(bufio.NewReader(strings.NewReader(tc.input)), false); got != tc.want {
				t.Errorf("promptYesNo(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
	t.Run("autoYes skips stdin", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_ = w.Close() // any read would return EOF immediately
		if got := promptYesNo(bufio.NewReader(r), true); !got {
			t.Error("autoYes returned false")
		}
		_ = r.Close()
	})
}

// TestRunSetupHomeUnsetExits1 pins runSetup's home-resolution failure: when
// the user home directory cannot be determined, runSetup prints the error
// and exits 1 before touching any client config. runSetup os.Exit(1)s, so
// it runs in a re-exec'd helper process. os.UserHomeDir falls back to
// user.Current() on most dev/CI boxes, so the branch only reproduces where
// that lookup fails (minimal containers) — the test skips otherwise.
func TestRunSetupHomeUnsetExits1(t *testing.T) {
	if os.Getenv("GO_WANT_SETUP_HOME_HELPER") == "1" {
		Run(false)
		return // unreachable: runSetup os.Exit(1)s
	}

	envKey := "HOME"
	switch runtime.GOOS {
	case "windows":
		envKey = "USERPROFILE"
	case "plan9":
		envKey = "home"
	}
	// Reproducibility pre-check: with the env source emptied, does
	// os.UserHomeDir error in THIS environment?
	prev, had := os.LookupEnv(envKey)
	if err := os.Setenv(envKey, ""); err != nil {
		t.Fatal(err)
	}
	_, homeErr := os.UserHomeDir()
	if had {
		_ = os.Setenv(envKey, prev)
	} else {
		_ = os.Unsetenv(envKey)
	}
	if homeErr == nil {
		t.Skip("os.UserHomeDir resolves via user.Current() here; the exit-1 branch is not reproducible")
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunSetupHomeUnsetExits1$")
	cmd.Env = append(os.Environ(), "GO_WANT_SETUP_HOME_HELPER=1", envKey+"=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helper exited 0, want 1\n%s", out)
	}
	if !strings.Contains(string(out), "cannot determine user home directory") {
		t.Errorf("helper output missing home error:\n%s", out)
	}
}
