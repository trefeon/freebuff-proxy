package upstream

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ChatOptions carries the envelope values for a chat completion request.
type ChatOptions struct {
	Model             string
	RunID             string
	SessionInstanceID string // "" when the session is disabled
	// RequestID is the server's per-request correlation id (D1): the
	// access wrapper mints it once and threads it here so the client's
	// do()/retry log lines (upstream ok/error/transient/retry) share the
	// server's req_id. Never sent upstream.
	RequestID string
	// TraceSessionID is the per-run trace id minted once by the run manager
	// (crypto/rand UUID) and reused across the run's requests, mirroring the
	// CLI (run.ts: previousRun?.traceSessionId ?? randomUUID). Injected as
	// codebuff_metadata["trace_session_id"] when set.
	TraceSessionID string
	// StepNumber is the 1-based per-run agent step counter (CLI parity:
	// llm_step_number is merged on every chat call, String(n);
	// reference/freebuff agent-runtime run-agent-step.ts:1175-1177).
	// Injected as codebuff_metadata["llm_step_number"] when > 0; the run
	// manager sets it per chat call at the server construction sites.
	StepNumber int
}

// ChatCompletions POSTs an OpenAI-shaped request to the upstream chat
// endpoint, injecting the CLI envelope, and returns the raw SSE body reader
// on 2xx. On error status it drains (up to 500 chars), classifies, and
// returns a typed error. The returned reader must be closed; closing it
// releases the connection.
func (c *Client) ChatCompletions(ctx context.Context, opts ChatOptions, body []byte) (io.ReadCloser, error) {
	// D1: thread the server's correlation id into the request context so
	// every do()/retry log line for this chat shares the server's req_id.
	if opts.RequestID != "" {
		ctx = withReqID(ctx, opts.RequestID)
	}
	if c.requestJitter > 0 {
		var b [8]byte
		_, _ = cryptoRand.Read(b[:])
		u := binary.BigEndian.Uint64(b[:])
		jitterNano := int64(u % uint64(c.requestJitter))
		timer := time.NewTimer(time.Duration(jitterNano))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}

	enveloped, err := injectEnvelope(body, c.costMode, opts)
	if err != nil {
		return nil, fmt.Errorf("upstream: envelope: %w", err)
	}

	// free_mode_capacity_deferred is the free tier's transient capacity queue:
	// upstream says "your request will be retried automatically" and a
	// same-session retry recovers immediately (empirically common on
	// deepseek-v4-flash). It is retried IN PLACE against the same lease and
	// session (opts are unchanged, so the instance id is reused), bounded by
	// the TRANSIENT_RETRIES budget — never a token cooldown, never a session
	// invalidation (reference/freebuff-proxy-hengxin proxy.js:652-668).
	// capacityDeferredAttempts is the per-request budget: a fresh call starts
	// at zero, so every request gets its own TRANSIENT_RETRIES allowance
	// (review P1 — the client-lifetime atomic only tracks the metric).
	capacityDeferredAttempts := 0
	for {
		req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/chat/completions", enveloped)
		if err != nil {
			return nil, err
		}
		// The streamed response body must stay readable after this call returns,
		// so the request timeout is applied here (not inside do) and released
		// only when the body is closed.
		var cancel context.CancelFunc
		if _, hasDeadline := req.Context().Deadline(); !hasDeadline && c.requestTimeout > 0 {
			reqCtx, cancelFn := context.WithTimeout(req.Context(), c.requestTimeout)
			cancel = cancelFn
			req = req.WithContext(reqCtx)
		}
		req.Header.Set("Accept", "application/json, text/event-stream")
		// Chat is the ONLY path carrying the ai-sdk UA (audit G5): the real
		// CLI pins it on model calls alone; newRequest defaulted this
		// request to the plain Bun fetch UA every other call sends.
		req.Header.Set("User-Agent", cliUserAgent)
		// The chat POST carries NO x-freebuff-model / x-freebuff-instance-id
		// headers (#106): the official CLI sends exactly Authorization + the
		// ai-sdk UA (+ optional acting-user-id) on chat
		// (reference/freebuff model-provider.ts:146-152); the model and
		// instance id ride only in the body metadata (injectEnvelope).
		if c.userID != "" {
			// The official CLI sends x-freebuff-acting-user-id on every
			// chat call with the account's OWN id, derived from
			// GET /api/v1/me (reference/freebuff sdk/src/run.ts:649-658;
			// sdk/src/impl/model-provider.ts:148-153 — agent-runs
			// START/FINISH carry it too, database.ts:318-320/396-398).
			// The server treats it as a trusted server-to-server header
			// honored only when the request authenticates as the FreeBuff
			// Web service account (reference/freebuff
			// common/src/constants/freebuff-models.ts:1180-1183).
			// ACTING_USER_ID is therefore only safe when it equals the
			// token's own account id; any other value impersonates a
			// foreign user (a possible flag).
			req.Header.Set("x-freebuff-acting-user-id", c.userID)
		}
		resp, _, err := c.do(req, 0)
		if err != nil {
			releaseCancel(cancel)
			return nil, err
		}
		if resp.StatusCode >= 400 {
			bodyText := drainBody(resp.Body)
			_ = resp.Body.Close()
			releaseCancel(cancel)
			c.dump("chat", req, resp.StatusCode, bodyText)
			cerr := c.classify(resp.StatusCode, bodyText, resp.Header)
			if isCapacityDeferred(cerr) && capacityDeferredAttempts < c.transientRetriesLimit {
				capacityDeferredAttempts++
				c.capacityDeferredRetries.Add(1) // lifetime metric
				// #105: the free-tier capacity queue asks the client to WAIT
				// before retrying — the AI SDK absorbs the deferral silently,
				// honoring retry-after with a 10s default
				// (reference/freebuff sdk model-provider.ts:41-49,62-81). Sleep
				// the parsed retry-after (floor 10s) so the same-session retry
				// does not re-POST immediately (amplification); ctx
				// cancellation aborts the sleep like every other upstream wait.
				ra := 10 * time.Second
				var cde *CapacityDeferredError
				if errors.As(cerr, &cde) && cde.RetryAfter > 0 {
					ra = cde.RetryAfter
				}
				timer := time.NewTimer(ra)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				}
				continue
			}
			return nil, cerr
		}
		// Callers MUST close the returned body to release the timeout
		// context; abandoning it leaks the timer goroutine until it fires.
		return &cancelBody{ReadCloser: resp.Body, cancel: cancel}, nil
	}
}

const (
	cliSystemMarker       = "You are Buffy, the strategic coding assistant. You are the AI agent behind the product, Freebuff, a tool where users can chat with you to code with AI for free."
	cliSystemMarkerPhrase = "You are Buffy, the strategic coding assistant"
)

// ensureCliSystemMarker guarantees the canonical "You are Buffy…" opening at
// byte position 0 of the first system message (the free-mode gate's trimmed
// prefix test — see the check loop below). It prepends the marker rather
// than replacing, so custom system instructions survive.
func ensureCliSystemMarker(payload map[string]any) {
	rawMsgs, ok := payload["messages"].([]any)
	if !ok || len(rawMsgs) == 0 {
		payload["messages"] = []any{
			map[string]any{"role": "system", "content": cliSystemMarker},
		}
		return
	}

	for _, m := range rawMsgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] == "system" {
			// The server gate is a TRIMMED PREFIX test at position 0
			// (hasFreebuffRootSystemPromptOpening, free-agents.ts:617-645),
			// hardened against the prepend-and-cancel proxy trick: a message
			// that merely mentions the phrase mid-string must NOT suppress
			// the canonical prefix (#110).
			if content, ok := msg["content"].(string); ok && strings.HasPrefix(strings.TrimSpace(content), cliSystemMarkerPhrase) {
				return // already present
			}
			if parts, ok := msg["content"].([]any); ok {
				for _, p := range parts {
					if partMap, ok := p.(map[string]any); ok {
						if txt, ok := partMap["text"].(string); ok && strings.HasPrefix(strings.TrimSpace(txt), cliSystemMarkerPhrase) {
							return // already present
						}
					}
				}
			}
		}
	}

	// Not present. Merge into first system message if exists, else unshift.
	for i, m := range rawMsgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] == "system" {
			if str, ok := msg["content"].(string); ok {
				if str == "" {
					msg["content"] = cliSystemMarker
				} else {
					msg["content"] = cliSystemMarker + "\n\n" + str
				}
			} else if parts, ok := msg["content"].([]any); ok {
				msg["content"] = append([]any{map[string]any{"type": "text", "text": cliSystemMarker}}, parts...)
			} else {
				msg["content"] = cliSystemMarker
			}
			rawMsgs[i] = msg
			payload["messages"] = rawMsgs
			return
		}
	}

	newMsgs := make([]any, 0, len(rawMsgs)+1)
	newMsgs = append(newMsgs, map[string]any{"role": "system", "content": cliSystemMarker})
	newMsgs = append(newMsgs, rawMsgs...)
	payload["messages"] = newMsgs
}

// injectEnvelope merges the CLI fingerprint into the request body without
// disturbing client-supplied fields: codebuff_metadata, provider
// data_collection=deny, stream=true, and the cb_easp stop sentinel when the
// request has no stop of its own.
func injectEnvelope(body []byte, costMode string, opts ChatOptions) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse request body: %w", err)
	}

	ensureCliSystemMarker(payload)

	// client_id is a FRESH random SDK-faithful draw per chat call — never
	// the sess:/run:-prefixed shapes the server fingerprints as a proxy
	// (#103; reference/freebuff run.ts:646
	// Math.random().toString(36).substring(2,15), cf-worker-signals.ts
	// ^wf-[a-z0-9]{8}$). trace_session_id remains per run (minted once by
	// the run manager, reused across the run's requests; run.ts:
	// previousRun?.traceSessionId ?? randomUUID, proxy-freebuff
	// lib/runs.js:43-46) and freebuff_instance_id stays per session.
	metadata := map[string]any{
		"run_id":    opts.RunID,
		"client_id": generateClientID(),
	}
	if opts.TraceSessionID != "" {
		metadata["trace_session_id"] = opts.TraceSessionID
	}
	if opts.SessionInstanceID != "" {
		metadata["freebuff_instance_id"] = opts.SessionInstanceID
	}
	// llm_step_number is the 1-based per-run agent step, String(n) on the
	// wire (#113; reference/freebuff run-agent-step.ts:1175-1177).
	if opts.StepNumber > 0 {
		metadata["llm_step_number"] = strconv.Itoa(opts.StepNumber)
	}
	if costMode != "" {
		metadata["cost_mode"] = costMode
	}
	payload["codebuff_metadata"] = metadata
	payload["provider"] = map[string]any{"data_collection": "deny"}
	payload["stream"] = true
	if _, hasStop := payload["stop"]; !hasStop {
		payload["stop"] = []string{"cb_easp"}
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("re-marshal envelope: %w", err)
	}
	return out, nil
}
