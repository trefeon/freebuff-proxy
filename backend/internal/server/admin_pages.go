package server

// Per-page UI state endpoints (slice 4): display-only dashboard snapshots
// that survive a gateway restart (last-visited hash, expanded rows, filter
// text). The SPA stays fetch-only: every page loads its snapshot on mount
// and saves it back debounced; failures are warn-only client-side.
//
//	GET /admin/api/pages/{id}  {data} (absent row -> {}, never 404)
//	PUT /admin/api/pages/{id}  {data} upserts (data must be a JSON object,
//	64KB cap; envelopes over the 72KB body limiter 413 as page_too_large)
//
// Unknown ids 404 against the allowlist below. A nil store (DB failed at
// boot) degrades GET to {} and 503s PUT — the same split the settings
// overlay uses, so a write never silently lands nowhere. PUT carries the
// CSRF gate like every other state-changing admin row.

import (
	"encoding/json"
	"net/http"
	"strings"
)

const maxPageStateBytes = 64 << 10

// validPageIDs is the pages_state allowlist: every NAV_ITEMS id from the SPA
// nav registry (frontend/src/lib/nav.js) plus "shell" — chrome state that is
// not a page itself (currently just {lastHash}, restored by App.svelte on
// boot). The four deep-link-only ids (setup/metrics/traces/playground,
// inSidebar:false, with playground aliasing the DevTools component) persist
// exactly like sidebar pages: the shell restores lastHash across them and
// each mounts its own snapshot key, so rejecting them would 404 real visits.
var validPageIDs = map[string]bool{
	"overview":   true,
	"tokens":     true,
	"maturity":   true,
	"quota":      true,
	"models":     true,
	"logs":       true,
	"settings":   true,
	"devtools":   true,
	"setup":      true,
	"metrics":    true,
	"traces":     true,
	"playground": true,
	"shell":      true,
}

func (a *adminHandlers) handlePageStateGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPageIDs[id] {
		a.dash.RenderResult(w, http.StatusNotFound, false, "Unknown page "+id+".", "unknown_page")
		return
	}
	if a.settings == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
		return
	}
	data, ok, err := a.settings.GetPageState(id)
	if err != nil {
		a.dash.RenderResult(w, http.StatusInternalServerError, false, "Failed to read page state: "+err.Error(), "persist_failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if !ok || data == "" {
		_, _ = w.Write([]byte(`{"data":{}}`))
		return
	}
	// Stored rows are validated JSON on PUT, so echoing verbatim is safe.
	_, _ = w.Write([]byte(`{"data":` + data + `}`))
}

func (a *adminHandlers) handlePageStatePut(w http.ResponseWriter, r *http.Request) {
	if a.settings == nil {
		a.dash.RenderResult(w, http.StatusServiceUnavailable, false,
			"Page state store unavailable — the dashboard runs live-only (the DB failed to open at boot).", "pages_unavailable")
		return
	}
	id := r.PathValue("id")
	if !validPageIDs[id] {
		a.dash.RenderResult(w, http.StatusNotFound, false, "Unknown page "+id+".", "unknown_page")
		return
	}
	// Envelope slack above the 64KB data cap: the cap applies to data, not
	// the {"data":...} wrapper.
	r.Body = http.MaxBytesReader(w, r.Body, (maxPageStateBytes + 8<<10))
	var req struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// An envelope over the 72KB body limiter trips MaxBytesReader: the
		// client blew past even the wrapper slack, so it keys truncation
		// on page_too_large (413), not a generic bad_request (400).
		if isBodyTooLarge(err) {
			a.dash.RenderResult(w, http.StatusRequestEntityTooLarge, false, "Page state exceeds 64KB.", "page_too_large")
			return
		}
		a.dash.RenderResult(w, http.StatusBadRequest, false, "Invalid JSON body (want {data}).", "bad_request")
		return
	}
	if len(req.Data) == 0 {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "Missing data (want {data}).", "bad_request")
		return
	}
	// Every consumer (loadPageState's object merge, the shell restore)
	// expects an object: null/number/string/array snapshots would echo back
	// verbatim and break the spread on the next load.
	if !isJSONObject(req.Data) {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "Page state data must be a JSON object (want {data: {...}}).", "bad_request")
		return
	}
	if len(req.Data) > maxPageStateBytes {
		a.dash.RenderResult(w, http.StatusRequestEntityTooLarge, false, "Page state exceeds 64KB.", "page_too_large")
		return
	}
	if err := a.settings.PutPageState(id, string(req.Data)); err != nil {
		a.dash.RenderResult(w, http.StatusInternalServerError, false, "Failed to persist page state: "+err.Error(), "persist_failed")
		return
	}
	a.dash.RenderResult(w, http.StatusOK, true, "Page state saved.", "page_saved")
}

// isBodyTooLarge reports a MaxBytesReader limiter trip (net/http surfaces it
// as "http: request body too large" through the JSON decoder).
func isBodyTooLarge(err error) bool {
	return err != nil && strings.Contains(err.Error(), "request body too large")
}

// isJSONObject reports whether raw JSON is an object (leading whitespace
// skipped). Missing data never reaches here (len == 0 rejects first).
func isJSONObject(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return b == '{'
		}
	}
	return false
}
