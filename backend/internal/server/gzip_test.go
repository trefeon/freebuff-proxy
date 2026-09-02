package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gunzipBody round-trips a gzipped response body for assertions.
func gunzipBody(t *testing.T, body []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	_ = zr.Close()
	return string(out)
}

// TestGzipMiddlewareCompressesJSON pins the happy path: a JSON response
// to a gzip-accepting client is gzipped with the right headers, and the
// body round-trips to the original payload.
func TestGzipMiddlewareCompressesJSON(t *testing.T) {
	var got string
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		got = `{"hello":["world"]}`
		_, _ = io.WriteString(w, got)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if ce := rec.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", ce)
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Error("missing Vary: Accept-Encoding")
	}
	if body := gunzipBody(t, rec.Body.Bytes()); body != got {
		t.Errorf("decompressed body = %q, want %q", body, got)
	}
}

// TestGzipMiddlewareIdentityWithoutGzip pins that a client that does not
// accept gzip gets the untouched body and no Content-Encoding.
func TestGzipMiddlewareIdentityWithoutGzip(t *testing.T) {
	payload := `{"plain":true}`
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding = %q, want empty", rec.Header().Get("Content-Encoding"))
	}
	if rec.Body.String() != payload {
		t.Errorf("body = %q, want %q", rec.Body.String(), payload)
	}
}

// TestGzipMiddlewareQ0 refuse pins gzip;q=0: the client explicitly
// refuses gzip, so the response stays identity.
func TestGzipMiddlewareQ0(t *testing.T) {
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"q":0}`)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0, br")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding = %q, want empty (q=0 refusal)", rec.Header().Get("Content-Encoding"))
	}
	if rec.Body.String() != `{"q":0}` {
		t.Errorf("body = %q, want identity body", rec.Body.String())
	}
}

// TestGzipMiddlewareSkipsSSE pins the streaming exemption: an SSE
// response is never compressed (Content-Encoding absent) and frame
// writes + Flush pass through so a client sees flushed deltas.
func TestGzipMiddlewareSkipsSSE(t *testing.T) {
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding = %q, want empty for SSE", rec.Header().Get("Content-Encoding"))
	}
	if !strings.Contains(rec.Body.String(), "event: ping") {
		t.Errorf("SSE body altered: %q", rec.Body.String())
	}
}

// TestGzipMiddlewareSkipsNoBody pin the status exemptions: 204, 304 and
// 206 (range) never get compressed.
func TestGzipMiddlewareSkipsNoBody(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotModified, http.StatusPartialContent} {
		h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(status)
		}))
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Header().Get("Content-Encoding") != "" {
			t.Errorf("status %d: Content-Encoding = %q, want empty", status, rec.Header().Get("Content-Encoding"))
		}
		if rec.Code != status {
			t.Errorf("status = %d, want %d", rec.Code, status)
		}
	}
}
