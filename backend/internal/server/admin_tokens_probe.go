package server

import (
	"context"
	"errors"
	"fmt"
	"freebuff-proxy/backend/internal/dashboard"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/upstream"
	"net/http"
	"strconv"
	"time"
)

func (a *adminHandlers) handleTokenTest(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	var state *upstream.SessionState
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		state, err = a.pool.ProbeToken(ctx, id)
	}
	if err != nil {
		if errors.Is(err, upstream.ErrNoActiveSession) {
			a.logfunc().Info("dashboard token probe ok (no active session)", "token", id)
			a.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" OK — zero-cost probe succeeded (no active session).")
			return
		}
		a.logfunc().Warn("dashboard token probe failed", "token", id, "err", err)
		a.dash.RenderConfigResult(w, r, false, "Token "+strconv.Itoa(id)+" test failed: "+err.Error())
		return
	}
	msg := "Token " + strconv.Itoa(id) + " OK — zero-cost probe succeeded"
	if q := quotaSummary(state); q != "" {
		msg += " (" + q + ")"
	}
	msg += "."
	a.logfunc().Info("dashboard token probe ok", "token", id)
	a.dash.RenderConfigResult(w, r, true, msg)
}

func (a *adminHandlers) handleTokenTestAll(w http.ResponseWriter, r *http.Request) {
	// Visit auto-probe (ADR-0025): the Quota Tracker page fires ?auto=1 on
	// mount so a cold page shows numbers without a button press. Stale
	// only (pool-scoped 1h throttle shared by all clients/tabs); fresh
	// returns the current view untouched with an ok note in the same
	// envelope shape the client already drains. The manual button (no
	// param) always forces and refreshes the throttle timestamp.
	if r.URL.Query().Get("auto") == "1" {
		if a.pool.ProbeAllIfStale(r.Context(), pool.QuotaVisitProbeMaxAge) {
			a.dash.RenderConfigResult(w, r, true, "Quotas refreshed from upstream.")
		} else {
			a.dash.RenderConfigResult(w, r, true, "Quota snapshot is fresh; skipping upstream probe.")
		}
		return
	}
	results := a.pool.ProbeAll(r.Context())
	outcomes := make([]dashboard.TokenTestOutcome, 0, len(results))
	for _, res := range results {
		i := res.Index
		state, err := res.State, res.Err
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
		outcomes = append(outcomes, dashboard.TokenTestOutcome{Token: i, OK: ok, Message: msg})
	}
	if len(outcomes) == 0 {
		a.dash.RenderConfigResult(w, r, false, "No tokens to test (bridge mode has no fixed AUTH_TOKENS).")
		return
	}
	a.dash.RenderTestResults(w, r, outcomes)
}

func (a *adminHandlers) probeTokenGate(ctx context.Context, token string) (*upstream.SessionState, error) {
	state, err := a.pool.ProbeNewToken(ctx, token)
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
