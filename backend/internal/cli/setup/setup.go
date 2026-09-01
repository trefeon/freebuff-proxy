// Package setup implements the -setup mode: detect installed AI client
// tools (Continue, opencode, aider) and offer to add the FreeBuff provider,
// without modifying any file without explicit permission.
package setup

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Run drives the interactive client-configuration helper and exits. It
// preserves the original -setup behavior exactly: prompts (auto-confirmed
// with autoYes) and os.Exit codes.
func Run(autoYes bool) {
	fmt.Println("freebuff-proxy interactive client setup")
	fmt.Println("======================================")
	fmt.Println("This helper detects installed AI tools and offers to configure them.")
	fmt.Println("No files will be modified without your explicit permission.")

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintf(os.Stderr, "ERROR: cannot determine user home directory: %v\n", err)
		os.Exit(1)
	}

	reader := bufio.NewReader(os.Stdin)
	ask := func(prompt string) bool {
		if !autoYes {
			fmt.Printf("\n%s [y/N]: ", prompt)
		}
		return promptYesNo(reader, autoYes)
	}

	configured := 0

	// 1. Continue (VS Code / JetBrains)
	continueDir := filepath.Join(home, ".continue")
	continueYamlPath := filepath.Join(continueDir, "config.yaml")
	continueJsonPath := filepath.Join(continueDir, "config.json")
	if fileExists(continueDir) || fileExists(continueYamlPath) || fileExists(continueJsonPath) {
		fmt.Printf("\n[+] Detected Continue extension (~/.continue/)\n")
		targetPath := continueYamlPath
		if !fileExists(continueYamlPath) && fileExists(continueJsonPath) {
			targetPath = continueJsonPath
		}
		if ask(fmt.Sprintf("Would you like to add freebuff-proxy to Continue (%s)?", filepath.Base(targetPath))) {
			if strings.HasSuffix(targetPath, ".yaml") || strings.HasSuffix(targetPath, ".yml") {
				if setupContinueYamlConfig(targetPath) {
					fmt.Printf("    [ok] Configured Continue in %s (backup saved to .bak)\n", targetPath)
					configured++
				}
			} else {
				if setupContinueConfig(targetPath) {
					fmt.Printf("    [ok] Configured Continue in %s (backup saved to .bak)\n", targetPath)
					configured++
				}
			}
		} else {
			fmt.Println("    [skipped] Left Continue config untouched.")
			fmt.Println("    Manual snippet for ~/.continue/config.yaml:")
			fmt.Println("    models:\n      - title: \"FreeBuff DeepSeek\"\n        provider: \"openai\"\n        model: \"deepseek/deepseek-v4-flash\"\n        apiBase: \"http://localhost:3457/v1\"\n        apiKey: \"not-needed\"")
		}
	} else {
		fmt.Println("[-] Continue (~/.continue/) not found on this system")
	}

	// 2. opencode
	opencodeDir := filepath.Join(home, ".config", "opencode")
	opencodeCfgPath := filepath.Join(opencodeDir, "opencode.json")
	if fileExists(opencodeDir) || fileExists(opencodeCfgPath) {
		fmt.Printf("\n[+] Detected opencode (~/.config/opencode/)\n")
		if ask("Would you like to add the freebuff provider to opencode.json?") {
			if setupOpencodeConfig(opencodeCfgPath) {
				fmt.Printf("    [ok] Configured opencode in %s (backup saved to .bak)\n", opencodeCfgPath)
				configured++
			}
		} else {
			fmt.Println("    [skipped] Left opencode config untouched.")
			fmt.Println("    Manual snippet for ~/.config/opencode/opencode.json:")
			fmt.Println(`    "freebuff": {"type": "openai", "options": {"baseURL": "http://localhost:3457/v1", "apiKey": "not-needed"}}`)
		}
	} else {
		fmt.Println("[-] opencode (~/.config/opencode/) not found on this system")
	}

	// 3. aider
	aiderCfgPath := filepath.Join(home, ".aider.conf.yml")
	if _, err := exec.LookPath("aider"); err != nil {
		fmt.Println("[-] aider not found on this system")
	} else if ask("Would you like to configure aider in ~/.aider.conf.yml?") {
		if setupAiderConfig(aiderCfgPath) {
			fmt.Printf("    [ok] Configured aider in %s\n", aiderCfgPath)
			configured++
		}
	} else {
		fmt.Println("    [skipped] Left aider config untouched.")
		fmt.Println("    Manual flags: aider --openai-api-base http://localhost:3457/v1 --openai-api-key not-needed")
	}

	fmt.Printf("\n======================================\n")
	fmt.Printf("Setup complete! Configured %d client tool(s).\n", configured)
	fmt.Println("Base URL: http://localhost:3457/v1")
	// The model list is served live by the proxy; pointing at the endpoint
	// beats maintaining a hardcoded, drifting copy here.
	fmt.Println("Models available: query http://localhost:3457/v1/models for the live list")
	fmt.Println("Note: which models your account can use is per-account; query that endpoint to see the live availability.")
	os.Exit(0)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// promptYesNo reads one confirmation answer from reader: y/yes
// (case-insensitive) accepts; anything else (n, empty line, garbage, EOF)
// declines. autoYes accepts without reading stdin at all — the caller's
// prompts are skipped wholesale.
func promptYesNo(reader *bufio.Reader, autoYes bool) bool {
	if autoYes {
		return true
	}
	input, _ := reader.ReadString('\n')
	input = strings.ToLower(strings.TrimSpace(input))
	return input == "y" || input == "yes"
}

func backupFile(p string) {
	if !fileExists(p) {
		return
	}
	bak := p + ".bak"
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	// S3: preserve the source's permission bits, floored at 0600. Continue
	// config.yaml / opencode.json carry apiKey secrets and are often 0600; a
	// hardcoded 0644 backup would leave the secret world-readable.
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(p); err == nil {
		if perm := fi.Mode().Perm(); perm > mode {
			mode = perm
		}
	}
	_ = os.WriteFile(bak, data, mode)
}

func setupContinueYamlConfig(p string) bool {
	dir := filepath.Dir(p)
	_ = os.MkdirAll(dir, 0755)

	backupFile(p)

	// The FreeBuff entry as a YAML list item under a top-level models: key.
	// One source of truth: used both to build the fresh-config snippet and to
	// insert into an existing models: list (see mergeContinueYamlModels).
	freebuffModel := []string{
		`  - title: "FreeBuff DeepSeek Flash"`,
		`    provider: "openai"`,
		`    model: "deepseek/deepseek-v4-flash"`,
		`    apiBase: "http://localhost:3457/v1"`,
		`    apiKey: "not-needed"`,
	}
	snippet := "\nmodels:\n" + strings.Join(freebuffModel, "\n") + "\n"
	if fileExists(p) {
		existing, err := os.ReadFile(p)
		if err != nil {
			return false
		}
		if strings.Contains(string(existing), "localhost:3457") {
			return true
		}
		merged, ok := mergeContinueYamlModels(string(existing), freebuffModel, snippet)
		if !ok {
			return false
		}
		return os.WriteFile(p, []byte(merged), 0644) == nil
	}
	return os.WriteFile(p, []byte(snippet), 0644) == nil
}

// mergeContinueYamlModels merges the FreeBuff model into existing Continue
// YAML text WITHOUT a YAML parser, preserving every byte outside the edit.
// When the file already has a top-level "models:" key (a line starting at
// column 0), the list item (itemLines) is inserted at the END of that list —
// after its last non-blank sibling, before the next column-0 key, comment or
// EOF — so the user's existing models stay intact and no duplicate key
// appears. When no top-level models: key exists, snippet (key + item) is
// appended verbatim. A "models:" line carrying an inline value (flow style,
// e.g. "models: []") cannot be merged line-wise: ok=false leaves the file
// untouched rather than corrupting it.
func mergeContinueYamlModels(existing string, itemLines []string, snippet string) (string, bool) {
	lines := strings.Split(existing, "\n")
	crlf := strings.Contains(existing, "\r\n")

	modelsIdx := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "models:") {
			if strings.TrimSpace(strings.TrimPrefix(line, "models:")) != "" {
				// Inline value: flow-style list. Rewriting it line-wise would
				// corrupt the file; leave it to the user.
				return "", false
			}
			modelsIdx = i
			break
		}
	}
	if modelsIdx < 0 {
		// No top-level models: key — append the whole snippet.
		return existing + snippet, true
	}

	// The list body runs from modelsIdx+1 until the next column-0 line
	// (non-blank and unindented: another key or a top-level comment) or EOF.
	// Insert the new item after the last non-blank line of that body so
	// trailing blank lines stay below the item.
	end := len(lines)
	for j := modelsIdx + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) != "" && lines[j][0] != ' ' && lines[j][0] != '\t' {
			end = j
			break
		}
	}
	last := modelsIdx
	for j := modelsIdx + 1; j < end; j++ {
		if strings.TrimSpace(lines[j]) != "" {
			last = j
		}
	}

	merged := make([]string, 0, len(lines)+len(itemLines))
	merged = append(merged, lines[:last+1]...)
	for _, item := range itemLines {
		if crlf {
			item += "\r" // keep the file's line-ending style
		}
		merged = append(merged, item)
	}
	merged = append(merged, lines[last+1:]...)
	return strings.Join(merged, "\n"), true
}

func setupContinueConfig(p string) bool {
	dir := filepath.Dir(p)
	_ = os.MkdirAll(dir, 0755)

	backupFile(p)

	var cfg map[string]any
	if data, err := os.ReadFile(p); err == nil {
		if jsonErr := json.Unmarshal(data, &cfg); jsonErr != nil {
			// S2: a failed parse must NOT be treated as an empty config —
			// rebuilding from scratch would overwrite the user's Continue
			// models/apiKeys with only the FreeBuff entry. Abort and leave
			// the file untouched (the .bak made above preserves the original
			// either way), mirroring setupOpencodeConfig's JSONC abort.
			fmt.Fprintf(os.Stderr, "ERROR: could not parse existing Continue config.json: %v - add the freebuff model manually, original saved as %s.bak\n", jsonErr, p)
			return false
		}
	}
	if cfg == nil {
		cfg = make(map[string]any)
	}

	models, _ := cfg["models"].([]any)
	hasFreebuff := false
	for _, m := range models {
		if mm, ok := m.(map[string]any); ok {
			if apiBase, _ := mm["apiBase"].(string); strings.Contains(apiBase, "3457") {
				hasFreebuff = true
				break
			}
		}
	}

	if !hasFreebuff {
		newModel := map[string]any{
			"title":    "FreeBuff DeepSeek Flash",
			"provider": "openai",
			"model":    "deepseek/deepseek-v4-flash",
			"apiBase":  "http://localhost:3457/v1",
			"apiKey":   "not-needed",
		}
		models = append(models, newModel)
		cfg["models"] = models

		out, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return false
		}
		return os.WriteFile(p, out, 0644) == nil
	}

	return true
}

func setupOpencodeConfig(p string) bool {
	dir := filepath.Dir(p)
	_ = os.MkdirAll(dir, 0755)

	backupFile(p)

	var cfg map[string]any
	data, err := os.ReadFile(p)
	if err == nil {
		if jsonErr := json.Unmarshal(data, &cfg); jsonErr != nil {
			// opencode.json allows // comments (JSONC); json.Unmarshal
			// rejects them, so a failed parse must NOT be treated as an
			// empty config — rewriting the file from scratch would silently
			// delete the user's providers/agents/MCPs (and their API keys).
			// Abort and leave the file untouched; the .bak made above
			// preserves the original either way.
			fmt.Fprintf(os.Stderr, "ERROR: could not parse existing opencode.json (it may contain JSONC comments); add the freebuff provider manually - original saved as %s.bak\n", p)
			return false
		}
	}
	if cfg == nil {
		cfg = make(map[string]any)
	}

	providers, ok := cfg["providers"].(map[string]any)
	if !ok {
		providers = make(map[string]any)
	}

	providers["freebuff"] = map[string]any{
		"type": "openai",
		"options": map[string]any{
			"baseURL": "http://localhost:3457/v1",
			"apiKey":  "not-needed",
		},
		"models": []map[string]any{
			{"id": "deepseek/deepseek-v4-flash", "name": "DeepSeek Flash"},
			{"id": "z-ai/glm-5.2", "name": "GLM 5.2"},
		},
	}
	cfg["providers"] = providers

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false
	}
	return os.WriteFile(p, out, 0644) == nil
}

func setupAiderConfig(p string) bool {
	newLines := []string{
		"openai-api-base: http://localhost:3457/v1",
		"openai-api-key: not-needed",
		"model: openai/deepseek/deepseek-v4-flash",
	}
	if fileExists(p) {
		existing, err := os.ReadFile(p)
		if err != nil {
			// S4: never clobber a file we could not read. An existing but
			// unreadable (EACCES) aider config must be left untouched, not
			// replaced with a fresh one.
			fmt.Fprintf(os.Stderr, "ERROR: could not read existing %s: %v - leaving it untouched\n", p, err)
			return false
		}
		if strings.Contains(string(existing), "localhost:3457") {
			return true
		}
		backupFile(p)
		merged := mergeAiderConfig(string(existing), newLines)
		return os.WriteFile(p, []byte(merged), 0644) == nil
	}
	return os.WriteFile(p, []byte(strings.Join(newLines, "\n")+"\n"), 0644) == nil
}

// mergeAiderConfig merges key:value lines into existing YAML-style config
// text, preserving every unrelated line. A key already present (matched by its
// "key:" line prefix) is replaced in place; missing keys are appended at the
// end. The file's original line-ending style is preserved.
func mergeAiderConfig(existing string, lines []string) string {
	nl := "\n"
	if strings.Contains(existing, "\r\n") {
		nl = "\r\n"
	}

	split := strings.Split(existing, "\n")
	// Strip the \r left by CRLF line endings so replacement lines don't end
	// up with doubled \r after the join below.
	for i, l := range split {
		split[i] = strings.TrimSuffix(l, "\r")
	}
	found := make(map[string]bool)
	for _, line := range lines {
		key := line[:strings.Index(line, ":")+1]
		for i, l := range split {
			if strings.HasPrefix(l, key) {
				split[i] = line
				found[key] = true
				break
			}
		}
	}

	var missing []string
	for _, line := range lines {
		if key := line[:strings.Index(line, ":")+1]; !found[key] {
			missing = append(missing, line)
		}
	}

	out := strings.Join(split, nl)
	if len(missing) > 0 {
		if !strings.HasSuffix(out, nl) {
			out += nl
		}
		out += strings.Join(missing, nl) + nl
	}
	return out
}
