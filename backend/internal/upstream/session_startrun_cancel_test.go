package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// emptyRunIDRoundTripper answers every agent-runs START with a valid 200 and
// an empty runId, ignoring the request context — so a pre-cancelled caller
// context deterministically reaches the empty-runId branch instead of failing
// in the transport.
type emptyRunIDRoundTripper struct{}

func (emptyRunIDRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Request:    req,
	}, nil
}

// TestStartRunEmptyRunIDWithCancelledCtx pins the run-agent-step.ts @0b7d580c
// port (issue #323): registration ending on the abort signal is a cancel,
// not a failure. A live context still gets the missing-runId failure.
func TestStartRunEmptyRunIDWithCancelledCtx(t *testing.T) {
	client, err := New("tok-cancel", testConfig("https://www.codebuff.com", nil))
	if err != nil {
		t.Fatal(err)
	}
	client.SetTransport(emptyRunIDRoundTripper{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.StartRun(ctx, "agent-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("StartRun with cancelled ctx = %v, want context.Canceled", err)
	}

	if _, err := client.StartRun(context.Background(), "agent-1"); err == nil || errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "missing runId") {
		t.Fatalf("StartRun with live ctx = %v, want missing-runId failure", err)
	}
}
