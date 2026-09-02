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
// FreeBuff / Codebuff CLI stores them:
//  1. ~/.config/freebuff/credentials.json (standalone FreeBuff CLI)
//  2. ~/.config/manicode/credentials.json (original Codebuff/FreeBuff CLI location)
//  3. ~/.config/codebuff/credentials.json (Codebuff CLI fallback)
//
// Inside each file, it inspects profiles in precedence:
// "freebuff" -> "default" -> "codebuff" -> any profile map.
// Token fields are checked in precedence: "authToken" -> "sessionToken" -> "token".
func DiscoverToken() (token, email, path string, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", "", "", false
	}
	candidates := []string{
		filepath.Join(home, ".config", "freebuff", "credentials.json"),
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

		// Look for preferred profile keys first
		var acct map[string]any
		for _, profileKey := range []string{"freebuff", "default", "codebuff"} {
			if m, ok := parsed[profileKey].(map[string]any); ok {
				acct = m
				break
			}
		}
		// Fallback: look for any map with a valid token
		if acct == nil {
			for _, v := range parsed {
				if m, ok := v.(map[string]any); ok {
					if extractToken(m) != "" {
						acct = m
						break
					}
				}
			}
		}

		if acct != nil {
			rawToken := extractToken(acct)
			rawEmail, _ := acct["email"].(string)
			if rawToken != "" {
				return rawToken, strings.TrimSpace(rawEmail), p, true
			}
		}
	}
	return "", "", "", false
}

func extractToken(m map[string]any) string {
	for _, key := range []string{"authToken", "sessionToken", "token"} {
		if val, ok := m[key].(string); ok {
			trimmed := strings.TrimSpace(val)
			if trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}
