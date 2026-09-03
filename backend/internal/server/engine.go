package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"freebuff-proxy/backend/internal/convert"
	"freebuff-proxy/backend/internal/phasetiming"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
)

// --- Shared completion engine (protocol-neutral) ---
//
// The acquire→upstream→relay core every completion surface runs on:
// chatCore (lease acquisition, bridge routing, phase timing, endpoint log
// lines), chatAttempt (retry-once recovery with session invalidation and
// token cooldowns), and the plain SSE plumbing every relay shares
// (relayReadLoop, lineChunk, keepaliveInterval). Protocol policy does NOT
// live here — each surface's handler, wire translation, stream relay and
// error envelope live in its own file:
//
//	openai.go            /v1/chat/completions handler (+ openai_stream.go relays)
//	responses.go         /v1/responses handler (+ responses_stream.go relays)
//	anthropic.go         /v1/messages handler (+ anthropic_stream.go,
//	                     anthropic_json.go, anthropic_count.go, anthropic_errors.go,
//	                     streamxml_anthropic.go)
//	models.go            /v1/models catalog
//
// The relay plumbing shared by every surface lives in engine_sse.go
// (relayReadLoop, lineChunk, keepaliveInterval, relayStats).
//
// Kindly keep it that way: a protocol-specific branch added here is a
// debugging regression — the entire point of the split is that an Anthropic
// bug never requires reading OpenAI relay code and vice versa.

// relayFunc relays the upstream SSE reader to the client in the endpoint's
// wire format (chat.completion chunks, Responses events, or Anthropic
// events). Implementations set their own headers, flush, and write terminal
// frames. chatStart is when the upstream chat call returned; the first
// relayed chunk records the upstream TTFB phase.
type relayFunc func(ctx context.Context, w http.ResponseWriter, up io.Reader, stats *relayStats, chatStart time.Time)

// timedBackend wraps a chatBackend so the token-acquisition phase is
// recorded into the request's phasetiming accumulator, mirroring the
// acquireTimed closure chatCore used to thread into chatAttempt.
type timedBackend struct {
	chatBackend
	phases *phasetiming.Phases
}

func (b *timedBackend) Acquire(ctx context.Context, model string) (*pool.Lease, error) {
	start := time.Now()
	l, err := b.chatBackend.Acquire(ctx, model)
	b.phases.Since(phasetiming.AcquireMS, start)
	return l, err
}

// fallbackBackend wraps the pooled chatBackend with the issue #100 bounded
// queue-time model fallback: when the requested model's acquire surfaces a
// waiting-room delay of at least fallbackAfter, the SAME token is re-routed
// to the fallback model instead of handing the client a 503. Conservative:
// only when a fallback is configured; the switch is surfaced to the client
// via the response header and the routing log line.
type fallbackBackend struct {
	chatBackend
	p             *pool.Pool
	model         string
	fallbackModel string
	fallbackAfter time.Duration
	fallbackUsed  *bool
	logger        *slog.Logger
}

func (b *fallbackBackend) Acquire(ctx context.Context, model string) (*pool.Lease, error) {
	l, err := b.chatBackend.Acquire(ctx, model)
	if err == nil || errors.Is(err, registry.ErrModelNotFound) {
		return l, err
	}
	var wr *session.WaitingRoomError
	if errors.As(err, &wr) && wr.RetryAfter >= b.fallbackAfter {
		b.logger.Info("model fallback: waiting room exceeds FALLBACK_AFTER_MS; switching model",
			"model", model, "fallback", b.fallbackModel, "retry_after", wr.RetryAfter.String())
		// Drop the queued session caches so the fallback-model acquire can
		// CREATE a fresh session instead of re-surfacing the same waiting
		// room (issue #100).
		if cleared := b.p.ClearQueuedCaches(); cleared > 0 {
			b.logger.Debug("model fallback: cleared queued session caches", "cleared", cleared)
		}
		l2, err2 := b.chatBackend.Acquire(ctx, b.fallbackModel)
		if err2 == nil {
			*b.fallbackUsed = true
		}
		return l2, err2
	}
	return l, err
}

// handleChat is the OpenAI chat-completions entry point: sanitize the
// chatCore is the shared acquire→relay core for every completion-style
// endpoint (chat completions, Responses, Anthropic messages): acquire a
// token lease (bridge routing included), call upstream with
// retry-once recovery, then relay the forced stream to the client through
// relay. kind names the endpoint in request/done log lines.
func (s *Server) chatCore(w http.ResponseWriter, r *http.Request, model string, stream bool, normalized []byte, reasoningEffort, kind string, relay relayFunc) {
	// Issue #140: the tool-name tolerance map. The handlers normalize
	// with NormalizeRequestMapped, which renames mapped client tools to
	// official signature names IN the normalized body; the mapper that maps
	// them BACK is rebuilt here from the client's ORIGINAL body so response
	// relays can restore names the client dispatched on.
	toolMap := convert.NewToolMapper(originalBodyFromContext(r.Context()))
	// D1: the access wrapper minted the request's correlation id; direct
	// handler calls (tests) mint here so it is never empty. The value is
	// threaded into the request context AND into ChatOptions.RequestID so
	// the upstream client's do()/retry lines share it.
	reqID := reqIDFrom(r.Context())
	if reqID == "" {
		reqID = newReqID()
	}
	st := &chatTraceState{reqID: reqID, clientRequestID: clientRequestID(r)}
	ctx, phases := phasetiming.WithContext(context.WithValue(r.Context(), reqIDKey{}, reqID))
	start := time.Now()

	agentID, _ := s.reg.AgentForModel(model)
	reqAttrs := []any{
		"req_id", reqID,
		"model", model,
		"agent", agentID,
		"stream", stream,
		"remote", remoteHost(r),
		"msgs", toolMap.MsgCount(),
		"tools", toolMap.ToolCount(),
	}
	if reasoningEffort != "" {
		reqAttrs = append(reqAttrs, "reasoning_effort", reasoningEffort)
	}
	s.logger.Info(kind+" request", reqAttrs...)
	// Client-side per-IP rate limiting runs in the OUTERMOST request wrapper
	// (Handler): it must cover every /v1/* surface with a single bucket, so
	// this core deliberately does not re-limit — direct handler calls (unit
	// tests) skip the limiter on purpose.
	// Bridge routing: bridge mode relays the client's Authorization header
	// as the upstream token.  No token in bridge → 401 before touching
	// the pool.
	var up io.ReadCloser
	var lease *pool.Lease
	// One request, one snapshot: requireAuth pinned the config it made its
	// pass-through decision with into the request context; chatCore and
	// authorized route from that same view, so a config swap mid-request
	// cannot split the pooled-vs-bridge decision across two configs.
	cfg := cfgSnapshotFrom(r.Context())
	if cfg == nil {
		// No stamped snapshot (direct handler calls in tests): load live.
		cfg = s.cfg.Load()
	}
	fallbackUsed := false
	tok := bearerToken(r)
	bridge := false
	// Hybrid (default when AUTH_TOKENS set): the pool and the bridge share
	// one instance. A credential matching API_KEYS uses the pool; any other
	// credential is relayed upstream as a bridge token. With no API_KEYS
	// configured every request uses the pool (the historic open behavior),
	// and a missing credential is rejected exactly like pure pooled mode.
	switch {
	case cfg.BridgeMode():
		// Bridge: the client token is the only upstream credential.
		bridge = true
		tok = clientToken(r)
	case cfg.HybridBridgeMode():
		provided := clientToken(r)
		if provided == "" && len(cfg.APIKeys) > 0 {
			s.writeClientError(w, r, http.StatusUnauthorized,
				"Authentication required: send a valid API key for pooled access, or your FreeBuff token for bridge mode",
				"missing_bearer_token", 0)
			return
		}
		if provided != "" && len(cfg.APIKeys) > 0 && !s.authorized(cfg, r) {
			bridge = true
			tok = provided
		}
	}
	// Issue #74: refuse new requests fast when (egress, model) is marked
	// unfit — the direct egress cannot serve this model for ~5 min. The
	// pooled path only: bridge clients relay their own token (the client's
	// own account may serve the model on this egress and their session
	// slots are theirs to spend), so the registry never gates them.
	// MarkModelUnfit always stores a LimitedIpError, so lie is non-nil in
	// practice; the bare sentinel keeps the refusal deterministic if it
	// ever is nil.
	if !bridge {
		if until, lie := s.pool.ModelUnfit(model); !until.IsZero() && time.Now().Before(until) {
			phases.Since(phasetiming.TotalMS, start)
			s.logger.Info(kind+" request refused", "model", model, "reason", "model_limited_on_egress", "until", until.Format(time.RFC3339))
			// Never mutate the registry's stored error (SEC-1): concurrent
			// refusals would race on RetryAfter. Surface a per-request
			// shallow copy carrying the computed window.
			refuseErr := upstream.ErrModelIPLimited
			if lie != nil {
				refuseErr = &upstream.LimitedIpError{Model: lie.Model, Body: lie.Body, RetryAfter: time.Until(until)}
			}
			s.traceChat(nil, model, time.Since(start).Milliseconds(), "error", "model_ip_limited", phases.All(), st)
			s.writeError(w, r, refuseErr, model, nil)
			return
		}
	}
	// The chatBackend abstracts the pooled-vs-bridge acquire/chat/invalidate/
	// cooldown/lease hooks (issue #255); the timing and (pooled only) rate
	// fallback wrappers decorate it.
	var err error
	var be chatBackend
	if bridge {
		if tok == "" {
			s.writeClientError(w, r, http.StatusUnauthorized,
				"bridge mode: send your FreeBuff token in Authorization: Bearer <token>, x-api-key, or anthropic-api-key",
				"missing_bearer_token", 0)
			return
		}
		be = bridgeBackend{p: s.pool, token: tok}
	} else {
		be = pooledBackend{p: s.pool}
		// Issue #100: bounded queue-time model fallback. When the request's
		// model has a configured fallback (FALLBACK_MODEL) and the pool
		// surfaces a waiting-room/queue delay of at least FALLBACK_AFTER_MS,
		// re-route the SAME token to the fallback model instead of handing
		// the client a 503 the client would have to wait out. Conservative:
		// only when a fallback is configured; the switch is surfaced to the
		// client via the X-FreeBuff-Fallback-Model response header and in
		// the routing log line.
		fallbackModel := cfg.FallbackModels[model]
		if cfg.FallbackAfter > 0 && fallbackModel != "" && fallbackModel != model {
			be = &fallbackBackend{chatBackend: be, p: s.pool, model: model, fallbackModel: fallbackModel, fallbackAfter: cfg.FallbackAfter, fallbackUsed: &fallbackUsed, logger: s.logger}
		}
	}
	be = &timedBackend{chatBackend: be, phases: phases}
	up, lease, err = s.chatAttempt(ctx, model, normalized, st, be)
	if err != nil {
		phases.Since(phasetiming.TotalMS, start)
		s.traceChat(lease, model, time.Since(start).Milliseconds(), "error", chatErrClass(err), phases.All(), st)
		// Issue #114: a chat that died on a terminal upstream error must
		// not leave its run FINISHing as completed — report it honestly
		// (nil-safe: an acquire failure leaves no lease).
		be.MarkRunFailed(lease)
		s.writeError(w, r, err, model, lease)
		return
	}
	defer func() { _ = up.Close() }()
	// Issue #53: when the downstream client disconnects mid-stream, abandon
	// the lease instead of a plain release — the run is FINISHed through the
	// bounded queue (last-in-flight only) so upstream does not keep an
	// abandoned agent run alive until the 6h rotation. A normal completion
	// releases the lease as before.
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if r.Context().Err() != nil {
			be.LeaseAbandon(lease)
			return
		}
		be.LeaseRelease(lease)
	}
	defer release()

	routingAttrs := []any{
		"req_id", reqID,
		"token", tokenLabel(lease),
		"model", model,
		"agent", lease.AgentID,
		"instance_id", lease.SessionInstanceID,
	}
	if reasoningEffort != "" {
		routingAttrs = append(routingAttrs, "reasoning_effort", reasoningEffort)
	}
	// Issue #164: fallback transparency. x-freebuff-served-model names the
	// model this lease's session/run is actually bound to — requested model
	// when served directly, the re-routed model after any fallback — and is
	// set on every successful response so clients can always tell what
	// served them. x-freebuff-fallback is set ONLY when a fallback fired,
	// with the reason: "quota_exhausted" (pool QUOTA_FALLBACK_MODELS path,
	// after every quota-positive token for the requested model was
	// exhausted) or "queue_timeout" (FALLBACK_AFTER_MS waiting-room
	// re-route, issue #100). The legacy X-FreeBuff-Fallback-Model header
	// (issue #100) is kept for the queue-time path.
	servedModel := lease.Model
	if servedModel == "" {
		servedModel = model
	}
	w.Header().Set("X-FreeBuff-Served-Model", servedModel)
	fallbackReason := lease.FallbackReason
	if fallbackUsed {
		fallbackReason = "queue_timeout"
		// Surface the transparent model switch to the client (issue #100):
		// the streamed response itself is indistinguishable, so the header
		// is the notice.
		w.Header().Set("X-FreeBuff-Fallback-Model", cfg.FallbackModels[model])
		routingAttrs = append(routingAttrs, "fallback", cfg.FallbackModels[model])
	}
	if fallbackReason != "" {
		w.Header().Set("X-FreeBuff-Fallback", fallbackReason)
		routingAttrs = append(routingAttrs, "served_model", servedModel)
	} else if servedModel != model {
		// Issue #230: upstream coercion transparency. When upstream binds the
		// session to a different model (e.g. limited-tier token coerced to mimo),
		// surface served_model in the INFO routing log so operators immediately
		// see the requested->served redirection.
		routingAttrs = append(routingAttrs, "served_model", servedModel)
	}
	s.logger.Info(kind+" routing", routingAttrs...)

	chatStart := time.Now()
	stats := &relayStats{servedModel: servedModel, toolMap: toolMap}
	relay(ctx, w, up, stats, chatStart)
	// Issue #114: record the completed chat as a run step — steps are
	// batched in memory and sent WITH FINISH (the CLI has no /steps
	// endpoint). The response message id is not extracted from the stream;
	// the CLI step schema allows a null messageId.
	be.RecordRunStep(lease, "")
	// Issue #122: feed the per-token spend ledger once per successful chat
	// completion with the usage total observed by the relay (0 when the
	// upstream stream carried none — RecordSpend ignores non-positive).
	be.RecordSpend(lease, stats.usageTokens)
	phases.Since(phasetiming.TotalMS, start)
	ms := time.Since(start).Milliseconds()
	s.logger.Info(kind+" done", chatDoneAttrs(reqID, model, lease.AgentID, stream, ms, stats.chunks, stats.bytes, reasoningEffort)...)
	s.traceChat(lease, model, ms, "ok", "", phases.All(), st)
}
