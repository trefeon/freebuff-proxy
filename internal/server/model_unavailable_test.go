package server_test

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
)

// metricValue extracts the integer value of a freebuff_proxy_* metrics line.
func metricValue(t *testing.T, body, name string) int64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name) {
			v, err := strconv.ParseInt(strings.TrimPrefix(line, name+" "), 10, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return v
		}
	}
	t.Fatalf("metric %s not found in:\n%s", name, body)
	return 0
}

// TestModelUnavailableSkipMetric pins issue #158 end-to-end: the first chat
// request for an off-window model pays the 409 admission roundtrip and falls
// back; the second request is served from the cached fallback session with
// zero upstream admission churn, counted on /metrics as
// freebuff_proxy_model_unavailable_skips_total.
func TestModelUnavailableSkipMetric(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		model := r.Header.Get("x-freebuff-model")
		w.Header().Set("Content-Type", "application/json")
		if model == "deepseek/deepseek-v4-pro" {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"status":"model_unavailable","requestedModel":"deepseek/deepseek-v4-pro","availableHours":"9am ET-5pm PT every day"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-fb","model":"`+model+`","expiresAt":"2030-01-01T00:00:00Z"}`)
	}
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.ModelUnavailableCacheTTL = time.Hour }, mock)

	_, m0 := doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	before := metricValue(t, string(m0), "freebuff_proxy_model_unavailable_skips_total")

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("deepseek/deepseek-v4-pro"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first chat status = %d, want 200: %s", resp.StatusCode, data)
	}
	resp, data = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("deepseek/deepseek-v4-pro"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	_, m1 := doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	after := metricValue(t, string(m1), "freebuff_proxy_model_unavailable_skips_total")
	if after != before+1 {
		t.Errorf("skip counter = %d, want %d (one skip for the cached second admission)", after, before+1)
	}
}
