// THROWAWAY SPIKE: Go baseline serving the same synthetic SSE shape as
// spike-ts-probe/server.ts, so throughput numbers are apples-to-apples.
// Not for merge to main. No upstream, no credentials.
package main

import (
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"time"
)

var startedAt = time.Now()

//go:embed static.txt
var indexBody string

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "18732"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"version":"0.0.0-spike-go","uptime_s":%d}`,
			int(time.Since(startedAt).Seconds()))
	})
	mux.HandleFunc("/v1/synthetic-sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fl, _ := w.(http.Flusher)
		for i := 1; i <= 20; i++ {
			fmt.Fprintf(w, "data: {\"i\":%d,\"text\":\"chunk-%d\"}\n\n", i, i)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexBody)
	})
	fmt.Fprintf(os.Stderr, "spike-go-baseline listening port=%s\n", port)
	_ = http.ListenAndServe("127.0.0.1:"+port, mux)
}
