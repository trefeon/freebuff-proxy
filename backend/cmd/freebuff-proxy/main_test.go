package main

import (
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/egress"
)

// TestEgressCacheGetSet guards the per-egress result cache: Set stores the
// latest result, Get returns it, keys stay independent, and missing keys
// report absent.
func TestEgressCacheGetSet(t *testing.T) {
	c := egress.NewCacheWithTTL(time.Minute)
	if _, ok := c.Get("direct"); ok {
		t.Fatal("empty cache returned a result for direct")
	}
	want := egress.Result{IP: "1.2.3.4", Country: "US"}
	c.Set("direct", want)
	got, ok := c.Get("direct")
	if !ok {
		t.Fatal("cached direct result not found")
	}
	if got != want {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
	// Overwrite replaces the previous result.
	latest := egress.Result{IP: "5.6.7.8", Country: "DE"}
	c.Set("direct", latest)
	if got, _ := c.Get("direct"); got != latest {
		t.Errorf("Get after overwrite = %+v, want %+v", got, latest)
	}
	// Keys are independent.
	if _, ok := c.Get("proxy-0"); ok {
		t.Error("proxy-0 returned a result that was never set")
	}
}

// TestEgressCacheTTL guards the freshness window: an entry must not be
// returned after its TTL elapses, and a re-Set refreshes the timestamp.
func TestEgressCacheTTL(t *testing.T) {
	c := egress.NewCacheWithTTL(50 * time.Millisecond)
	c.Set("direct", egress.Result{IP: "1.2.3.4", Country: "US"})
	if _, ok := c.Get("direct"); !ok {
		t.Fatal("fresh entry not returned")
	}
	time.Sleep(80 * time.Millisecond)
	if _, ok := c.Get("direct"); ok {
		t.Error("expired entry still returned")
	}
	c.Set("direct", egress.Result{IP: "1.2.3.4", Country: "US"})
	if _, ok := c.Get("direct"); !ok {
		t.Error("re-Set entry not returned")
	}
}

// TestVersionFlagPrintsVersion re-executes the test binary with -version
// (main() os.Exit's, so it cannot run in-process) and pins the output:
// "freebuff-proxy <version>" on stdout, exit 0.
func TestVersionFlagPrintsVersion(t *testing.T) {
	if os.Getenv("GO_WANT_VERSION_HELPER") == "1" {
		// Re-executed: the test framework already consumed -test.* flags on
		// the global flag set, so swap in a fresh set before running main.
		flag.CommandLine = flag.NewFlagSet("freebuff-proxy", flag.ExitOnError)
		os.Args = []string{"freebuff-proxy", "-version"}
		main()
		return // unreachable: main os.Exit(0)s
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestVersionFlagPrintsVersion$")
	cmd.Env = append(os.Environ(), "GO_WANT_VERSION_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper exited with error: %v\n%s", err, out)
	}
	if want := "freebuff-proxy " + version; !strings.Contains(string(out), want) {
		t.Errorf("output %q missing %q", out, want)
	}
}
