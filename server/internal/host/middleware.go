package host

import (
	"net/http"
	"strings"
)

// Middleware hands requests under the seam prefix (/runtime/v1/*) to h and everything else to next (the Studio
// bundle), so the seam shares the Studio's origin.
func Middleware(prefix string, h http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
				h.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
