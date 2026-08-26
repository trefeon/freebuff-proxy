package upstream

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
)

// TestChatCompletionsStreamBodySurvives streams three chunks with real
// delays and asserts the whole body reads back. Regression: do() used to
// defer-cancel the request context when the response headers arrived, which
// aborted every streamed body read (observed live: "upstream stream error:
// context canceled" right after a successful upstream 200).
func TestChatCompletionsStreamBodySurvives(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	chunks := []string{
		`{"id":"c0","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"0"},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"1"},"finish_reason":null}]}`,
		`{"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"2"},"finish_reason":null}]}`,
	}
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk))
			flusher.Flush()
			time.Sleep(150 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("stream read failed (request context canceled too early?): %v", err)
	}
	text := string(data)
	for i, want := range []string{`"content":"0"`, `"content":"1"`, `"content":"2"`, "[DONE]"} {
		if !strings.Contains(text, want) {
			t.Errorf("stream missing %q (chunk %d): %s", want, i, text)
		}
	}
}

func TestChatCompletionsEnvelope(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`)

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{
		Model:             "deepseek/deepseek-v4-flash",
		RunID:             "run-abc",
		SessionInstanceID: "inst-1",
	}, body)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()

	headers, bodies := mock.RecordedChatHeaders, mock.RecordedChatBodies
	if len(headers) != 1 || len(bodies) != 1 {
		t.Fatalf("want 1 chat request, got %d / %d", len(headers), len(bodies))
	}
	h := headers[0]
	// #106: the chat POST carries NO x-freebuff-model / x-freebuff-instance-id
	// headers — the model and instance id ride only in the body metadata.
	if got := h.Get("x-freebuff-model"); got != "" {
		t.Errorf("x-freebuff-model = %q on the chat POST, want absent (#106)", got)
	}
	if got := h.Get("x-freebuff-instance-id"); got != "" {
		t.Errorf("x-freebuff-instance-id = %q on the chat POST, want absent (#106)", got)
	}
	if got := h.Get("Authorization"); got != "Bearer tok-a" {
		t.Errorf("Authorization = %q", got)
	}
	if got := h.Get("Accept"); got != "application/json, text/event-stream" {
		t.Errorf("Accept = %q", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
		t.Fatalf("recorded body not JSON: %v", err)
	}
	md, ok := sent["codebuff_metadata"].(map[string]any)
	if !ok {
		t.Fatalf("missing codebuff_metadata in %s", bodies[0])
	}
	if md["run_id"] != "run-abc" {
		t.Errorf("run_id = %v", md["run_id"])
	}
	if md["freebuff_instance_id"] != "inst-1" {
		t.Errorf("freebuff_instance_id = %v", md["freebuff_instance_id"])
	}
	// client_id is the RUN's id (one per run, repeated per call) and keeps
	// the SDK-faithful shape — never the sess:-prefixed form the server
	// fingerprints as a proxy. This call passes none, so it is the fallback
	// draw.
	clientID, _ := md["client_id"].(string)
	if !regexp.MustCompile(`^[a-z0-9]{13}$`).MatchString(clientID) || strings.HasPrefix(clientID, "sess:") {
		t.Errorf("client_id = %q, want a 13-char base36 draw", clientID)
	}
	provider, ok := sent["provider"].(map[string]any)
	if !ok || provider["data_collection"] != "deny" {
		t.Errorf("provider.data_collection not deny: %v", sent["provider"])
	}
	if sent["stream"] != true {
		t.Errorf("stream not forced: %v", sent["stream"])
	}
	stop, ok := sent["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != `"cb_easp"` {
		t.Errorf("stop sentinel not injected (JSON-quoted form): %v", sent["stop"])
	}
	if sent["temperature"] != 0.7 {
		t.Errorf("temperature lost in envelope: %v", sent["temperature"])
	}
	if sent["cost_mode"] != nil {
		// cost_mode lives inside codebuff_metadata only
		t.Errorf("cost_mode leaked to top level: %v", sent["cost_mode"])
	}
}

func TestEnvelopeCostModeAndStopPreserved(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	// cost_mode present
	withMode, err := New("tok", testConfig(mock.URL(), func(c *config.Config) { c.CostMode = "free" }))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := withMode.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r", SessionInstanceID: "i"}, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	var sent map[string]any
	_ = json.Unmarshal([]byte(mock.RecordedChatBodies[0]), &sent)
	md := sent["codebuff_metadata"].(map[string]any)
	if md["cost_mode"] != "free" {
		t.Errorf("cost_mode = %v, want free", md["cost_mode"])
	}

	// cost_mode absent
	noMode, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	rc, err = noMode.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r", SessionInstanceID: "i"}, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	_ = json.Unmarshal([]byte(mock.RecordedChatBodies[1]), &sent)
	md = sent["codebuff_metadata"].(map[string]any)
	if _, present := md["cost_mode"]; present {
		t.Errorf("cost_mode present despite empty config: %v", md)
	}

	// client-supplied stop is preserved
	rc, err = noMode.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r", SessionInstanceID: "i"},
		[]byte(`{"model":"m","stop":["my-stop"]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	_ = json.Unmarshal([]byte(mock.RecordedChatBodies[2]), &sent)
	stop := sent["stop"].([]any)
	if len(stop) != 1 || stop[0] != "my-stop" {
		t.Errorf("client stop overwritten: %v", stop)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"run invalid", 400, `{"error":"runId not found"}`, ErrRunInvalid},
		{"run not running", 400, `{"error":"runId not running"}`, ErrRunInvalid},
		{"session superseded", 400, `{"error":"session_superseded"}`, ErrSessionSuperseded},
		{"session expired", 400, `{"error":"session_expired"}`, ErrSessionInvalid},
		{"update required", 400, `{"error":"freebuff_update_required"}`, ErrSessionInvalid},
		{"auth", 401, `{"error":"unauthorized"}`, ErrAuthRejected},
		{"waiting room 503", 503, `{"error":"waiting_room_queued"}`, ErrWaitingRoom},
		{"waiting room required (428)", 428, `{"error":"waiting_room_required"}`, ErrWaitingRoomRequired},
		{"waiting room required body (any status)", 429, `{"error":"waiting_room_required"}`, ErrWaitingRoomRequired},
		{"generic", 500, `{"error":"boom"}`, &UpstreamError{Status: 500}},
		{"402 out of credits", 402, `{"error":"out of credits"}`, ErrCredits},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.ChatStatus = tc.status
			mock.ChatErrorBody = tc.body

			client, err := New("tok", testConfig(mock.URL(), nil))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
			if err == nil {
				t.Fatal("expected error")
			}
			if _, isUpstream := tc.want.(*UpstreamError); isUpstream {
				var upErr *UpstreamError
				if !errors.As(err, &upErr) {
					t.Fatalf("want UpstreamError, got %v", err)
				}
				if upErr.Status != tc.status {
					t.Fatalf("status = %d, want %d", upErr.Status, tc.status)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("errors.Is(%q) = false, want %v", err, tc.want)
			}
		})
	}
}

func TestTruncationOfLargeErrorBody(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = 500
	mock.ChatErrorBody = strings.Repeat("x", 2000)

	client, _ := New("tok", testConfig(mock.URL(), nil))
	_, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("want UpstreamError, got %v", err)
	}
	if len(upErr.Body) > 503 {
		t.Errorf("body not truncated: %d chars", len(upErr.Body))
	}
	if !strings.HasSuffix(upErr.Body, "...") {
		t.Errorf("truncation marker missing: %q", upErr.Body)
	}
}

func TestWaitingRoomRetryAfterHeader(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":"waiting_room_queued"}`)
	}

	client, _ := New("tok", testConfig(mock.URL(), nil))
	_, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
	var wrErr *WaitingRoomError
	if !errors.As(err, &wrErr) {
		t.Fatalf("want WaitingRoomError, got %v", err)
	}
	if wrErr.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %s, want 7s", wrErr.RetryAfter)
	}
	if !errors.Is(err, ErrWaitingRoom) {
		t.Error("not unwrap-able to ErrWaitingRoom")
	}
}

func TestAbortPropagation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBlocks = true

	client, _ := New("tok", testConfig(mock.URL(), nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		rc, err := client.ChatCompletions(ctx, ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		if err == nil {
			_ = rc.Close()
		}
		done <- err
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ChatCompletions error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ChatCompletions still blocked after cancel")
	}

	deadline := time.Now().Add(2 * time.Second)
	for !mock.AbortDetected.Load() {
		if time.Now().After(deadline) {
			t.Fatal("upstream request was not aborted on client cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEnsureCliSystemMarkerBranches covers the system-marker merge matrix
// (G5): empty messages, already-present marker, non-string content, merge
// into the first system message, and the unshift path.
func TestEnsureCliSystemMarkerBranches(t *testing.T) {
	t.Run("missing messages gets marker-only system", func(t *testing.T) {
		p := map[string]any{}
		ensureCliSystemMarker(p, "base2-free")
		msgs, ok := p["messages"].([]any)
		if !ok || len(msgs) != 1 {
			t.Fatalf("messages = %v, want a single system message", p["messages"])
		}
		sys, ok := msgs[0].(map[string]any)
		if !ok || sys["role"] != "system" || sys["content"] != cliSystemMarker {
			t.Errorf("system message = %v, want role=system with the CLI marker", msgs[0])
		}
	})

	t.Run("empty messages gets marker-only system", func(t *testing.T) {
		p := map[string]any{"messages": []any{}}
		ensureCliSystemMarker(p, "base2-free")
		msgs := p["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("messages = %v, want a single system message", msgs)
		}
		if msgs[0].(map[string]any)["content"] != cliSystemMarker {
			t.Errorf("system content = %v", msgs[0])
		}
	})

	t.Run("marker already present in string is untouched", func(t *testing.T) {
		content := cliSystemMarker + "\n\nextra instructions"
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": content},
			map[string]any{"role": "user", "content": "hi"},
		}}
		ensureCliSystemMarker(p, "base2-free")
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want unchanged length", msgs)
		}
		if got := msgs[0].(map[string]any)["content"]; got != content {
			t.Errorf("system content changed: %v", got)
		}
	})

	t.Run("marker already present in structured parts is untouched", func(t *testing.T) {
		parts := []any{
			map[string]any{"type": "text", "text": cliSystemMarker + " customized"},
		}
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": parts},
			map[string]any{"role": "user", "content": "hi"},
		}}
		ensureCliSystemMarker(p, "base2-free")
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want unchanged length", msgs)
		}
		gotParts, ok := msgs[0].(map[string]any)["content"].([]any)
		if !ok || len(gotParts) != 1 {
			t.Fatalf("structured parts modified: %v", gotParts)
		}
		if gotParts[0].(map[string]any)["text"] != cliSystemMarker+" customized" {
			t.Errorf("structured text modified: %v", gotParts[0])
		}
	})

	t.Run("phrase mid-string in string prepends marker", func(t *testing.T) {
		// #110: the server gate is a TRIMMED PREFIX test at position 0 — a
		// system message that merely mentions the phrase mid-string must NOT
		// suppress the canonical prefix.
		content := "Please act as " + cliSystemMarkerPhrase + " and be concise."
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": content},
			map[string]any{"role": "user", "content": "hi"},
		}}
		ensureCliSystemMarker(p, "base2-free")
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want length 2", msgs)
		}
		got := msgs[0].(map[string]any)["content"].(string)
		if !strings.HasPrefix(got, cliSystemMarker+"\n\n") || !strings.Contains(got, content) {
			t.Errorf("system content = %q, want marker prepended to the mid-string mention", got)
		}
	})

	t.Run("phrase mid-string in structured part prepends marker", func(t *testing.T) {
		parts := []any{
			map[string]any{"type": "text", "text": "Remember: " + cliSystemMarkerPhrase + "."},
		}
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": parts},
		}}
		ensureCliSystemMarker(p, "base2-free")
		msgs := p["messages"].([]any)
		gotParts, ok := msgs[0].(map[string]any)["content"].([]any)
		if !ok || len(gotParts) != 2 {
			t.Fatalf("system parts = %v, want 2 with marker prepended", msgs[0])
		}
		if gotParts[0].(map[string]any)["text"] != cliSystemMarker {
			t.Errorf("marker part = %v, want the CLI marker first", gotParts[0])
		}
	})

	t.Run("structured system content array prepends marker", func(t *testing.T) {
		originalParts := []any{
			map[string]any{"type": "text", "text": "custom instructions"},
			map[string]any{"type": "text", "text": "more instructions"},
		}
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": originalParts},
		}}
		ensureCliSystemMarker(p, "base2-free")
		msgs := p["messages"].([]any)
		parts, ok := msgs[0].(map[string]any)["content"].([]any)
		if !ok || len(parts) != 3 {
			t.Fatalf("system parts = %v, want 3 parts with marker prepended", msgs[0])
		}
		markerPart, ok := parts[0].(map[string]any)
		if !ok || markerPart["type"] != "text" || markerPart["text"] != cliSystemMarker {
			t.Errorf("marker part = %v, want text type with CLI marker", parts[0])
		}
		if parts[1].(map[string]any)["text"] != "custom instructions" || parts[2].(map[string]any)["text"] != "more instructions" {
			t.Errorf("original parts lost: %v", parts)
		}
	})

	t.Run("non-string non-array system content replaced", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": 12345},
		}}
		ensureCliSystemMarker(p, "base2-free")
		msgs := p["messages"].([]any)
		if got := msgs[0].(map[string]any)["content"]; got != cliSystemMarker {
			t.Errorf("system content = %v, want the CLI marker", got)
		}
	})

	t.Run("empty string system content replaced with marker", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": ""},
		}}
		ensureCliSystemMarker(p, "base2-free")
		msgs := p["messages"].([]any)
		if got := msgs[0].(map[string]any)["content"]; got != cliSystemMarker {
			t.Errorf("system content = %v, want the CLI marker", got)
		}
	})

	t.Run("merges into first system message", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "u"},
			map[string]any{"role": "system", "content": "existing"},
		}}
		ensureCliSystemMarker(p, "base2-free")
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want length 2", msgs)
		}
		sys := msgs[1].(map[string]any)
		if sys["role"] != "system" {
			t.Fatalf("second message = %v, want system", sys)
		}
		if got := sys["content"].(string); !strings.HasPrefix(got, cliSystemMarker) || !strings.Contains(got, "existing") {
			t.Errorf("merged content = %q, want marker + existing", got)
		}
	})

	t.Run("unshifts marker before user", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "u"},
		}}
		ensureCliSystemMarker(p, "base2-free")
		msgs := p["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v, want length 2", msgs)
		}
		if msgs[0].(map[string]any)["role"] != "system" {
			t.Errorf("first message = %v, want system", msgs[0])
		}
	})

	// PR #207 routes Luna onto base3-free-luna and vendor cce4800 adds
	// base3-free-ox-alpha: a base3 run must open with the BASE3 canonical
	// identity (agents/base3.ts), not base2's strategic-assistant one.
	t.Run("base3 agent gets base3 opening", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "u"},
		}}
		ensureCliSystemMarker(p, "base3-free-luna")
		msgs := p["messages"].([]any)
		got := msgs[0].(map[string]any)["content"].(string)
		if !strings.HasPrefix(got, cliSystemMarkerBase3) {
			t.Errorf("base3 opening = %q, want prefix %q", got, cliSystemMarkerBase3)
		}
		if strings.Contains(got, "strategic coding assistant") {
			t.Errorf("base3 run leaked the base2 identity: %q", got)
		}
	})

	t.Run("empty agent id keeps base2 marker", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "u"},
		}}
		ensureCliSystemMarker(p, "")
		got := p["messages"].([]any)[0].(map[string]any)["content"].(string)
		if !strings.HasPrefix(got, cliSystemMarkerPhrase) {
			t.Errorf("default opening = %q, want the base2 marker", got)
		}
	})

	// The gate is any-of-five (hasFreebuffRootSystemPromptOpening): a request
	// already opening with ANY canonical identity is left untouched even when
	// it differs from the run's own marker — prepending would corrupt a
	// prompt that already passes the gate.
	for _, opening := range cliSystemGateOpenings {
		t.Run("gate accepts "+opening[:34], func(t *testing.T) {
			p := map[string]any{"messages": []any{
				map[string]any{"role": "system", "content": opening + "\n\nCustom persona."},
				map[string]any{"role": "user", "content": "u"},
			}}
			ensureCliSystemMarker(p, "base2-free")
			sys := p["messages"].([]any)[0].(map[string]any)
			if got := sys["content"].(string); got != opening+"\n\nCustom persona." {
				t.Errorf("content rewritten: %q", got)
			}
		})
	}

	// Mid-string mentions must still NOT suppress the prepend (#110).
	t.Run("mid-string phrase does not suppress", func(t *testing.T) {
		content := "Please act as " + cliSystemMarkerPhrase + " and be concise."
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": content},
		}}
		ensureCliSystemMarker(p, "base2-free")
		got := p["messages"].([]any)[0].(map[string]any)["content"].(string)
		if !strings.HasPrefix(got, cliSystemMarker) {
			t.Errorf("marker not prepended for mid-string mention: %q", got)
		}
	})
}

// TestInjectEnvelopeBranchMatrix covers injectEnvelope's override behavior
// (G5): stream:false is force-overridden, provider is replaced, stop is
// preserved, and a non-object body is rejected.
func TestInjectEnvelopeBranchMatrix(t *testing.T) {
	t.Run("stream false overridden to true", func(t *testing.T) {
		out, err := injectEnvelope([]byte(`{"model":"m","stream":false}`), "free", ChatOptions{RunID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["stream"] != true {
			t.Errorf("stream = %v, want true", payload["stream"])
		}
	})

	t.Run("provider replaced", func(t *testing.T) {
		out, err := injectEnvelope([]byte(`{"model":"m","provider":{"data_collection":"allow"}}`), "free", ChatOptions{RunID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatal(err)
		}
		prov, ok := payload["provider"].(map[string]any)
		if !ok || prov["data_collection"] != "deny" {
			t.Errorf("provider = %v, want data_collection=deny", payload["provider"])
		}
	})

	t.Run("client stop preserved", func(t *testing.T) {
		out, err := injectEnvelope([]byte(`{"model":"m","stop":["custom"]}`), "free", ChatOptions{RunID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatal(err)
		}
		stop, ok := payload["stop"].([]any)
		if !ok || len(stop) != 1 || stop[0] != "custom" {
			t.Errorf("stop = %v, want preserved [custom]", payload["stop"])
		}
	})

	t.Run("no stop adds quoted cb_easp", func(t *testing.T) {
		out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatal(err)
		}
		stop, ok := payload["stop"].([]any)
		if !ok || len(stop) != 1 || stop[0] != `"cb_easp"` {
			t.Errorf(`stop = %v, want ["\"cb_easp\""] (JSON-quoted, agent-runtime constants.ts:3)`, payload["stop"])
		}
	})

	t.Run("non-object body rejected", func(t *testing.T) {
		if _, err := injectEnvelope([]byte(`[1,2,3]`), "free", ChatOptions{RunID: "r"}); err == nil {
			t.Error("injectEnvelope accepted a JSON array body")
		}
	})
}

// TestRequestJitter guards the REQUEST_JITTER gate (G6): the request is held
// before any upstream contact, and canceling during the window aborts with
// context.Canceled and no upstream hit.
func TestRequestJitter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := New("tok", testConfig(mock.URL(), func(c *config.Config) {
		c.RequestJitter = time.Hour
	}))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.ChatCompletions(ctx, ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		done <- err
	}()

	// The jitter gate must hold the request before any upstream contact.
	time.Sleep(50 * time.Millisecond)
	if n := mock.Requests; n != 0 {
		t.Fatalf("upstream hit %d times during the jitter window, want 0", n)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ChatCompletions did not abort on cancel during jitter")
	}
	if n := mock.Requests; n != 0 {
		t.Fatalf("upstream hit %d times after cancel, want 0", n)
	}

	t.Run("small jitter still completes", func(t *testing.T) {
		mock2 := testutil.NewMock()
		defer mock2.Close()
		mock2.ChatBody = testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`)
		client2, err := New("tok", testConfig(mock2.URL(), func(c *config.Config) {
			c.RequestJitter = 30 * time.Millisecond
		}))
		if err != nil {
			t.Fatal(err)
		}
		rc, err := client2.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatalf("chat with jitter failed: %v", err)
		}
		_ = rc.Close()
		if mock2.Requests != 1 {
			t.Errorf("Requests = %d, want 1", mock2.Requests)
		}
	})
}

// TestClassify429ChatLevel guards 429 chat-level bodies (G10): ip_capped
// classifies as the distinct IpCappedError (admission-only, NOT a quota
// reset — never ErrRateLimited), while spend_limited keeps quota-lock
// RateLimitError semantics.
func TestClassify429ChatLevel(t *testing.T) {
	t.Run("ip_capped", func(t *testing.T) {
		err := classifyError(http.StatusTooManyRequests,
			`{"status":"ip_capped","activeUsersForIp":5,"limit":4,"retryAfterMs":30000}`, http.Header{})
		if errors.Is(err, ErrRateLimited) {
			t.Fatal("ip_capped classified as ErrRateLimited, want distinct ErrIpCapped")
		}
		var ice *IpCappedError
		if !errors.As(err, &ice) {
			t.Fatalf("err = %v, want *IpCappedError", err)
		}
		if !errors.Is(err, ErrIpCapped) {
			t.Errorf("err = %v, want ErrIpCapped", err)
		}
		if ice.ActiveUsersForIP != 5 || ice.Limit != 4 {
			t.Errorf("IpCappedError = %+v, want ActiveUsersForIP 5 limit 4", ice)
		}
		if ice.RetryAfter != 30*time.Second {
			t.Errorf("RetryAfter = %v, want 30s (bounded to retryAfterMs only)", ice.RetryAfter)
		}
	})
	t.Run("spend_limited", func(t *testing.T) {
		err := classifyError(http.StatusTooManyRequests,
			`{"status":"spend_limited","message":"Daily budget reached","retryAfterMs":60000}`, http.Header{})
		var rle *RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("err = %v, want RateLimitError", err)
		}
		if !errors.Is(err, ErrRateLimited) {
			t.Errorf("err = %v, want ErrRateLimited", err)
		}
		if rle.Status != "spend_limited" {
			t.Errorf("RateLimitError.Status = %q, want spend_limited", rle.Status)
		}
	})
}

// TestChatNonObjectBodyAndGzipError guards G12: a non-object chat body is
// rejected at the envelope stage, and a gzip-compressed 4xx error body is
// drained and decompressed before classification.
func TestChatNonObjectBodyAndGzipError(t *testing.T) {
	t.Run("non-object body rejected", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, []byte(`[1,2,3]`))
		if err == nil {
			t.Fatal("array chat body accepted, want envelope error")
		}
		if !strings.Contains(err.Error(), "envelope") {
			t.Errorf("err = %v, want an envelope error", err)
		}
		if mock.Requests != 0 {
			t.Errorf("upstream hit %d times for a rejected body, want 0", mock.Requests)
		}
	})

	t.Run("gzip 4xx body decompressed before classify", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusTooManyRequests)
			zw := gzip.NewWriter(w)
			_, _ = zw.Write([]byte(`{"status":"rate_limited","retryAfterMs":60000}`))
			_ = zw.Close()
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, []byte(`{"model":"m"}`))
		if !errors.Is(err, ErrRateLimited) {
			t.Errorf("err = %v, want ErrRateLimited (gzip body must be decompressed before classification)", err)
		}
	})
}

// TestFullChatLifecycleChained is E2E flow 7: create session, start run,
// chat (with instance-id + envelope), finish run, end session — in one
// chain, asserting the instance/run ids thread through.
func TestFullChatLifecycleChained(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lifecycle"},"finish_reason":null}]}`)

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	st, err := client.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if st.Status != "active" || st.InstanceID == "" {
		t.Fatalf("session = %+v, want active with an instance id", st)
	}

	runID, err := client.StartRun(ctx, "agent-1")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if runID == "" {
		t.Fatal("StartRun returned an empty run id")
	}

	rc, err := client.ChatCompletions(ctx, ChatOptions{Model: "m", RunID: runID, SessionInstanceID: st.InstanceID},
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(data), `"content":"lifecycle"`) {
		t.Errorf("stream missing chunk: %s", data)
	}
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("recorded chat headers = %d, want 1", len(mock.RecordedChatHeaders))
	}
	// #106: the chat POST carries no instance/model headers — they ride in
	// the body metadata only.
	if got := mock.RecordedChatHeaders[0].Get("x-freebuff-instance-id"); got != "" {
		t.Errorf("chat x-freebuff-instance-id = %q, want absent (#106)", got)
	}
	if got := mock.RecordedChatHeaders[0].Get("x-freebuff-model"); got != "" {
		t.Errorf("chat x-freebuff-model = %q, want absent (#106)", got)
	}
	if !mock.BodyContains(`"freebuff_instance_id":"` + st.InstanceID + `"`) {
		t.Error("chat body missing freebuff_instance_id in codebuff_metadata")
	}
	if !mock.BodyContains(`"run_id":"` + runID + `"`) {
		t.Error("chat body missing run_id in codebuff_metadata")
	}

	if err := client.FinishRun(ctx, runID, "completed", 3, nil, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if err := client.EndSession(ctx); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	if mock.SessionCreates != 1 || mock.SessionEnds != 1 {
		t.Errorf("session creates/ends = %d/%d, want 1/1", mock.SessionCreates, mock.SessionEnds)
	}
	if got := mock.StartedRunsSnapshot(); len(got) != 1 || got[0] != "agent-1" {
		t.Errorf("started runs = %v, want [agent-1]", got)
	}
	finished := mock.FinishedRunsSnapshot()
	if len(finished) != 1 || finished[0].RunID != runID || finished[0].TotalSteps != 3 {
		t.Errorf("finished runs = %+v, want run %s with 3 steps", finished, runID)
	}
}

// TestChatSendsActingUserID verifies #79: when ACTING_USER_ID is configured
// the client sends x-freebuff-acting-user-id on the chat path (the CLI
// sends the account's own id derived from /api/v1/me); when unset the
// header is omitted.
func TestChatSendsActingUserID(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`)

	t.Run("set", func(t *testing.T) {
		client, err := New("tok-a", testConfig(mock.URL(), func(c *config.Config) { c.ActingUserID = "user-123" }))
		if err != nil {
			t.Fatal(err)
		}
		rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = rc.Close()
		if len(mock.RecordedChatHeaders) != 1 {
			t.Fatalf("want 1 chat request, got %d", len(mock.RecordedChatHeaders))
		}
		if got := mock.RecordedChatHeaders[0].Get("x-freebuff-acting-user-id"); got != "user-123" {
			t.Errorf("x-freebuff-acting-user-id = %q, want user-123", got)
		}
	})
	t.Run("unset omits header", func(t *testing.T) {
		client, err := New("tok-a", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = rc.Close()
		if got := mock.RecordedChatHeaders[1].Get("x-freebuff-acting-user-id"); got != "" {
			t.Errorf("x-freebuff-acting-user-id = %q, want unset", got)
		}
	})
}

// TestWaitingRoomChainWireFidelity verifies #124: the pre-session ad chain
// matches the CLI wire shape — header UA Freebuff-CLI/1.0.0 (never the
// old 2.0.42 login UA), body userAgent = the Chrome-124 browser UA,
// device carries the host IANA timezone/locale, messages stays [] with no
// sessionId (fresh waiting-room), and the streak GET carries newRequest's
// bunUserAgent (the real CLI's request() sets no override → Bun default).
func TestWaitingRoomChainWireFidelity(t *testing.T) {
	var mu sync.Mutex
	var adsHeaders, streakHeaders http.Header
	var adsBody map[string]any
	adsHits, streakHits := 0, 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/ads" && r.Method == http.MethodPost:
			adsHits++
			adsHeaders = r.Header.Clone()
			_ = json.NewDecoder(r.Body).Decode(&adsBody)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ads":[],"provider":"gravity"}`)
		case r.URL.Path == "/api/v1/freebuff/streak" && r.Method == http.MethodGet:
			streakHits++
			streakHeaders = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client, err := New("tok-a", testConfig(ts.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	client.FireWaitingRoomChain(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if adsHits == 0 {
		t.Fatal("ads request not fired")
	}
	if streakHits == 0 {
		t.Fatal("streak request not fired")
	}
	// Header UA: Freebuff-CLI/<installed binary version>.
	if got := adsHeaders.Get("User-Agent"); got != freebuffCliUA {
		t.Errorf("ads header User-Agent = %q, want %q", got, freebuffCliUA)
	}
	// Body userAgent: the platform-consistent Chrome-124 browser UA (ad
	// targeting) — must agree with the device block's os.
	if got := adsBody["userAgent"]; got != adBrowserUserAgent() {
		t.Errorf("ads body userAgent = %q, want %q", got, adBrowserUserAgent())
	}
	// Device block: host-derived IANA tz/locale, not hardcoded UTC/en-US.
	device, ok := adsBody["device"].(map[string]any)
	if !ok {
		t.Fatalf("ads body device = %T, want object", adsBody["device"])
	}
	tz, _ := device["timezone"].(string)
	if tz == "" || tz == "Local" {
		t.Errorf("ads device timezone = %q, want host IANA name or UTC", tz)
	} else if _, err := time.LoadLocation(tz); err != nil {
		t.Errorf("ads device timezone %q is not a valid IANA zone", tz)
	}
	loc, _ := device["locale"].(string)
	if loc == "" || loc == "C" || loc == "POSIX" || strings.Contains(loc, "_") {
		t.Errorf("ads device locale = %q, want a BCP-47-style locale (e.g. en-US)", loc)
	}
	// The device os follows the host's wire mapping (darwin→macos) and the
	// body UA agrees with it — the CLI picks both from the same platform.
	if os, _ := device["os"].(string); os != deviceOS() {
		t.Errorf("ads device os = %q, want %q (host wire mapping)", os, deviceOS())
	}
	// Faithful details kept: empty messages and NO sessionId (the chain
	// fires before a session exists).
	if msgs, _ := adsBody["messages"].([]any); len(msgs) != 0 {
		t.Errorf("ads body messages = %v, want []", msgs)
	}
	if _, hasSession := adsBody["sessionId"]; hasSession {
		t.Error("ads body carries sessionId, want omitted (fresh waiting-room)")
	}
	// Streak GET: no UA override — it inherits newRequest's bunUserAgent
	// (plain Bun fetch traffic, audit G5).
	if got := streakHeaders.Get("User-Agent"); got != bunUserAgent {
		t.Errorf("streak User-Agent = %q, want %q (bunUserAgent, no override)", got, bunUserAgent)
	}
}

// TestInjectEnvelopeTraceSessionIDAndFreshClientID verifies #80+#103: the
// envelope injects trace_session_id when carried by ChatOptions (stable per
// run) while client_id is a FRESH random draw per call (never derived from
// the run id).
func TestInjectEnvelopeTraceSessionIDAndFreshClientID(t *testing.T) {
	out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "run-1", TraceSessionID: "trace-abc"})
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	md := sent["codebuff_metadata"].(map[string]any)
	if md["trace_session_id"] != "trace-abc" {
		t.Errorf("trace_session_id = %v, want trace-abc", md["trace_session_id"])
	}
	if id, _ := md["client_id"].(string); !regexp.MustCompile(`^[a-z0-9]{13}$`).MatchString(id) || strings.HasPrefix(id, "run:") {
		t.Errorf("client_id = %v, want a fresh unprefixed 13-char base36 draw (#103)", md["client_id"])
	}
	// Re-injecting the same run yields a DIFFERENT client_id across calls.
	out2, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	var sent2 map[string]any
	_ = json.Unmarshal(out2, &sent2)
	if md2 := sent2["codebuff_metadata"].(map[string]any); md2["client_id"] == md["client_id"] {
		t.Errorf("client_id = %v, want a fresh draw per request (same run)", md2["client_id"])
	}
	// Without a run id the SDK-faithful 13-char base36 draw is kept.
	out3, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var sent3 map[string]any
	_ = json.Unmarshal(out3, &sent3)
	md3 := sent3["codebuff_metadata"].(map[string]any)
	if id, _ := md3["client_id"].(string); !regexp.MustCompile(`^[0-9a-z]{13}$`).MatchString(id) {
		t.Errorf("client_id %q not 13-char base36 when no run id", id)
	}
}

func TestDeviceOSWireContract(t *testing.T) {
	tests := []struct {
		goos string
		want string
	}{
		{"darwin", "macos"}, // Go reports darwin, wire contract wants macos
		{"windows", "windows"},
		{"linux", "linux"},
		{"freebsd", "linux"}, // CLI falls back to linux for unknown platforms
		{"", "linux"},
	}
	for _, tt := range tests {
		if got := deviceOSFor(tt.goos); got != tt.want {
			t.Errorf("deviceOSFor(%q) = %q, want %q", tt.goos, got, tt.want)
		}
	}
}
