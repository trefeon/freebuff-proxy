package server

import (
	"bytes"
	"context"
	"encoding/json"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/dashboard"
	"freebuff-proxy/backend/internal/phasetiming"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/upstream"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type smokeRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Token  string `json:"token"` // bridge mode: client token to relay upstream
}

const maxSmokeBytes = 32 << 10

func (s *Server) handleSmoke(w http.ResponseWriter, r *http.Request) {
	// Server-side DEVTOOLS_ENABLED gate: the UI hides the Dev Tools
	// page when the knob is off, but a direct POST must be refused too —
	// the handlers are real upstream consumers.
	cfg := s.cfg.Load()
	if !cfg.DevToolsEnabled {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "Dev tools are disabled — set DEVTOOLS_ENABLED=true to enable the smoke test."})
		return
	}
	var req smokeRequest
	// The dashboard form posts urlencoded model=&prompt=&token=; read those
	// first and only fall back to JSON for programmatic clients (mirrors
	// handleTokenAdd).
	// Cap the body before FormValue: ParseForm would otherwise slurp the
	// entire request into memory before the fallback read applies its own
	// cap. The 8KB bound here governs both the form and the JSON fallback
	// paths (form fields are tiny; a smoke request needs no more).
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var err error
	req.Model = strings.TrimSpace(r.FormValue("model"))
	req.Prompt = strings.TrimSpace(r.FormValue("prompt"))
	req.Token = strings.TrimSpace(r.FormValue("token"))
	if req.Model == "" && req.Prompt == "" && req.Token == "" {
		var body []byte
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to read request: "+err.Error())
			return
		}
		if err = json.Unmarshal(body, &req); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Invalid request JSON: "+err.Error())
			return
		}
	}
	req.Model = strings.TrimSpace(req.Model)
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.Token = strings.TrimSpace(req.Token)
	if req.Model == "" {
		req.Model = probeModel(s.reg)
		if req.Model == "" {
			s.dash.RenderConfigResult(w, r, false, "No models in the registry to test.")
			return
		}
	}
	if req.Prompt == "" {
		req.Prompt = "ping"
	}
	if len(req.Prompt) > 200 {
		s.dash.RenderConfigResult(w, r, false, "Prompt too long (max 200 chars).")
		return
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ctx, phases := phasetiming.WithContext(ctx)

	chatBody := []byte(`{"model":` + strconv.Quote(req.Model) + `,"messages":[{"role":"user","content":` + strconv.Quote(req.Prompt) + `}],"stream":false}`)
	chatOpts := upstream.ChatOptions{Model: req.Model}

	var lease *pool.Lease
	var up io.ReadCloser
	acquireStart := time.Now()
	if cfg.BridgeMode() {
		if req.Token == "" {
			s.dash.RenderConfigResult(w, r, false, "Bridge mode: include a client token in the smoke request.")
			return
		}
		lease, err = s.pool.AcquireBridge(ctx, req.Token, req.Model)
	} else if cfg.HybridBridgeMode() && req.Token != "" {
		// Hybrid: a supplied client token smoke-tests the bridge surface;
		// without one the pooled surface is probed.
		lease, err = s.pool.AcquireBridge(ctx, req.Token, req.Model)
	} else {
		lease, err = s.pool.Acquire(ctx, req.Model)
	}
	phases.Since(phasetiming.AcquireMS, acquireStart)
	if err == nil {
		up, err = s.pool.Chat(ctx, lease, chatOpts, chatBody)
	}
	if err != nil {
		if lease != nil {
			s.pool.LeaseRelease(lease)
		}
		phases.Since(phasetiming.TotalMS, start)
		s.logger.Warn("dashboard smoke test failed", "model", req.Model, "err", err)
		s.dash.RenderConfigResult(w, r, false, "Smoke test failed: "+err.Error())
		return
	}
	defer s.pool.LeaseRelease(lease)
	defer func() { _ = up.Close() }()

	// Read a bounded prefix of the SSE stream for the preview.
	chatStart := time.Now()
	preview, readErr := readBounded(up, maxSmokeBytes)
	phases.Since(phasetiming.UpstreamTTFBMS, chatStart)
	phases.Since(phasetiming.TotalMS, start)
	ms := time.Since(start).Milliseconds()
	if readErr != nil {
		s.dash.RenderConfigResult(w, r, false, "Smoke test: upstream accepted but stream read failed: "+readErr.Error())
		return
	}
	s.dash.RenderSmokeResult(w, r, req.Model, tokenLabel(lease), ms, preview, dashboard.PhaseList(phases.All()))
}

func (s *Server) handlePlaygroundChat(w http.ResponseWriter, r *http.Request) {
	// Server-side DEVTOOLS_ENABLED gate: a direct POST must be
	// refused when the knob is off, matching the hidden UI page.
	if !s.cfg.Load().DevToolsEnabled {
		s.writeJSONError(w, http.StatusNotFound, "dev tools are disabled — set DEVTOOLS_ENABLED=true to enable the playground", "invalid_request_error", "devtools_disabled", 0)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "failed to read request: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	var req struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "request must be a JSON object", "invalid_request_error", "invalid_json", 0)
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Model == "" {
		if m := probeModel(s.reg); m != "" {
			req.Model = m
		} else {
			s.writeJSONError(w, http.StatusBadRequest, "no model specified and no models in the registry", "invalid_request_error", "model_not_found", 0)
			return
		}
	}
	if req.Prompt == "" {
		req.Prompt = "ping"
	}
	// Build a chat-completions request and run it through the real handler
	// (streaming forced, exactly like /v1/chat/completions).
	chatBody := []byte(`{"model":` + strconv.Quote(req.Model) +
		`,"messages":[{"role":"user","content":` + strconv.Quote(req.Prompt) + `}],"stream":true}`)
	playReq := r.Clone(r.Context())
	playReq.Body = io.NopCloser(bytes.NewReader(chatBody))
	playReq.ContentLength = int64(len(chatBody))
	s.handleChat(w, playReq)
}

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if s.authClient == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "login wizard disabled (no upstream auth client)", "server_error", "login_unavailable", 0)
		return
	}
	s.pruneLoginFlows()
	// The dashboard device login defaults to isolated random fingerprints
	// (mirroring gen-freebuff-token.sh: "enhanced-" + base64url(random-32-bytes))
	// so multiple accounts added to a pool are not correlated by a single machine
	// identifier. Passing ?isolated=false requests the stable machine fingerprint.
	var code *upstream.CLILoginCode
	var err error
	if r.URL.Query().Get("isolated") == "false" {
		code, err = s.authClient.StartCLILogin(r.Context())
	} else {
		code, err = s.authClient.StartCLILoginIsolated(r.Context())
	}
	if err != nil {
		s.logger.Warn("login wizard: start failed", "err", err)
		s.writeJSONError(w, http.StatusBadGateway, "failed to start browser login: "+err.Error(), "server_error", "login_start_failed", 0)
		return
	}
	flowID := shortFlowID(code.FingerprintID)
	flow := &loginFlow{ID: flowID, Code: code, Started: time.Now()}
	s.loginMu.Lock()
	s.loginFlows[code.FingerprintID] = flow
	s.loginMu.Unlock()
	s.logger.Info("login wizard: started", "flow", flowID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"flow_id":     flowID,
		"fingerprint": code.FingerprintID, // full id: the status poll key
		"login_url":   code.LoginURL,
		"expires_at":  code.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	s.pruneLoginFlows()
	fp := strings.TrimSpace(r.URL.Query().Get("fingerprint"))
	if fp == "" {
		s.writeJSONError(w, http.StatusBadRequest, "missing fingerprint query param", "invalid_request_error", "bad_request", 0)
		return
	}
	s.loginMu.Lock()
	flow := s.loginFlows[fp]
	s.loginMu.Unlock()
	if flow == nil {
		s.writeJSONError(w, http.StatusNotFound, "login flow not found or expired — start a new one", "invalid_request_error", "login_flow_missing", 0)
		return
	}
	// Read the completion state under the lock: concurrent status polls
	// (second tab) must not both proceed to addTokenPersist —
	// the completing flag is set before the network poll so exactly one
	// goroutine owns the add.
	s.loginMu.Lock()
	done := flow.Done
	completing := flow.Completing
	flow.Completing = true
	s.loginMu.Unlock()
	if done {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "token_index": flow.Index})
		return
	}
	if completing {
		// Another poll is mid-completion; report pending so the client
		// re-polls instead of double-adding.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		return
	}
	status, err := s.authClient.PollCLILogin(r.Context(), flow.Code)
	if err != nil {
		// Transient poll failure: keep the flow alive, report pending. A
		// later poll may retry completion.
		s.loginMu.Lock()
		flow.Completing = false
		s.loginMu.Unlock()
		s.logger.Debug("login wizard: poll failed", "flow", flow.ID, "err", err)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		return
	}
	if !status.Done {
		s.loginMu.Lock()
		flow.Completing = false
		s.loginMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		return
	}
	// Completed: add to the pool + persist to .env (mirrors handleTokenAdd).
	// All completion fields are written under the lock so a concurrent poll
	// observing Done reads a consistent record.
	flow.Done = true
	flow.Token = status.AuthToken
	s.loginMu.Lock()
	s.loginFlows[fp] = flow
	s.loginMu.Unlock()
	index, addErr := s.addTokenPersist(r.Context(), status.AuthToken)
	if addErr != nil {
		flow.Error = addErr.Error()
		s.loginMu.Lock()
		flow.Completing = false
		s.loginMu.Unlock()
		s.logger.Warn("login wizard: token persist failed", "flow", flow.ID, "err", addErr)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": addErr.Error()})
		return
	}
	flow.Index = index
	if index >= 0 {
		s.pool.SetTokenAccountInfo(index, status.User.Email, status.User.ID)
	}
	s.logger.Info("login wizard: completed", "flow", flow.ID, "token_index", index, "user", status.User.Name)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "token_index": index, "user": status.User.Name})
}

func (s *Server) pruneLoginFlows() {
	cutoff := time.Now().Add(-loginFlowTTL)
	s.loginMu.Lock()
	for fp, flow := range s.loginFlows {
		if flow.Started.Before(cutoff) {
			delete(s.loginFlows, fp)
		}
	}
	s.loginMu.Unlock()
}

// applyReloadedConfig propagates a freshly loaded config to every live
// consumer: the atomic config snapshot, the registry, the pool, and the
// per-IP rate limiter. Every config-save/reload path must apply new
// settings through this one method so no consumer is skipped — the
// change-password reload historically dropped the rate limiter.
func (s *Server) applyReloadedConfig(cfg *config.Config) {
	s.cfg.Store(cfg)
	s.reg.SetConfig(cfg)
	s.pool.SetConfig(cfg)
	s.rateLimiter.SetRate(cfg.RateLimitPerIP, cfg.RateLimitBurst)
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("admin reload requested", "remote", remoteHost(r), "path", r.URL.Path)
	// Serialize with the .env writers (config editor, token add/remove,
	// mode switch): the reload re-reads the SAME files those writers mutate,
	// and applying a load raced against a save could store stale config
	// over a just-saved one (disk NEW, memory OLD).
	s.adminSaveMu.Lock()
	defer s.adminSaveMu.Unlock()
	newCfg, err := config.Load(s.configPath)
	if err != nil {
		s.logger.Warn("admin reload failed", "remote", remoteHost(r), "path", r.URL.Path, "err", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to reload config: "+err.Error(), "internal_error", "reload_failed", 0)
		return
	}
	s.applyReloadedConfig(&newCfg)
	s.logger.Info("config reloaded successfully", "remote", remoteHost(r), "path", r.URL.Path,
		"auth_tokens", len(newCfg.AuthTokens), "safe_mode", newCfg.SafeMode)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "ok",
		"message":     "configuration reloaded",
		"auth_tokens": len(newCfg.AuthTokens),
		"safe_mode":   newCfg.SafeMode,
	})
}
