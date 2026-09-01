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

func (a *adminHandlers) handleSmoke(w http.ResponseWriter, r *http.Request) {
	// Server-side DEVTOOLS_ENABLED gate: the UI hides the Dev Tools
	// page when the knob is off, but a direct POST must be refused too —
	// the handlers are real upstream consumers.
	cfg := a.cfgLoad()
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
			a.dash.RenderConfigResult(w, r, false, "Failed to read request: "+err.Error())
			return
		}
		if err = json.Unmarshal(body, &req); err != nil {
			a.dash.RenderConfigResult(w, r, false, "Invalid request JSON: "+err.Error())
			return
		}
	}
	req.Model = strings.TrimSpace(req.Model)
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.Token = strings.TrimSpace(req.Token)
	if req.Model == "" {
		req.Model = probeModel(a.reg)
		if req.Model == "" {
			a.dash.RenderConfigResult(w, r, false, "No models in the registry to test.")
			return
		}
	}
	if req.Prompt == "" {
		req.Prompt = "ping"
	}
	if len(req.Prompt) > 200 {
		a.dash.RenderConfigResult(w, r, false, "Prompt too long (max 200 chars).")
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
			a.dash.RenderConfigResult(w, r, false, "Bridge mode: include a client token in the smoke request.")
			return
		}
		lease, err = a.pool.AcquireBridge(ctx, req.Token, req.Model)
	} else if cfg.HybridBridgeMode() && req.Token != "" {
		// Hybrid: a supplied client token smoke-tests the bridge surface;
		// without one the pooled surface is probed.
		lease, err = a.pool.AcquireBridge(ctx, req.Token, req.Model)
	} else {
		lease, err = a.pool.Acquire(ctx, req.Model)
	}
	phases.Since(phasetiming.AcquireMS, acquireStart)
	if err == nil {
		up, err = a.pool.Chat(ctx, lease, chatOpts, chatBody)
	}
	if err != nil {
		if lease != nil {
			a.pool.LeaseRelease(lease)
		}
		phases.Since(phasetiming.TotalMS, start)
		a.logfunc().Warn("dashboard smoke test failed", "model", req.Model, "err", err)
		a.dash.RenderConfigResult(w, r, false, "Smoke test failed: "+err.Error())
		return
	}
	defer a.pool.LeaseRelease(lease)
	defer func() { _ = up.Close() }()

	// Read a bounded prefix of the SSE stream for the preview.
	chatStart := time.Now()
	preview, readErr := readBounded(up, maxSmokeBytes)
	phases.Since(phasetiming.UpstreamTTFBMS, chatStart)
	phases.Since(phasetiming.TotalMS, start)
	ms := time.Since(start).Milliseconds()
	if readErr != nil {
		a.dash.RenderConfigResult(w, r, false, "Smoke test: upstream accepted but stream read failed: "+readErr.Error())
		return
	}
	a.dash.RenderSmokeResult(w, r, req.Model, tokenLabel(lease), ms, preview, dashboard.PhaseList(phases.All()))
}

func (a *adminHandlers) handlePlaygroundChat(w http.ResponseWriter, r *http.Request) {
	// Server-side DEVTOOLS_ENABLED gate: a direct POST must be
	// refused when the knob is off, matching the hidden UI page.
	if !a.cfgLoad().DevToolsEnabled {
		a.dash.RenderResult(w, http.StatusNotFound, false, "dev tools are disabled — set DEVTOOLS_ENABLED=true to enable the playground", "devtools_disabled")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "failed to read request: "+err.Error(), "invalid_json")
		return
	}
	var req struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "request must be a JSON object", "invalid_json")
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Model == "" {
		if m := probeModel(a.reg); m != "" {
			req.Model = m
		} else {
			a.dash.RenderResult(w, http.StatusBadRequest, false, "no model specified and no models in the registry", "model_not_found")
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
	a.handleChat(w, playReq)
}

func (a *adminHandlers) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if a.authClientFunc == nil || a.authClientFunc() == nil {
		a.dash.RenderResult(w, http.StatusServiceUnavailable, false, "login wizard disabled (no upstream auth client)", "login_unavailable")
		return
	}
	a.pruneLoginFlows()
	// The dashboard device login defaults to isolated random fingerprints
	// (mirroring gen-freebuff-token.sh: "enhanced-" + base64url(random-32-bytes))
	// so multiple accounts added to a pool are not correlated by a single machine
	// identifier. Passing ?isolated=false requests the stable machine fingerprint.
	var code *upstream.CLILoginCode
	var err error
	if r.URL.Query().Get("isolated") == "false" {
		code, err = a.authClientFunc().StartCLILogin(r.Context())
	} else {
		code, err = a.authClientFunc().StartCLILoginIsolated(r.Context())
	}
	if err != nil {
		a.logfunc().Warn("login wizard: start failed", "err", err)
		a.dash.RenderResult(w, http.StatusBadGateway, false, "failed to start browser login: "+err.Error(), "login_start_failed")
		return
	}
	flowID := shortFlowID(code.FingerprintID)
	flow := &loginFlow{ID: flowID, Code: code, Started: time.Now()}
	a.loginMu.Lock()
	a.loginFlows[code.FingerprintID] = flow
	a.loginMu.Unlock()
	a.logfunc().Info("login wizard: started", "flow", flowID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"flow_id":     flowID,
		"fingerprint": code.FingerprintID, // full id: the status poll key
		"login_url":   code.LoginURL,
		"expires_at":  code.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (a *adminHandlers) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	a.pruneLoginFlows()
	fp := strings.TrimSpace(r.URL.Query().Get("fingerprint"))
	if fp == "" {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "missing fingerprint query param", "bad_request")
		return
	}
	a.loginMu.Lock()
	flow := a.loginFlows[fp]
	a.loginMu.Unlock()
	if flow == nil {
		a.dash.RenderResult(w, http.StatusNotFound, false, "login flow not found or expired — start a new one", "login_flow_missing")
		return
	}
	// Read the completion state under the lock: concurrent status polls
	// (second tab) must not both proceed to addTokenPersist —
	// the completing flag is set before the network poll so exactly one
	// goroutine owns the add.
	a.loginMu.Lock()
	done := flow.Done
	completing := flow.Completing
	flow.Completing = true
	a.loginMu.Unlock()
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
	status, err := a.authClientFunc().PollCLILogin(r.Context(), flow.Code)
	if err != nil {
		// Transient poll failure: keep the flow alive, report pending. A
		// later poll may retry completion.
		a.loginMu.Lock()
		flow.Completing = false
		a.loginMu.Unlock()
		a.logfunc().Debug("login wizard: poll failed", "flow", flow.ID, "err", err)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		return
	}
	if !status.Done {
		a.loginMu.Lock()
		flow.Completing = false
		a.loginMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		return
	}
	// Completed: add to the pool + persist to .env (mirrors handleTokenAdd).
	// All completion fields are written under the lock so a concurrent poll
	// observing Done reads a consistent record.
	flow.Done = true
	flow.Token = status.AuthToken
	a.loginMu.Lock()
	a.loginFlows[fp] = flow
	a.loginMu.Unlock()
	index, addErr := a.addTokenPersist(r.Context(), status.AuthToken)
	if addErr != nil {
		flow.Error = addErr.Error()
		a.loginMu.Lock()
		flow.Completing = false
		a.loginMu.Unlock()
		a.logfunc().Warn("login wizard: token persist failed", "flow", flow.ID, "err", addErr)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": addErr.Error()})
		return
	}
	flow.Index = index
	if index >= 0 {
		a.pool.SetTokenAccountInfo(index, status.User.Email, status.User.ID)
	}
	a.logfunc().Info("login wizard: completed", "flow", flow.ID, "token_index", index, "user", status.User.Name)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "token_index": index, "user": status.User.Name})
}

func (a *adminHandlers) pruneLoginFlows() {
	cutoff := time.Now().Add(-loginFlowTTL)
	a.loginMu.Lock()
	for fp, flow := range a.loginFlows {
		if flow.Started.Before(cutoff) {
			delete(a.loginFlows, fp)
		}
	}
	a.loginMu.Unlock()
}

// applyReloadedConfig propagates a freshly loaded config to every live
// consumer: the atomic config snapshot, the registry, the pool, and the
// per-IP rate limiter. Every config-save/reload path must apply new
// settings through this one method so no consumer is skipped — the
// change-password reload historically dropped the rate limiter.
func (a *adminHandlers) applyReloadedConfig(cfg *config.Config) {
	a.cfgStore(cfg)
	a.reg.SetConfig(cfg)
	a.pool.SetConfig(cfg)
	a.rateLimiter.SetRate(cfg.RateLimitPerIP, cfg.RateLimitBurst)
}

func (a *adminHandlers) handleReload(w http.ResponseWriter, r *http.Request) {
	a.logfunc().Info("admin reload requested", "remote", remoteHost(r), "path", r.URL.Path)
	// Serialize with the .env writers (config editor, token add/remove,
	// mode switch): the reload re-reads the SAME files those writers mutate,
	// and applying a load raced against a save could store stale config
	// over a just-saved one (disk NEW, memory OLD).
	a.adminSaveMu.Lock()
	defer a.adminSaveMu.Unlock()
	newCfg, err := config.Load(a.configPath)
	if err != nil {
		a.logfunc().Warn("admin reload failed", "remote", remoteHost(r), "path", r.URL.Path, "err", err)
		a.dash.RenderResult(w, http.StatusInternalServerError, false, "failed to reload config: "+err.Error(), "reload_failed")
		return
	}
	a.applyReloadedConfig(&newCfg)
	a.logfunc().Info("config reloaded successfully", "remote", remoteHost(r), "path", r.URL.Path,
		"auth_tokens", len(newCfg.AuthTokens), "safe_mode", newCfg.SafeMode)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "ok",
		"message":     "configuration reloaded",
		"auth_tokens": len(newCfg.AuthTokens),
		"safe_mode":   newCfg.SafeMode,
	})
}
