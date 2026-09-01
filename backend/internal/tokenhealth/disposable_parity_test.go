package tokenhealth

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDisposableEmailDomainsParity pins the Go email-domain tables to the
// pinned upstream snapshot (backend/internal/upstream/testdata/
// disposable-email.ts, copied from reference/freebuff common/src/util/
// disposable-email.ts on every sync). The test re-reads the TS, extracts the
// three string-literal arrays, and asserts the parsed sets equal the Go tables
// entry-for-entry. An upstream sync that changes a domain WITHOUT updating the
// Go table fails here (issue #274).
func TestDisposableEmailDomainsParity(t *testing.T) {
	src := readUpstreamTestdata(t, "disposable-email.ts")

	check := func(name string, got domainSet) {
		want := extractDomainArray(t, src, name)
		if !setsEqual(got, want) {
			t.Errorf("%s mismatch:\n  Go-only: %v\n  TS-only: %v",
				name, diffSets(got, want), diffSets(want, got))
		}
	}
	check("DISPOSABLE_EMAIL_DOMAINS", disposableDomains)
	check("MAINSTREAM_PRIVACY_EMAIL_DOMAINS", mainstreamPrivacyDomains)
	check("PRIVACY_RELAY_EMAIL_DOMAINS", privacyRelayDomains)
}

// readUpstreamTestdata reads a pinned snapshot from ../upstream/testdata/
// (relative to this package).
func readUpstreamTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "upstream", "testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v (copy the upstream file into backend/internal/upstream/testdata/)", name, err)
	}
	return string(b)
}

// extractDomainArray parses a `const NAME = [ 'a', 'b', ... ]` block out of
// the TS source, returning the quoted string literals. The array closes on a
// line whose first non-space char is ']'; // line comments are stripped first
// so apostrophes inside them (e.g. "Proton's") are not mistaken for literals.
func extractDomainArray(t *testing.T, src, name string) domainSet {
	t.Helper()
	marker := "const " + name + " = ["
	start := strings.Index(src, marker)
	if start < 0 {
		t.Fatalf("could not find const %s in the upstream snapshot", name)
	}
	open := start + len(marker)
	rest := src[open:]
	end := -1
	for i := range rest {
		if rest[i] == '\n' {
			j := i + 1
			for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t') {
				j++
			}
			if j < len(rest) && rest[j] == ']' {
				end = open + i
				break
			}
		}
	}
	if end < 0 {
		t.Fatalf("could not find the closing ] of const %s", name)
	}

	body := src[open:end]
	var clean strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if ci := strings.Index(line, "//"); ci >= 0 {
			line = line[:ci]
		}
		clean.WriteString(line)
		clean.WriteByte('\n')
	}

	out := make(domainSet)
	lit := regexp.MustCompile(`'([^']+)'`)
	for _, lm := range lit.FindAllStringSubmatch(clean.String(), -1) {
		out[lm[1]] = struct{}{}
	}
	return out
}

// setsEqual reports whether two domain sets are identical.
func setsEqual(x, y domainSet) bool {
	if len(x) != len(y) {
		return false
	}
	for k := range x {
		if _, ok := y[k]; !ok {
			return false
		}
	}
	return true
}

// diffSets returns the set of domains present in x but not y.
func diffSets(x, y domainSet) []string {
	var out []string
	for k := range x {
		if _, ok := y[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}
