// Package validate implements the -validate-tokens mode: for each configured
// token (or an explicit comma-separated override list) run ONLY non-mutating
// upstream probes (GET /api/v1/me + the zero-cost session probe), print a
// one-row-per-token health report, and exit 0 (all healthy / soft cooldown
// states), 1 (any BANNED, INVALID, or DISPOSABLE-mailbox token) or 2 (config
// error). The pool state, sessions, and AUTH_TOKENS are never touched.
package validate

import (
	"context"
	"fmt"
	"os"
	"strings"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/upstream"
)

// Run drives the -validate-tokens mode for the config at configPath. override
// is the comma-separated list that replaces AUTH_TOKENS ("" uses the
// configured tokens; the main.go dispatcher normalizes the tri-state flag).
func Run(configPath, override string) {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -validate-tokens: config load failed: %v\n", err)
		os.Exit(2)
	}
	tokens := cfg.AuthTokens
	if override != "" {
		tokens = splitTokenOverride(override)
	}
	if len(tokens) == 0 {
		fmt.Fprintln(os.Stderr, "freebuff-proxy: -validate-tokens: no tokens to validate (AUTH_TOKENS empty — bridge mode); pass -validate-tokens=tok1,tok2 to validate specific tokens")
		os.Exit(2)
	}
	rows, err := upstream.ValidateTokens(context.Background(), &cfg, tokens)
	if err != nil {
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -validate-tokens: %v\n", err)
		os.Exit(2)
	}
	upstream.FlagSharedMailboxes(rows)
	fmt.Print(upstream.FormatHealthReport(rows))
	exit := 0
	for _, r := range rows {
		if r.State == upstream.TokenBanned || r.State == upstream.TokenInvalid || r.Risk == upstream.EmailRiskDisposable {
			exit = 1
		}
	}
	os.Exit(exit)
}

// splitTokenOverride splits a -validate-tokens override on commas (or
// newlines), trimming whitespace and dropping duplicates — the same list
// semantics config.Load applies to AUTH_TOKENS.
func splitTokenOverride(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}
