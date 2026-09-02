package server

// gzipMiddleware compresses text-based responses when the client accepts
// gzip. Streaming responses (text/event-stream) are never compressed: SSE
// clients rely on immediate flushed frames, and intermediaries can stall
// them. The decision happens at WriteHeader time so handlers that set
// Content-Type late (ServeContent, SSE relays) behave correctly.

import (
	"compress/gzip"
	"net/http"
	"strings"
)

// compressibleTypes are the media types worth compressing (compared
// without parameters, e.g. "text/html; charset=utf-8").
var compressibleTypes = map[string]bool{
	"text/html":              true,
	"text/plain":             true,
	"text/css":               true,
	"text/javascript":        true,
	"application/javascript": true,
	"application/json":       true,
	"image/svg+xml":          true,
}

// gzipResponseWriter compresses the body when the handler's Content-Type
// at WriteHeader is compressible; everything else passes through
// untouched.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz         *gzip.Writer
	decided    bool
	compressed bool
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	if g.decided {
		g.ResponseWriter.WriteHeader(status)
		return
	}
	g.decided = true
	ct := g.Header().Get("Content-Type")
	mediaType, _, _ := strings.Cut(ct, ";")
	switch {
	case status == http.StatusNoContent || status == http.StatusNotModified ||
		status == http.StatusSwitchingProtocols || status == http.StatusPartialContent:
		// No body / upgrade / range: never rewrite the response.
	case g.Header().Get("Content-Encoding") != "":
		// Already encoded (or client says so): never double-compress.
	case compressibleTypes[strings.TrimSpace(strings.ToLower(mediaType))]:
		g.Header().Del("Content-Length")
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Add("Vary", "Accept-Encoding")
		g.gz = gzip.NewWriter(g.ResponseWriter)
		g.compressed = true
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	if !g.decided {
		g.WriteHeader(http.StatusOK)
	}
	if g.compressed {
		return g.gz.Write(p)
	}
	return g.ResponseWriter.Write(p)
}

// Flush forwards to the underlying flusher so streamed-but-uncompressed
// responses (SSE) and compressed chunks both reach the wire promptly.
func (g *gzipResponseWriter) Flush() {
	if g.compressed && g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Close drains and closes the gzip writer (flushes the gzip footer).
func (g *gzipResponseWriter) Close() {
	if g.gz != nil {
		_ = g.gz.Close()
	}
}

// acceptsGzip reports whether the request accepts gzip, honoring a
// q=0 refusal ("gzip;q=0").
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc := strings.TrimSpace(strings.ToLower(part))
		name, params, _ := strings.Cut(enc, ";")
		if strings.TrimSpace(name) != "gzip" {
			continue
		}
		if strings.Contains(params, "q=0") {
			return false
		}
		return true
	}
	return false
}

// gzipMiddleware wraps next and compresses compressible responses for
// gzip-accepting clients. HEAD requests are skipped (no body to compress;
// setting Content-Encoding on a header-only response is misleading).
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.Close()
		next.ServeHTTP(gw, r)
	})
}
