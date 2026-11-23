// Package webapp serves the built vibecode Studio (apps/web `build:desktop`, a Vite SPA): a file when it exists, else
// the SPA fallback index.html, so client routes (/p/:id, /settings) load the app.
package webapp

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

const fallback = "index.html"

// types fixes the MIME types of the site's files: Go reads them from the OS (the Windows registry maps .mjs to
// text/plain, and a module script with that type is refused by the webview).
var types = map[string]string{
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".svg":   "image/svg+xml",
	".wasm":  "application/wasm",
	".html":  "text/html; charset=utf-8",
	".png":   "image/png",
	".webp":  "image/webp",
	".woff2": "font/woff2",
}

// Handler serves site; when site has no index.html it answers a short page that says how to build the frontend.
func Handler(site fs.FS) http.Handler {
	files := http.FileServerFS(site)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name != "" && name != fallback {
			if info, err := fs.Stat(site, name); err == nil && !info.IsDir() {
				if typ, ok := types[strings.ToLower(path.Ext(name))]; ok {
					w.Header().Set("content-type", typ)
				}
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("cache-control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		page, err := fs.ReadFile(site, fallback)
		w.Header().Set("cache-control", "no-cache")
		w.Header().Set("content-type", "text/html; charset=utf-8")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("<!doctype html><title>BuildY VibeChat</title><p>The Studio is not built into this binary. Run scripts/build-frontend.sh, then build again.</p>"))
			return
		}
		_, _ = w.Write(page)
	})
}
