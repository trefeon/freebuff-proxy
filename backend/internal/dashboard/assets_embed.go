package dashboard

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

// HasEmbeddedSPA reports whether the binary was compiled with the embedded web dashboard.
const HasEmbeddedSPA = true

//go:embed all:dist
var files embed.FS

// DistFS returns the embedded dist filesystem for SPA serving.
func DistFS() fs.FS {
	if sub, err := fs.Sub(files, "dist"); err == nil {
		return sub
	}
	return files
}

// ServeSPA serves the embedded single-page application and static assets from dist/.
func (d *Dashboard) ServeSPA(w http.ResponseWriter, r *http.Request) {
	dist, err := fs.Sub(files, "dist")
	if err != nil {
		http.Error(w, "SPA not available", http.StatusInternalServerError)
		return
	}

	reqPath := strings.TrimPrefix(r.URL.Path, "/admin")
	reqPath = strings.TrimPrefix(reqPath, "/")

	// Serve a real top-level dist file when one exists (index.html at
	// minimum). ServeContent avoids FileServer's "/index.html" → "./"
	// redirect and its directory-listing behavior; the stripped path is
	// passed as the file name so the /admin prefix is never double-nested.
	if reqPath != "" && !strings.Contains(reqPath, "..") {
		if f, err := dist.Open(reqPath); err == nil {
			if stat, statErr := f.Stat(); statErr == nil && !stat.IsDir() {
				if rs, ok := f.(io.ReadSeeker); ok {
					http.ServeContent(w, r, reqPath, stat.ModTime(), rs)
					_ = f.Close()
					return
				}
			}
			_ = f.Close()
		}
	}

	index, err := dist.Open("index.html")
	if err != nil {
		http.Error(w, "SPA index not available", http.StatusInternalServerError)
		return
	}
	defer func() { _ = index.Close() }()

	stat, err := index.Stat()
	if err != nil {
		http.Error(w, "SPA index not available", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", stat.ModTime(), index.(io.ReadSeeker))
}
