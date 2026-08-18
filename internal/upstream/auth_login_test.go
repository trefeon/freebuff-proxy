package upstream

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/internal/testutil"
)

// --- #125: login poll cadence + continue-on-error loop ----------------------

// TestLoginPollIntervalIsFiveSeconds verifies #125: the status poll cadence
// matches the official CLI's login-flow.ts pollLoginStatus intervalMs=5000.
func TestLoginPollIntervalIsFiveSeconds(t *testing.T) {
	if loginPollInterval != 5*time.Second {
		t.Fatalf("loginPollInterval = %v, want 5s", loginPollInterval)
	}
}

// TestLoginUserAgentIsOfficialLLMProvidersVersion verifies #125: the login UA
// matches the vendored @codebuff/llm-providers package version (1.0.0) — the
// only SDK-shaped UA the official tree emits — not a third-party 2.0.x value.
func TestLoginUserAgentIsOfficialLLMProvidersVersion(t *testing.T) {
	const want = "ai-sdk/openai-compatible/1.0.0/codebuff"
	if freebuffLoginUserAgent != want {
		t.Fatalf("freebuffLoginUserAgent = %q, want %q", freebuffLoginUserAgent, want)
	}
}

// TestFreebuffCLIUserAgent verifies #124: the ads/streak request UA matches
// the installed binary version (Freebuff-CLI/0.0.149, use-gravity-ad.ts
// getCliAdRequestUserAgent).
func TestFreebuffCLIUserAgent(t *testing.T) {
	const want = "Freebuff-CLI/0.0.149"
	if freebuffCLIUserAgent != want {
		t.Fatalf("freebuffCLIUserAgent = %q, want %q", freebuffCLIUserAgent, want)
	}
}

// TestPollForCompletionSurvivesTransientErrors verifies #125: pollForCompletion
// keeps polling through a 5xx and a pending 401 instead of aborting, exactly
// like login-flow.ts pollLoginStatus (non-401 non-ok → warn + sleep +
// continue; 401 → silent + continue; only the 5-min cap / caller cancellation
// stops the loop).
func TestPollForCompletionSurvivesTransientErrors(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	code, err := client.StartCLILogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	var polls atomic.Int32
	mock.AuthCLIHandler = func(w http.ResponseWriter, r *http.Request) {
		switch polls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusInternalServerError) // transient 5xx
		case 2:
			w.WriteHeader(http.StatusUnauthorized) // pending (silent)
		default:
			_, _ = w.Write([]byte(`{"authToken":"cb_after_500","user":{"id":"gh-1","name":"Ada","email":"ada@example.com"}}`))
		}
	}

	status, err := client.pollForCompletion(context.Background(), code, time.Millisecond)
	if err != nil {
		t.Fatalf("pollForCompletion aborted on transient errors: %v", err)
	}
	if !status.Done || status.AuthToken != "cb_after_500" {
		t.Fatalf("status = %+v, want completed token cb_after_500", status)
	}
	if got := polls.Load(); got != 3 {
		t.Errorf("status polls = %d, want 3 (500, 401, success)", got)
	}
}

// TestPollForCompletionRespectsCancellation verifies the loop still stops on
// caller cancellation even while a transient error would otherwise continue
// (the CLI's aborted path).
func TestPollForCompletionRespectsCancellation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	code, err := client.StartCLILogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	mock.AuthCLIHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	_, err = client.pollForCompletion(ctx, code, time.Millisecond)
	if err == nil {
		t.Fatal("pollForCompletion succeeded despite cancellation, want context error")
	}
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
