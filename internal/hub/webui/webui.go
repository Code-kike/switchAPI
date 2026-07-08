// Package webui serves the embedded web SPA build (web/dist → dist/ here via
// `make web`). The checked-in dist/index.html is a placeholder so `go build`
// works without a node toolchain; real builds overwrite it (never committed).
package webui

import (
	"io/fs"
	"net/http"
	"strings"

	"embed"
)

//go:embed all:dist
var distFS embed.FS

// Handler serves static files from the embedded dist, falling back to
// index.html for client-side routes. Mount it at "/" AFTER the /api and
// /healthz patterns — the Go 1.22 mux picks the most specific pattern, so
// this never shadows them.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("webui: embedded dist missing: " + err.Error())
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" && p != "index.html" {
			if f, err := sub.Open(p); err == nil {
				f.Close()
				// Vite emits content-hashed filenames under assets/ — safe to
				// cache hard; everything else revalidates.
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		// SPA fallback: unknown paths are client-side routes.
		w.Header().Set("Cache-Control", "no-cache")
		r.URL.Path = "/"
		files.ServeHTTP(w, r)
	})
}
