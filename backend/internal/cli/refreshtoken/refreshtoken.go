// Package refreshtoken implements the -refresh-token mode (issue #66):
// re-authenticate one pooled token in .env via the headless GitHub login
// flow — the same /api/auth/cli/code + /api/auth/cli/status endpoints the
// dashboard wizard (#62) and the CLI use. Interactive: start → print login
// URL → poll → replace token #N in .env atomically. Non-interactive (-yes):
// with GITHUB_USER/GITHUB_PASSWORD/GITHUB_TOTP present attempt the reference
// GitHub password+TOTP protocol login; otherwise print the login URL and
// exit 2 (manual completion needed).
package refreshtoken

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/upstream"
)

// refreshPollTimeout bounds the whole interactive poll (the login code
// itself expires after ~5 minutes).
const refreshPollTimeout = 5 * time.Minute

// envUpdate is one key/value replacement in a .env file (issue #66): the
// key's existing line (KEY=...) is rewritten in place, preserving every
// other line, comment, and the file's overall shape.
type envUpdate struct {
	Key   string
	Value string
}

// updateEnvKeysAt applies updates to the .env at path atomically
// (issue #66: temp file + rename, 0600, matching writeFileAtomic): the
// existing KEY= line is replaced, absent keys are appended. Returns the
// resulting file bytes so callers can print the new list. A missing file
// is an error — callers must ensure the file exists first.
func updateEnvKeysAt(path string, updates []envUpdate) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// Build a lookup of the updates; keys are case-sensitive .env names.
	byKey := make(map[string]string, len(updates))
	for _, u := range updates {
		byKey[u.Key] = u.Value
	}
	lines := strings.Split(string(data), "\n")
	seen := make(map[string]bool, len(byKey))
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:eq])
		if val, ok := byKey[key]; ok && !seen[key] {
			lines[i] = key + "=" + val
			seen[key] = true
		}
	}
	// Append any keys that had no existing line.
	var missing []string
	for _, u := range updates {
		if !seen[u.Key] {
			missing = append(missing, u.Key+"="+u.Value)
			seen[u.Key] = true
		}
	}
	out := strings.Join(lines, "\n")
	if len(missing) > 0 {
		out += "\n" + strings.Join(missing, "\n") + "\n"
	}
	// Atomic write: temp in the same dir + rename (0600, matching the
	// project's .env and session-state file conventions).
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".env-update-*")
	if err != nil {
		return nil, fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.WriteString(out); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return nil, fmt.Errorf("rename onto %s: %w", path, err)
	}
	return []byte(out), nil
}

// Run drives the -refresh-token mode and exits. index is the 0-based
// AUTH_TOKENS position to replace.
func Run(configPath string, index int, autoYes bool) {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freebuff-proxy: invalid config:", err)
		os.Exit(1)
	}
	if index < 0 || index >= len(cfg.AuthTokens) {
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -refresh-token %d is out of range (AUTH_TOKENS has %d token(s), 0-based)\n", index, len(cfg.AuthTokens))
		os.Exit(1)
	}

	client, err := upstream.NewForAuth(&cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freebuff-proxy: build auth client:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), refreshPollTimeout)
	defer cancel()

	code, err := client.StartCLILogin(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freebuff-proxy: start login:", err)
		os.Exit(1)
	}

	// Protocol login when non-interactive with credentials present (#66).
	if autoYes {
		user := strings.TrimSpace(os.Getenv("GITHUB_USER"))
		pass := strings.TrimSpace(os.Getenv("GITHUB_PASSWORD"))
		totp := strings.TrimSpace(os.Getenv("GITHUB_TOTP"))
		if user != "" && pass != "" && totp != "" {
			fmt.Fprintln(os.Stderr, "GITHUB_USER/GITHUB_PASSWORD/GITHUB_TOTP present — attempting the GitHub protocol login (password + TOTP)...")
			status, perr := client.ProtocolGitHubLogin(ctx, user, pass, totp, nil)
			if perr != nil {
				fmt.Fprintln(os.Stderr, "freebuff-proxy: protocol login failed:", perr)
				fmt.Fprintln(os.Stderr, "Open this URL in a browser to complete the login manually:")
				fmt.Fprintln(os.Stderr, "  "+code.LoginURL)
				os.Exit(2)
			}
			persistReplacement(cfg, index, status.AuthToken)
			os.Exit(0)
		}
		// No credentials: needs manual completion — print the URL, exit 2.
		fmt.Fprintln(os.Stderr, "GITHUB_USER/GITHUB_PASSWORD/GITHUB_TOTP are not all set — manual login required.")
		fmt.Fprintln(os.Stderr, "Open this URL in a browser (ideally a private/incognito window):")
		fmt.Fprintln(os.Stderr, "  "+code.LoginURL)
		os.Exit(2)
	}

	// Interactive: print the URL and poll for completion.
	fmt.Printf("\n  Open this URL in your browser to re-authenticate token #%d:\n\n    %s\n\n", index, code.LoginURL)
	fmt.Printf("  Waiting for login (up to %s)...\n", refreshPollTimeout.String())
	for {
		status, err := client.PollCLILogin(ctx, code)
		if err != nil {
			fmt.Fprintln(os.Stderr, "freebuff-proxy: poll login:", err)
			os.Exit(1)
		}
		if status.Done {
			persistReplacement(cfg, index, status.AuthToken)
			fmt.Printf("  Token #%d refreshed (account: %s <%s>).\n", index, status.User.Name, status.User.Email)
			fmt.Println("  A running proxy picks the change up via /admin/reload or a restart.")
			os.Exit(0)
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "freebuff-proxy: login timed out")
			os.Exit(1)
		// 5s matches the CLI's pollLoginStatus intervalMs=5000 (#125; the
		// upstream const loginPollInterval is the same value).
		case <-time.After(5 * time.Second):
		}
	}
}

// persistReplacement rewrites AUTH_TOKENS #index in .env atomically
// (issue #66: writeFileAtomic temp+rename, 0600) and prints the new list.
// Missing .env is an error — the token must come from somewhere.
func persistReplacement(cfg config.Config, index int, newToken string) {
	tokens := append([]string(nil), cfg.AuthTokens...)
	tokens[index] = newToken
	envPath := cfg.EnvFile
	if envPath == "" {
		envPath = filepath.Join(".", ".env")
	}
	updates := []envUpdate{{Key: "AUTH_TOKENS", Value: strings.Join(tokens, ",")}}
	if _, err := updateEnvKeysAt(envPath, updates); err != nil {
		fmt.Fprintln(os.Stderr, "freebuff-proxy: persist AUTH_TOKENS:", err)
		os.Exit(1)
	}
}
