package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/upstream"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

func tokenActionID(r *http.Request) (int, error) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 0 {
		return 0, errors.New("invalid token id")
	}
	return id, nil
}

func (s *Server) handleTokenUnlock(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err == nil {
		err = s.pool.UnlockToken(id)
	}
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, "Unlock failed: "+err.Error())
		return
	}
	s.logger.Info("dashboard token unlocked", "token", id)
	s.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" unlocked — no cooldown or ban window remains.")
}

func (s *Server) handleTokenLock(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err == nil {
		err = s.pool.LockToken(id)
	}
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, "Lock failed: "+err.Error())
		return
	}
	s.logger.Info("dashboard token locked", "token", id)
	s.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" locked — it will not be used for new requests.")
}

func (s *Server) handleTokenUnlockLock(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err == nil {
		err = s.pool.UnlockLockToken(id)
	}
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, "Unlock failed: "+err.Error())
		return
	}
	s.logger.Info("dashboard token unlocked (admin)", "token", id)
	s.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" unlocked — it is available for requests again.")
}

func (s *Server) handleTokenFinish(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err == nil {
		err = s.pool.FinishTokenRuns(r.Context(), id)
	}
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, "Finish failed: "+err.Error())
		return
	}
	s.logger.Info("dashboard token runs finished", "token", id)
	s.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" runs finished.")
}

func (s *Server) handleTokenSpawnSession(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, "Invalid token ID: "+err.Error())
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		model = "mimo/mimo-v2.5"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	instanceID, err := s.pool.EnsureTokenSession(ctx, id, model)
	if err != nil {
		s.logger.Warn("dashboard token session create failed", "token", id, "model", model, "err", err)
		s.dash.RenderConfigResult(w, r, false, fmt.Sprintf("Token #%d session failed for %s: %s", id, model, err.Error()))
		return
	}
	s.logger.Info("dashboard token session created", "token", id, "model", model, "instance", instanceID)
	s.dash.RenderConfigResult(w, r, true, fmt.Sprintf("Token #%d session created for %s (instance: %s).", id, model, instanceID))
}

func (s *Server) handleTokenTest(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	var state *upstream.SessionState
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		state, err = s.pool.ProbeToken(ctx, id)
	}
	if err != nil {
		if errors.Is(err, upstream.ErrNoActiveSession) {
			s.logger.Info("dashboard token probe ok (no active session)", "token", id)
			s.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" OK — zero-cost probe succeeded (no active session).")
			return
		}
		s.logger.Warn("dashboard token probe failed", "token", id, "err", err)
		s.dash.RenderConfigResult(w, r, false, "Token "+strconv.Itoa(id)+" test failed: "+err.Error())
		return
	}
	msg := "Token " + strconv.Itoa(id) + " OK — zero-cost probe succeeded"
	if q := quotaSummary(state); q != "" {
		msg += " (" + q + ")"
	}
	msg += "."
	s.logger.Info("dashboard token probe ok", "token", id)
	s.dash.RenderConfigResult(w, r, true, msg)
}

func (s *Server) handleTokenTestAll(w http.ResponseWriter, r *http.Request) {
	count := 0
	for _, snap := range s.pool.PoolSnapshot().Tokens {
		i := snap.Token
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		state, err := s.pool.ProbeToken(ctx, i)
		cancel()
		ok := err == nil || errors.Is(err, upstream.ErrNoActiveSession)
		msg := "ok"
		switch {
		case errors.Is(err, upstream.ErrNoActiveSession):
			msg = "ok (no active session)"
		case err != nil:
			msg = err.Error()
		default:
			if q := quotaSummary(state); q != "" {
				msg = "ok (" + q + ")"
			}
		}
		s.dash.RenderTestResult(w, r, i, ok, msg, "")
		count++
	}
	if count == 0 {
		s.dash.RenderConfigResult(w, r, false, "No tokens to test (bridge mode has no fixed AUTH_TOKENS).")
	}
}

func (s *Server) addTokenPersist(ctx context.Context, token string) (int, error) {
	// Tier gate (mirrors handleTokenAdd): a banned/country-blocked token
	// minted from a datacenter IP must never enter the pool — it would fail
	// every request with 403 and amplify the ban (issue #140).
	if _, err := s.probeTokenGate(ctx, token); err != nil {
		return 0, fmt.Errorf("token rejected by probe: %w", err)
	}
	cfg := s.cfg.Load()
	existing := cfg.AuthTokens
	if len(existing) > 0 {
		idx, err := s.pool.AddToken(token)
		if err != nil {
			return 0, fmt.Errorf("add token to pool: %w", err)
		}
		// Persist the runtime list (pool may have bridge additions too, but
		// AUTH_TOKENS is the fixed set — append only when not already there).
		tokens := append([]string(nil), existing...)
		seen := false
		for _, t := range tokens {
			if t == token {
				seen = true
				break
			}
		}
		if !seen {
			tokens = append(tokens, token)
		}
		if err := s.syncTokensAfterMutation(tokens); err != nil {
			return 0, err
		}
		return idx, nil
	}
	// Bridge mode (no fixed tokens): the first wizard token switches to
	// pooled mode, exactly like handleTokenAdd.
	idx, err := s.pool.AddToken(token)
	if err != nil {
		return 0, fmt.Errorf("add token to pool: %w", err)
	}
	if err := s.syncTokensAfterMutation([]string{token}); err != nil {
		return 0, err
	}
	return idx, nil
}

func shortFlowID(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

func (s *Server) syncTokensAfterMutation(tokens []string) error {
	// Snapshot the .env before writing so a reload-verification failure can
	// restore it byte-exact (mirrors handleModeSwitch's persist → verify →
	// rollback). Otherwise the failed add leaves AUTH_TOKENS=<new> in .env
	// while the live pool holds the old list — the very divergence the
	// caller is trying to avoid.
	old, oldErr := os.ReadFile(".env")
	if _, err := updateAuthTokensEnv(tokens); err != nil {
		return fmt.Errorf("persist AUTH_TOKENS: %w", err)
	}
	newCfg, err := config.Load(s.configPath)
	if err != nil {
		restoreEnvFile(old, oldErr)
		return fmt.Errorf("reload config: %w", err)
	}
	if !reflect.DeepEqual(newCfg.AuthTokens, tokens) {
		restoreEnvFile(old, oldErr)
		return fmt.Errorf("AUTH_TOKENS overridden by environment or -config JSON (%d effective vs %d requested) — persisted to .env but NOT activated; clear it there or restart without env_file, then retry", len(newCfg.AuthTokens), len(tokens))
	}
	s.cfg.Store(&newCfg)
	s.reg.SetConfig(&newCfg)
	s.pool.SetConfig(&newCfg)
	s.rateLimiter.SetRate(newCfg.RateLimitPerIP, newCfg.RateLimitBurst)
	return nil
}

func (s *Server) probeTokenGate(ctx context.Context, token string) (*upstream.SessionState, error) {
	state, err := s.pool.ProbeNewToken(ctx, token)
	if err != nil {
		if errors.Is(err, upstream.ErrNoActiveSession) {
			// No active session is fine: the pool will create one on first
			// use. Treat as usable.
			return state, nil
		}
		return nil, err
	}
	if state != nil {
		switch state.Status {
		case "banned":
			return nil, fmt.Errorf("token is banned upstream (status banned): %w", upstream.ErrBanned)
		case "country_blocked":
			return nil, fmt.Errorf("token is country-blocked upstream: %w", upstream.ErrCountryBlocked)
		}
	}
	return state, nil
}

func (s *Server) handleTokenAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	req.Token = strings.TrimSpace(r.FormValue("token"))
	if req.Token == "" {
		// JSON fallback for programmatic clients.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
		if err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to read request: "+err.Error())
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Invalid request: "+err.Error())
			return
		}
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" || strings.HasPrefix(strings.ToLower(req.Token), "bearer ") {
		s.dash.RenderConfigResult(w, r, false, "Invalid token (must not start with 'Bearer ').")
		return
	}

	// adminSaveMu serializes the pool mutation + persist + reload with the
	// other .env writers (config editor, token remove, mode switch) so a
	// concurrent save cannot interleave and lose a token from .env.
	s.adminSaveMu.Lock()
	defer s.adminSaveMu.Unlock()

	cfg := s.cfg.Load()
	// Divergence guard (mirrors handleTokenRemove): a config-editor
	// AUTH_TOKENS edit or /admin/reload can diverge cfg.AuthTokens from the
	// live pool. Adding to a stale list would persist cfg.AuthTokens+new to
	// .env while the pool holds its own list, leaving pool/.env/cfg
	// permanently divergent — and the next remove is rejected by the same
	// guard, stranding the operator until restart.
	if len(cfg.AuthTokens) != s.pool.TokenCount() {
		s.dash.RenderConfigResult(w, r, false, "AUTH_TOKENS in .env differs from the live pool — reconcile in the Config editor or restart.")
		return
	}
	// Tier gate: reject dead accounts before they enter the pool. The probe
	// is zero-cost (no session slot claimed); a banned/country-blocked/
	// auth-rejected token is refused with a clear message instead of being
	// added and failing every request with 403 (the ban amplifier).
	_, err := s.probeTokenGate(r.Context(), req.Token)
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, "Token rejected by probe: "+err.Error())
		return
	}
	idx, err := s.pool.AddToken(req.Token)
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	// Build the persist list from cfg (the fixed AUTH_TOKENS set) plus the
	// new token, skipping any token already present: a duplicate add must
	// not write `tok,cb,cb` to .env — splitList would collapse it on reload
	// and the strict reload check would reject the add and roll back.
	tokens := append([]string{}, cfg.AuthTokens...)
	seen := false
	for _, t := range tokens {
		if t == req.Token {
			seen = true
			break
		}
	}
	if !seen {
		tokens = append(tokens, req.Token)
	}
	if err := s.syncTokensAfterMutation(tokens); err != nil {
		_ = s.pool.RemoveLastToken()
		s.logger.Warn("dashboard token add rolled back", "remote", remoteHost(r), "err", err)
		s.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	s.logger.Info("dashboard token added", "remote", remoteHost(r), "index", idx)
	s.dash.RenderConfigResult(w, r, true, "Token added at index "+strconv.Itoa(idx)+" and persisted to .env.")
}

func (s *Server) handleTokenRemove(w http.ResponseWriter, r *http.Request) {
	// adminSaveMu serializes the pool mutation + persist + reload with the
	// other .env writers, exactly like handleTokenAdd.
	s.adminSaveMu.Lock()
	defer s.adminSaveMu.Unlock()

	cfg := s.cfg.Load()
	// A config-editor AUTH_TOKENS edit or /admin/reload can diverge
	// cfg.AuthTokens from the live pool; removing "the last token" from a
	// stale list would persist the wrong .env and leave pool/.env/cfg
	// permanently inconsistent.
	if len(cfg.AuthTokens) != s.pool.TokenCount() {
		s.dash.RenderConfigResult(w, r, false, "AUTH_TOKENS in .env differs from the live pool — reconcile in the Config editor or restart.")
		return
	}
	removed := ""
	if len(cfg.AuthTokens) > 0 {
		removed = cfg.AuthTokens[len(cfg.AuthTokens)-1]
	}
	if err := s.pool.RemoveLastToken(); err != nil {
		s.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	tokens := cfg.AuthTokens
	if len(tokens) > 0 {
		tokens = tokens[:len(tokens)-1]
	}
	if err := s.syncTokensAfterMutation(tokens); err != nil {
		// Roll the pool back so a failed persist does not leave the token
		// removed from the pool but still listed in .env/cfg (mirrors
		// handleTokenAdd's rollback).
		if removed != "" {
			if _, addErr := s.pool.AddToken(removed); addErr != nil {
				s.logger.Warn("dashboard token remove rollback re-add failed", "remote", remoteHost(r), "err", addErr)
			}
		}
		s.logger.Warn("dashboard token remove rolled back", "remote", remoteHost(r), "err", err)
		s.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	s.logger.Info("dashboard token removed", "remote", remoteHost(r))
	s.dash.RenderConfigResult(w, r, true, "Last token removed and persisted to .env.")
}

func (s *Server) handleModeSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	req.Mode = r.FormValue("mode")
	if req.Mode == "" {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
		if err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to read request: "+err.Error())
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Invalid request: "+err.Error())
			return
		}
	}
	cfg := s.cfg.Load()
	switch strings.ToLower(strings.TrimSpace(req.Mode)) {
	case "bridge":
		if cfg.BridgeMode() {
			s.dash.RenderConfigResult(w, r, false, "Already in bridge mode.")
			return
		}
		// adminSaveMu serializes the persist → verify → rollback sequence
		// with the other .env writers (config editor, token add/remove) so a
		// concurrent save cannot interleave between the write and the reload.
		// The live-pool drain stays outside the lock, after the reload is
		// verified (persist → verify → drain).
		s.adminSaveMu.Lock()
		defer s.adminSaveMu.Unlock()
		// Persist AUTH_TOKENS= (explicit empty) and
		// reload, verifying the effective config actually lands in bridge
		// mode before touching the live pool. Roll the .env back on failure.
		old, oldErr := os.ReadFile(".env")
		if _, err := updateEnvKeys([]envUpdate{{Key: "AUTH_TOKENS", Value: ""}}); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to persist .env: "+err.Error())
			return
		}
		newCfg, err := config.Load(s.configPath)
		if err != nil {
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Reload rejected: "+err.Error())
			return
		}
		if !newCfg.BridgeMode() {
			// A higher-precedence source (e.g. AUTH_TOKENS in a -config JSON
			// file or the real environment) still supplies tokens — .env alone
			// cannot clear it, so the switch cannot succeed.
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Could not switch to bridge mode: AUTH_TOKENS is still set by a -config JSON file or the environment, which overrides .env. Clear it there, or run without -config, then retry.")
			return
		}
		s.cfg.Store(&newCfg)
		s.reg.SetConfig(&newCfg)
		s.pool.SetConfig(&newCfg)
		s.rateLimiter.SetRate(newCfg.RateLimitPerIP, newCfg.RateLimitBurst)
		s.pool.RemoveAllTokens(r.Context())
		s.logger.Info("dashboard switched to bridge mode")
		s.dash.RenderConfigResult(w, r, true, "Switched to bridge mode — AUTH_TOKENS cleared; clients now send their own token.")
	case "pooled":
		if !cfg.BridgeMode() {
			s.dash.RenderConfigResult(w, r, false, "Already in pooled mode.")
			return
		}
		s.dash.RenderConfigResult(w, r, false, "Pooled mode needs tokens — add one via the Add-token form first.")
		return
	default:
		s.dash.RenderConfigResult(w, r, false, "Mode must be 'bridge' or 'pooled'.")
	}
}

// handleBridgeTokenLock locks a bridge token by its key hash (#187).
func (s *Server) handleBridgeTokenLock(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	if err := s.pool.LockBridgeEntry(key); err != nil {
		s.dash.RenderConfigResult(w, r, false, "Lock failed: "+err.Error())
		return
	}
	s.logger.Info("bridge token locked", "key", key)
	s.dash.RenderConfigResult(w, r, true, "Bridge token "+shortKey(key)+" locked.")
}

// handleBridgeTokenUnlock clears the admin lock on a bridge token (#187).
func (s *Server) handleBridgeTokenUnlock(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	if err := s.pool.UnlockBridgeEntry(key); err != nil {
		s.dash.RenderConfigResult(w, r, false, "Unlock failed: "+err.Error())
		return
	}
	s.logger.Info("bridge token unlocked", "key", key)
	s.dash.RenderConfigResult(w, r, true, "Bridge token "+shortKey(key)+" unlocked.")
}

// shortKey returns the first 8 chars of a bridge key hash for display.
func shortKey(key string) string {
	if len(key) > 8 {
		return key[:8] + "…"
	}
	return key
}
