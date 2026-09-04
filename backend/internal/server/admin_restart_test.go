package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
)

func TestAdminRestart(t *testing.T) {
	var restarted atomic.Bool
	oldRestart := restartProcess
	restartProcess = func() {
		restarted.Store(true)
	}
	defer func() {
		restartProcess = oldRestart
	}()

	admin := &adminHandlers{
		cfgLoad: func() *config.Config {
			return &config.Config{}
		},
		logfunc: func() *slog.Logger {
			return slog.Default()
		},
	}

	// 1. GET is rejected with 405 Method Not Allowed
	req := httptest.NewRequest(http.MethodGet, "/admin/restart", nil)
	rec := httptest.NewRecorder()
	admin.handleAdminRestart(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /admin/restart = %d, want 405", rec.Code)
	}

	// 2. Valid POST returns 200 and initiates restart
	req = httptest.NewRequest(http.MethodPost, "/admin/restart", nil)
	rec = httptest.NewRecorder()
	admin.handleAdminRestart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /admin/restart = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var res struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !res.OK {
		t.Errorf("OK = false, want true")
	}

	// Wait for goroutine to invoke restartProcess
	time.Sleep(300 * time.Millisecond)
	if !restarted.Load() {
		t.Errorf("restartProcess was not called")
	}
}
