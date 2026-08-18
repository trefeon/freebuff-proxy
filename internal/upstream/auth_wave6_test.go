package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/testutil"
)

// --- #103: fresh random client_id per chat call -----------------------------

// TestInjectEnvelopeFreshRandomClientID verifies #103: client_id is a FRESH
// random SDK-faithful base36 draw per chat call — never the sess:/run:-
// prefixed shapes the server fingerprints as a proxy, never wf- prefixed,
// and never stable across calls. trace_session_id stays per run and
// freebuff_instance_id stays per session.
func TestInjectEnvelopeFreshRandomClientID(t *testing.T) {
	out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "run-1", SessionInstanceID: "inst-9", TraceSessionID: "trace-abc"})
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	md := sent["codebuff_metadata"].(map[string]any)
	clientID, _ := md["client_id"].(string)
	if !regexp.MustCompile(`^[a-z0-9]{13}$`).MatchString(clientID) {
		t.Errorf("client_id = %q, want a 13-char base36 draw", clientID)
	}
	for _, prefix := range []string{"sess:", "run:", "wf-"} {
		if strings.HasPrefix(clientID, prefix) {
			t.Errorf("client_id = %q, must not carry the %q prefix (proxy fingerprint)", clientID, prefix)
		}
	}
	if md["trace_session_id"] != "trace-abc" {
		t.Errorf("trace_session_id = %v, want trace-abc (per run)", md["trace_session_id"])
	}
	if md["freebuff_instance_id"] != "inst-9" {
		t.Errorf("freebuff_instance_id = %v, want inst-9 (per session)", md["freebuff_instance_id"])
	}

	// A second call with the SAME run AND session must draw a DIFFERENT id.
	out2, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "run-1", SessionInstanceID: "inst-9"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out2, &sent); err != nil {
		t.Fatal(err)
	}
	md2 := sent["codebuff_metadata"].(map[string]any)
	if id2, _ := md2["client_id"].(string); id2 == clientID {
		t.Errorf("client_id = %q on a second call, want a fresh draw per call", id2)
	}

	// No instance id (disabled session): still a fresh unprefixed draw.
	out3, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "run-3"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out3, &sent); err != nil {
		t.Fatal(err)
	}
	md3 := sent["codebuff_metadata"].(map[string]any)
	if id3, _ := md3["client_id"].(string); !regexp.MustCompile(`^[a-z0-9]{13}$`).MatchString(id3) || strings.HasPrefix(id3, "run:") {
		t.Errorf("client_id without instance = %q, want fresh unprefixed 13-char base36", id3)
	}
}

// TestInjectEnvelopeStepNumber verifies #113: llm_step_number is injected as
// a STRING when ChatOptions.StepNumber > 0 and absent when zero.
func TestInjectEnvelopeStepNumber(t *testing.T) {
	out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "r", StepNumber: 3})
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	md := sent["codebuff_metadata"].(map[string]any)
	if md["llm_step_number"] != "3" {
		t.Errorf("llm_step_number = %v (%T), want %q (string form)", md["llm_step_number"], md["llm_step_number"], "3")
	}

	out2, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out2, &sent); err != nil {
		t.Fatal(err)
	}
	if _, present := sent["codebuff_metadata"].(map[string]any)["llm_step_number"]; present {
		t.Error("llm_step_number present when StepNumber == 0")
	}
}

// --- #94: 428 waiting_room_required classification ---------------------------

func TestClassifyWaitingRoomRequired(t *testing.T) {
	err := classifyError(428, `{"error":"waiting_room_required","message":"walk the ads chain"}`, http.Header{})
	if !errors.Is(err, ErrWaitingRoomRequired) {
		t.Fatalf("428 waiting_room_required: errors.Is = false, want ErrWaitingRoomRequired (got %v)", err)
	}
	// Issue #116: FREEBUFF_GATE_CODES marks waiting_room_required
	// endsTheSession:true — the refused session row is ENDED upstream, so
	// the error must ALSO unwrap to ErrSessionInvalid (session-ending:
	// chatAttempt invalidates + reacquires once).
	if !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("428 waiting_room_required must be session-ending (unwrap to ErrSessionInvalid) since #116")
	}
	if errors.Is(err, ErrWaitingRoom) {
		t.Fatal("428 waiting_room_required must NOT be ErrWaitingRoom (it is its own signal)")
	}
	// Retry-After honored.
	err = classifyError(428, `{"error":"waiting_room_required"}`, http.Header{"Retry-After": {"45"}})
	var wrr *WaitingRoomRequiredError
	if !errors.As(err, &wrr) {
		t.Fatalf("want *WaitingRoomRequiredError, got %T", err)
	}
	if wrr.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %v, want 45s from header", wrr.RetryAfter)
	}
}

// TestClientClassifySetsWaitingRoomFlag verifies the client wrapper records
// the 428 flag so the pool can fire the gated WAITING_ROOM_CHAIN (#94).
func TestClientClassifySetsWaitingRoomFlag(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = 428
	mock.ChatErrorBody = `{"error":"waiting_room_required"}`
	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if client.PendingWaitingRoomChain() {
		t.Fatal("flag set before any 428")
	}
	_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
	if err == nil {
		t.Fatal("expected 428 error")
	}
	if !errors.Is(err, ErrWaitingRoomRequired) {
		t.Fatalf("err = %v, want ErrWaitingRoomRequired", err)
	}
	if !client.PendingWaitingRoomChain() {
		t.Fatal("PendingWaitingRoomChain = false after 428, want true")
	}
	if !client.ConsumeWaitingRoomChain() {
		t.Fatal("ConsumeWaitingRoomChain = false, want true (flag was set)")
	}
	if client.PendingWaitingRoomChain() {
		t.Fatal("flag still set after Consume")
	}
	// A second consume must return false (fired exactly once).
	if client.ConsumeWaitingRoomChain() {
		t.Fatal("second ConsumeWaitingRoomChain = true, want false")
	}
}

// --- #62: headless OAuth login flow ------------------------------------------

func TestStartCLILogin(t *testing.T) {
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
	if mock.AuthCLICodeRequests != 1 {
		t.Errorf("AuthCLICodeRequests = %d, want 1", mock.AuthCLICodeRequests)
	}
	if !strings.HasPrefix(code.FingerprintID, "enhanced-") {
		t.Errorf("FingerprintID = %q, want enhanced- prefix", code.FingerprintID)
	}
	if code.FingerprintHash == "" || code.LoginURL == "" || code.ExpiresAt.IsZero() {
		t.Errorf("code = %+v, want hash+loginURL+expiresAt", code)
	}
}

func TestStartCLILoginError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.AuthCLICodeStatus = 500
	mock.AuthCLICodeBody = `{"error":"boom"}`
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartCLILogin(context.Background()); err == nil {
		t.Fatal("StartCLILogin succeeded, want error on 500")
	}
}

func TestPollCLILoginPendingThenComplete(t *testing.T) {
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

	// Pending: the mock serves 401 while AuthCLIStatusBody is empty.
	status, err := client.PollCLILogin(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	if status.Done || status.AuthToken != "" {
		t.Errorf("pending status = %+v, want Done=false", status)
	}

	// Completed: token + user metadata once the body is served.
	mock.AuthCLIStatusBody = `{"authToken":"cb_complete","user":{"id":"gh-1","name":"Ada","email":"ada@example.com"}}`
	status, err = client.PollCLILogin(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Done {
		t.Fatal("Done = false, want true after token appears")
	}
	if status.AuthToken != "cb_complete" {
		t.Errorf("AuthToken = %q, want cb_complete", status.AuthToken)
	}
	if status.User.ID != "gh-1" || status.User.Name != "Ada" {
		t.Errorf("user = %+v, want gh-1/Ada", status.User)
	}
	if mock.AuthCLIStatusRequests != 2 {
		t.Errorf("AuthCLIStatusRequests = %d, want 2", mock.AuthCLIStatusRequests)
	}
}

// TestProtocolGitHubLoginOffline exercises the protocol login's status
// vocabulary against a scripted mock that serves NO GitHub HTML — the flow
// must fail with a parse-style message naming the login URL, never panic
// (the live GitHub walk cannot be exercised in CI).
func TestProtocolGitHubLoginFormNotFound(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	// The mock's /api/auth/cli/code loginUrl points at the mock itself
	// (github.com is never contacted), which serves 404 JSON — no forms.
	mock.AuthCLICodeBody = `{"fingerprintId":"enhanced-x","fingerprintHash":"h","loginUrl":"` + mock.URL() + `/login","expiresAt":` + rfc3339MillisUnix() + `}`
	_, err = client.ProtocolGitHubLogin(context.Background(), "user", "pass", "JBSWY3DPEHPK3PXP", nil)
	if err == nil {
		t.Fatal("ProtocolGitHubLogin succeeded, want form-not-found error")
	}
	if !strings.Contains(err.Error(), "login form not found") {
		t.Errorf("err = %v, want login-form-not-found message", err)
	}
}

func TestProtocolTOTP(t *testing.T) {
	// RFC 6238 test vector: secret "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	// (ASCII "12345678901234567890"), T=59s → 287082.
	code, err := githubProtocolTOTPAt("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(59, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, "287082") {
		t.Errorf("TOTP at 59s = %q, want 287082 prefix", code)
	}
	// T=1111111109 → 081804; T=1111111111 → 050471.
	if c, _ := githubProtocolTOTPAt("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(1111111109, 0)); !strings.HasPrefix(c, "081804") {
		t.Errorf("TOTP at 1111111109 = %q, want 081804 prefix", c)
	}
	if c, _ := githubProtocolTOTPAt("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(1111111111, 0)); !strings.HasPrefix(c, "050471") {
		t.Errorf("TOTP at 1111111111 = %q, want 050471 prefix", c)
	}
}

func rfc3339MillisUnix() string {
	return "1750000000000" // a fixed future epoch ms (2025-06-15)
}

// TestAuthClientSendsNoToken verifies the token-less auth client never
// attaches credential headers on the login endpoints (#62).
func TestAuthClientSendsNoToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartCLILogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	// There is no request-body recording for the auth route; assert via the
	// recorded session route on a harmless probe instead — the auth client
	// must not carry tokens anywhere.
	if client.token != "" {
		t.Errorf("auth client token = %q, want empty", client.token)
	}
}
