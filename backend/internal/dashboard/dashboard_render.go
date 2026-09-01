package dashboard

import (
	"encoding/json"
	"net/http"

	"freebuff-proxy/backend/internal/phasetiming"
)

// resultEnvelope is the single admin wire shape: every admin endpoint ships
// {ok, message} with an optional machine-readable code (issue #289). The
// server's /v1 protocol handlers keep their own wire error bodies; admin
// never emits the {error:{message}} shape.
type resultEnvelope struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// RenderResult writes the unified admin envelope at the given HTTP status.
func (d *Dashboard) RenderResult(w http.ResponseWriter, status int, ok bool, message, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resultEnvelope{OK: ok, Message: message, Code: code})
}

// RenderLogin renders the login response with an optional error message,
// writing 401 when errMsg is non-empty (the login page is a 200 shell).
func (d *Dashboard) RenderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	status := http.StatusOK
	if errMsg != "" {
		status = http.StatusUnauthorized
	}
	d.RenderResult(w, status, errMsg == "", errMsg, "")
}

// RenderRestricted renders the access-denied response as JSON.
func (d *Dashboard) RenderRestricted(w http.ResponseWriter, r *http.Request, msg string) {
	d.RenderResult(w, http.StatusForbidden, false, msg, "access_denied")
}

// RenderConfigResult renders the response after a config save or token
// action (200 on success, 400 on rejection).
func (d *Dashboard) RenderConfigResult(w http.ResponseWriter, r *http.Request, ok bool, message string) {
	status := http.StatusOK
	if !ok {
		status = http.StatusBadRequest
	}
	d.RenderResult(w, status, ok, message, "")
}

// RenderTestResult appends one per-token outcome.
func (d *Dashboard) RenderTestResult(w http.ResponseWriter, r *http.Request, token int, ok bool, message, instanceID string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":       token,
		"ok":          ok,
		"message":     message,
		"instance_id": shortID(instanceID),
	})
}

// PhaseKV is one rendered latency phase.
type PhaseKV struct {
	Name string `json:"name"`
	Ms   int64  `json:"ms"`
}

// PhaseList orders a phase map for rendering.
func PhaseList(phases map[string]int64) []PhaseKV {
	order := []string{
		phasetiming.AcquireMS,
		phasetiming.SessionRefreshMS,
		phasetiming.RunAcquireMS,
		phasetiming.UpstreamTTFBMS,
		phasetiming.TotalMS,
	}
	out := make([]PhaseKV, 0, len(order))
	for _, name := range order {
		if v, ok := phases[name]; ok {
			out = append(out, PhaseKV{Name: name, Ms: v})
		}
	}
	return out
}

// RenderSmokeResult renders the smoke-test outcome.
func (d *Dashboard) RenderSmokeResult(w http.ResponseWriter, r *http.Request, model, token string, ms int64, preview []byte, phases []PhaseKV) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"model":   model,
		"token":   token,
		"ms":      ms,
		"preview": string(preview),
		"phases":  phases,
	})
}

func (d *Dashboard) RenderDiag(w http.ResponseWriter, r *http.Request, checks []DiagCheck) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"checks": checks})
}

type DiagCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Warn    bool   `json:"warn"`
	Message string `json:"message"`
}
