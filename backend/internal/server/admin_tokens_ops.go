package server

import (
	"context"
	"encoding/json"
	"fmt"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/dashboard"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
)

func (a *adminHandlers) addTokenPersist(ctx context.Context, token string) (int, error) {
	// Tier gate (mirrors handleTokenAdd): a banned/country-blocked token
	// minted from a datacenter IP must never enter the pool — it would fail
	// every request with 403 and amplify the ban (issue #140).
	if _, err := a.probeTokenGate(ctx, token); err != nil {
		return 0, fmt.Errorf("token rejected by probe: %w", err)
	}
	cfg := a.cfgLoad()
	existing := cfg.AuthTokens
	if len(existing) > 0 {
		idx, err := a.pool.AddToken(token)
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
		if err := a.syncTokensAfterMutation(tokens); err != nil {
			return 0, err
		}
		return idx, nil
	}
	// Bridge mode (no fixed tokens): the first wizard token switches to
	// pooled mode, exactly like handleTokenAdd.
	idx, err := a.pool.AddToken(token)
	if err != nil {
		return 0, fmt.Errorf("add token to pool: %w", err)
	}
	if err := a.syncTokensAfterMutation([]string{token}); err != nil {
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

func (a *adminHandlers) syncTokensAfterMutation(tokens []string) error {
	// Dual-layer persist (DB-unified storage): AUTH_TOKENS itself stays
	// .env-only (raw tokens never reach the store), while the
	// auth/tokens_configured presence marker goes write-through to the
	// settings table — zero secret material. A reload-verification failure
	// restores BOTH layers (mirrors handleModeSwitch's persist → verify →
	// rollback). Otherwise the failed add leaves AUTH_TOKENS=<new> in .env
	// while the live pool holds the old list — the very divergence the
	// caller is trying to avoid.
	for i, tok := range tokens {
		if strings.Contains(tok, ",") {
			return fmt.Errorf("persist AUTH_TOKENS: AUTH_TOKENS entry %d contains a comma (AUTH_TOKENS is comma-separated in .env)", i+1)
		}
	}
	set, del := tokenMarkerDelta(tokens)
	newCfg, err := a.dualWrite(
		[]config.EnvUpdate{{Key: "AUTH_TOKENS", Value: strings.Join(tokens, ",")}},
		set, del,
		func(newCfg config.Config) error {
			if !reflect.DeepEqual(newCfg.AuthTokens, tokens) {
				return fmt.Errorf("AUTH_TOKENS overridden by environment or -config JSON (%d effective vs %d requested) — persisted to .env but NOT activated; clear it there or restart without env_file, then retry", len(newCfg.AuthTokens), len(tokens))
			}
			return nil
		},
	)
	if err != nil {
		if phase, ok := dualPhaseOf(err); ok {
			switch phase {
			case dualPersistPhase:
				return fmt.Errorf("persist AUTH_TOKENS: %w", err)
			default:
				return fmt.Errorf("reload config: %w", err)
			}
		}
		return err
	}
	a.applyReloadedConfig(&newCfg)
	return nil
}

func (a *adminHandlers) handleTokenAdd(w http.ResponseWriter, r *http.Request) {
	// Cap the body before FormValue: ParseForm would otherwise slurp the
	// entire request into memory before the JSON fallback's 8KB cap applies.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req dashboard.TokenAddRequest
	req.Token = strings.TrimSpace(r.FormValue("token"))
	if req.Token == "" {
		// JSON fallback for programmatic clients.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
		if err != nil {
			a.dash.RenderConfigResult(w, r, false, "Failed to read request: "+err.Error())
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			a.dash.RenderConfigResult(w, r, false, "Invalid request: "+err.Error())
			return
		}
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" || strings.HasPrefix(strings.ToLower(req.Token), "bearer ") {
		a.dash.RenderConfigResult(w, r, false, "Invalid token (must not start with 'Bearer ').")
		return
	}
	// AUTH_TOKENS is comma-joined in .env, so a pasted token with an
	// interior comma or newline would corrupt the file on the next reload.
	// Reject before the (validity) probe and any pool mutation.
	if strings.ContainsAny(req.Token, ",\r\n") {
		a.dash.RenderConfigResult(w, r, false, "Invalid token: must not contain commas or newlines (AUTH_TOKENS is comma-separated in .env).")
		return
	}

	// adminSaveMu serializes the pool mutation + persist + reload with the
	// other .env writers (config editor, token remove, mode switch) so a
	// concurrent save cannot interleave and lose a token from .env.
	a.adminSaveMu.Lock()
	defer a.adminSaveMu.Unlock()

	cfg := a.cfgLoad()
	// Divergence guard (mirrors handleTokenRemove): a config-editor
	// AUTH_TOKENS edit or /admin/reload can diverge cfg.AuthTokens from the
	// live pool. Adding to a stale list would persist cfg.AuthTokens+new to
	// .env while the pool holds its own list, leaving pool/.env/cfg
	// permanently divergent — and the next remove is rejected by the same
	// guard, stranding the operator until restart.
	if len(cfg.AuthTokens) != a.pool.TokenCount() {
		a.dash.RenderConfigResult(w, r, false, "AUTH_TOKENS in .env differs from the live pool — reconcile in the Config editor or restart.")
		return
	}
	// Tier gate: reject dead accounts before they enter the pool. The probe
	// is zero-cost (no session slot claimed); a banned/country-blocked/
	// auth-rejected token is refused with a clear message instead of being
	// added and failing every request with 403 (the ban amplifier).
	_, err := a.probeTokenGate(r.Context(), req.Token)
	if err != nil {
		a.dash.RenderConfigResult(w, r, false, "Token rejected by probe: "+err.Error())
		return
	}
	idx, err := a.pool.AddToken(req.Token)
	if err != nil {
		a.dash.RenderConfigResult(w, r, false, err.Error())
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
	if err := a.syncTokensAfterMutation(tokens); err != nil {
		_ = a.pool.RemoveLastToken()
		a.logfunc().Warn("dashboard token add rolled back", "remote", remoteHost(r), "err", err)
		a.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	a.logfunc().Info("dashboard token added", "remote", remoteHost(r), "index", idx)
	a.dash.RenderConfigResult(w, r, true, "Token added at index "+strconv.Itoa(idx)+" and persisted to .env.")
}

func (a *adminHandlers) handleTokenRemove(w http.ResponseWriter, r *http.Request) {
	// Cap the body before FormValue: ParseForm would otherwise slurp the
	// entire request into memory. The form value is a token index, a few
	// bytes.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	// adminSaveMu serializes the pool mutation + persist + reload with the
	// other .env writers, exactly like handleTokenAdd.
	a.adminSaveMu.Lock()
	defer a.adminSaveMu.Unlock()

	cfg := a.cfgLoad()
	// A config-editor AUTH_TOKENS edit or /admin/reload can diverge
	// cfg.AuthTokens from the live pool; removing "the last token" from a
	// stale list would persist the wrong .env and leave pool/.env/cfg
	// permanently inconsistent.
	if len(cfg.AuthTokens) != a.pool.TokenCount() {
		a.dash.RenderConfigResult(w, r, false, "AUTH_TOKENS in .env differs from the live pool — reconcile in the Config editor or restart.")
		return
	}
	// The SPA sends the token INDEX it wants removed (values stay masked
	// client-side). Parse it; the old last-token-removed behavior is kept
	// when the parameter is absent (compat for callers that do not send
	// one). A middle removal is refused by the pool while any request is
	// in flight — surfaced as a plain error message.
	idx, ok := parseTokenIndex(w, r, []string{"token", "index"}, len(cfg.AuthTokens))
	if !ok {
		a.dash.RenderConfigResult(w, r, false, "Invalid token index.")
		return
	}
	removed := ""
	if idx >= 0 {
		removed = cfg.AuthTokens[idx]
	} else if len(cfg.AuthTokens) > 0 {
		removed = cfg.AuthTokens[len(cfg.AuthTokens)-1]
	}
	var err error
	if idx >= 0 {
		err = a.pool.RemoveTokenAt(idx)
	} else {
		err = a.pool.RemoveLastToken()
	}
	if err != nil {
		a.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	tokens := removeAtCopy(cfg.AuthTokens, idx)
	if idx < 0 && len(tokens) > 0 {
		tokens = tokens[:len(tokens)-1]
	}
	if err := a.syncTokensAfterMutation(tokens); err != nil {
		// Roll the pool back so a failed persist does not leave the token
		// removed from the pool but still listed in .env/cfg (mirrors
		// handleTokenAdd's rollback).
		if removed != "" {
			if _, addErr := a.pool.AddToken(removed); addErr != nil {
				a.logfunc().Warn("dashboard token remove rollback re-add failed", "remote", remoteHost(r), "err", addErr)
			}
		}
		a.logfunc().Warn("dashboard token remove rolled back", "remote", remoteHost(r), "err", err)
		a.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	a.logfunc().Info("dashboard token removed", "remote", remoteHost(r))
	msg := "Last token removed and persisted to .env."
	if idx >= 0 {
		msg = "Token removed and persisted to .env."
	}
	a.dash.RenderConfigResult(w, r, true, msg)
}

func (a *adminHandlers) handleTokenSwap(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	a.adminSaveMu.Lock()
	defer a.adminSaveMu.Unlock()

	cfg := a.cfgLoad()
	if len(cfg.AuthTokens) != a.pool.TokenCount() {
		a.dash.RenderConfigResult(w, r, false, "AUTH_TOKENS in .env differs from the live pool — reconcile in the Config editor or restart.")
		return
	}

	fromIdx := -1
	toIdx := -1

	var req dashboard.TokenSwapRequest
	body, _ := io.ReadAll(r.Body)
	if len(body) > 0 {
		_ = json.Unmarshal(body, &req)
	}

	if req.I != nil && req.J != nil {
		fromIdx = *req.I
		toIdx = *req.J
	} else if req.From != nil && req.To != nil {
		fromIdx = *req.From
		toIdx = *req.To
	} else if req.Idx != nil {
		fromIdx = *req.Idx
		switch req.Dir {
		case "up":
			toIdx = fromIdx - 1
		case "down":
			toIdx = fromIdx + 1
		default:
			if req.To != nil {
				toIdx = *req.To
			}
		}
	} else if rawFrom := r.URL.Query().Get("from"); rawFrom != "" {
		fromIdx, _ = strconv.Atoi(rawFrom)
		toIdx, _ = strconv.Atoi(r.URL.Query().Get("to"))
	} else if rawIdx := r.URL.Query().Get("index"); rawIdx != "" {
		fromIdx, _ = strconv.Atoi(rawIdx)
		dir := r.URL.Query().Get("direction")
		switch dir {
		case "up":
			toIdx = fromIdx - 1
		case "down":
			toIdx = fromIdx + 1
		}
	}

	if fromIdx < 0 || fromIdx >= len(cfg.AuthTokens) || toIdx < 0 || toIdx >= len(cfg.AuthTokens) {
		a.dash.RenderConfigResult(w, r, false, "Invalid token index or target out of range.")
		return
	}
	if fromIdx == toIdx {
		a.dash.RenderConfigResult(w, r, true, "Tokens already in requested order.")
		return
	}
	isMove := req.Action == "move" || r.URL.Query().Get("action") == "move"
	if isMove {
		if err := a.pool.MoveToken(fromIdx, toIdx); err != nil {
			a.dash.RenderConfigResult(w, r, false, err.Error())
			return
		}

		tokens := moveStringSlice(cfg.AuthTokens, fromIdx, toIdx)
		if err := a.syncTokensAfterMutation(tokens); err != nil {
			_ = a.pool.MoveToken(toIdx, fromIdx) // rollback pool order
			a.logfunc().Warn("dashboard token move rolled back", "remote", remoteHost(r), "err", err)
			a.dash.RenderConfigResult(w, r, false, err.Error())
			return
		}
		a.logfunc().Info("dashboard token moved", "remote", remoteHost(r), "from", fromIdx, "to", toIdx)
		a.dash.RenderConfigResult(w, r, true, fmt.Sprintf("Token #%d moved to position #%d and updated in .env.", fromIdx+1, toIdx+1))
		return
	}

	if err := a.pool.SwapTokens(fromIdx, toIdx); err != nil {
		a.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}

	tokens := append([]string{}, cfg.AuthTokens...)
	tokens[fromIdx], tokens[toIdx] = tokens[toIdx], tokens[fromIdx]

	if err := a.syncTokensAfterMutation(tokens); err != nil {
		_ = a.pool.SwapTokens(fromIdx, toIdx) // rollback pool order
		a.logfunc().Warn("dashboard token swap rolled back", "remote", remoteHost(r), "err", err)
		a.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	a.logfunc().Info("dashboard tokens swapped", "remote", remoteHost(r), "from", fromIdx, "to", toIdx)
	a.dash.RenderConfigResult(w, r, true, fmt.Sprintf("Token #%d and Token #%d swapped and prioritized in .env.", fromIdx, toIdx))
}

func moveStringSlice(s []string, from, to int) []string {
	if from < 0 || from >= len(s) || to < 0 || to >= len(s) || from == to {
		return s
	}
	target := s[from]
	without := make([]string, 0, len(s)-1)
	without = append(without, s[:from]...)
	without = append(without, s[from+1:]...)

	res := make([]string, 0, len(s))
	res = append(res, without[:to]...)
	res = append(res, target)
	res = append(res, without[to:]...)
	return res
}
