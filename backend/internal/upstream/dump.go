// Debug dump writer: the `upstream response`/`upstream ok` debug records
// persist a redacted request/response record under dump/ when the client is
// constructed with debug dumping enabled. Separate from wire_helpers.go so
// the pure JSON extractors stay free of the dump path.
package upstream

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/telemetry"
)

// dump writes a debug record to dump/ when enabled.
func (c *Client) dump(kind string, req *http.Request, status int, body string) {
	if !c.debugDump {
		return
	}
	name := fmt.Sprintf("%s-%d-%s.dump", kind, time.Now().UnixNano(), sanitizeName(req.URL.Path))
	path := filepath.Join("dump", name)
	_ = os.MkdirAll("dump", 0o755)
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s %s\n", req.Method, req.URL.String())
	// RedactHeaders is the authoritative secret set (Authorization,
	// x-api-key, x-codebuff-api-key, Cookie/Set-Cookie, every x-freebuff-*):
	// dump files persist to disk, so a partial inline check leaks.
	for k, vs := range telemetry.RedactHeaders(req.Header) {
		for _, v := range vs {
			fmt.Fprintf(&buf, "%s: %s\n", k, v)
		}
	}
	fmt.Fprintf(&buf, "\n[status %d]\n%s\n", status, truncate(body, 20000))
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		// The write was previously swallowed (`_ = os.WriteFile`) —
		// surface the failure so a broken dump dir is not silent.
		slog.Warn("debug dump write failed", "path", path, "err", err)
	}
}

func sanitizeName(p string) string {
	p = strings.ReplaceAll(p, "/", "_")
	p = strings.ReplaceAll(p, ".", "_")
	if len(p) > 60 {
		p = p[:60]
	}
	return p
}
