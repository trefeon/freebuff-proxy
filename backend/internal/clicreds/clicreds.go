// Package clicreds reads FreeBuff credentials from the official CLI login
// files. It is a small, standalone helper so the bottom-layer config package
// can accept a discovery function without importing product-specific file
// formats (issue #283).
package clicreds

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DiscoverToken auto-discovers the CLI credentials the same way the official
// FreeBuff CLI stores them: ~/.config/manicode/credentials.json first, then
// ~/.config/codebuff/credentials.json. It returns the auth token, the
// account email, the source file path, and whether a token was found. The
// boolean is false on any failure (missing home, unreadable file, malformed
// JSON, no authToken), so callers can silently fall back to bridge mode.
func DiscoverToken() (token, email, path string, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", "", "", false
	}
	candidates := []string{
		filepath.Join(home, ".config", "manicode", "credentials.json"),
		filepath.Join(home, ".config", "codebuff", "credentials.json"),
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// Strip a leading UTF-8 BOM (Windows credential writers can add one)
		// or json.Unmarshal fails and auto-discovery silently skips the file.
		data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
		var parsed map[string]any
		if err := json.Unmarshal(data, &parsed); err != nil {
			continue
		}
		acct, _ := parsed["default"].(map[string]any)
		if acct == nil {
			for _, v := range parsed {
				if m, ok := v.(map[string]any); ok && m["authToken"] != nil {
					acct = m
					break
				}
			}
		}
		if acct != nil {
			rawToken, _ := acct["authToken"].(string)
			rawEmail, _ := acct["email"].(string)
			rawToken = strings.TrimSpace(rawToken)
			if rawToken != "" {
				return rawToken, rawEmail, p, true
			}
		}
	}
	return "", "", "", false
}
