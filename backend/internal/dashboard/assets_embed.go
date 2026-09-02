package dashboard

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var files embed.FS

// DistFS returns the embedded dist filesystem for SPA serving.
func DistFS() fs.FS {
	if sub, err := fs.Sub(files, "dist"); err == nil {
		return sub
	}
	return files
}

// serveCSP is the Content-Security-Policy applied to every embedded SPA
// response (both real dist files and the index.html fallback). The Vite
// bundle is external-script based (no inline JS), style-src keeps
// 'unsafe-inline' for Svelte style: attributes, and frame-ancestors 'none'
// blocks clickjacking of the admin panel.
const serveCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'"

// ServeSPA serves the embedded single-page application and static assets from dist/.
func (d *Dashboard) ServeSPA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", serveCSP)
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
					// Vite content-hashes everything under assets/, so those
					// files are immutable per build; index.html and any other
					// root file revalidate (#312).
					if strings.HasPrefix(reqPath, "assets/") {
						w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					} else {
						w.Header().Set("Cache-Control", "no-cache")
					}
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

	// index.html — direct or fallback — must never stay stale across
	// deploys: the hashed asset URLs it references change per build.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", stat.ModTime(), index.(io.ReadSeeker))
}
