// Package host answers the Studio host seam (docs/adr/0002 in buildytech/studio,
// packages/agent-contract/host-seam.json) inside vibechat-server: the Studio's fetch("/runtime/v1/...") reaches it on
// the server's own origin (the desktop app shows it through its SSH tunnel). The Cursor key stays here.
package host

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/buildytech/vibechat-desktop/protocol"
)

// The seam types are the protocol's (protocol/seam.go, shared with the desktop app).
type (
	Seam      = protocol.Seam
	SeamAuth  = protocol.SeamAuth
	SeamRoute = protocol.SeamRoute
)

// LoadSeam parses host-seam.json.
func LoadSeam(raw []byte) (Seam, error) { return protocol.LoadSeam(raw) }

// Params are the ":name" values of a matched route.
type Params map[string]string

// HandlerFunc answers one seam route.
type HandlerFunc func(w http.ResponseWriter, r *http.Request, p Params)

// Router dispatches the seam routes. A seam route without a handler answers 501 with { error }, a path outside the
// seam 404 with { error }: never a blank page (vibecode AGENTS.md rule 10).
type Router struct {
	seam     Seam
	handlers map[string]HandlerFunc
}

// NewRouter is a router for seam with no handlers yet.
func NewRouter(seam Seam) *Router {
	return &Router{seam: seam, handlers: map[string]HandlerFunc{}}
}

// Handle registers h for "METHOD /path"; it panics when the route is not in the seam (a host must not invent routes
// the Studio does not know; add them to buildytech/studio first).
func (rt *Router) Handle(key string, h HandlerFunc) {
	for _, r := range rt.seam.All() {
		if r.Key() == key {
			rt.handlers[key] = h
			return
		}
	}
	panic("host: route not in host-seam.json: " + key)
}

// Missing lists seam routes that have no handler.
func (rt *Router) Missing() []string {
	var out []string
	for _, r := range rt.seam.All() {
		if _, ok := rt.handlers[r.Key()]; !ok {
			out = append(out, r.Key())
		}
	}
	return out
}

// ServeHTTP answers a request whose path starts with the seam prefix.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, rt.seam.Prefix)
	if rel == "" {
		rel = "/"
	}
	pathMatched := false
	for _, route := range rt.seam.All() {
		p, ok := match(route.Path, rel)
		if !ok {
			continue
		}
		pathMatched = true
		if route.Method != r.Method {
			continue
		}
		h, ok := rt.handlers[route.Key()]
		if !ok {
			WriteError(w, http.StatusNotImplemented, "Not available in this VibeChat build yet: "+route.Key())
			return
		}
		h(w, r, p)
		return
	}
	if pathMatched {
		WriteError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	WriteError(w, http.StatusNotFound, "Not found")
}

func match(pattern, path string) (Params, bool) {
	ps := strings.Split(strings.Trim(pattern, "/"), "/")
	xs := strings.Split(strings.Trim(path, "/"), "/")
	if len(ps) != len(xs) {
		return nil, false
	}
	p := Params{}
	for i := range ps {
		if strings.HasPrefix(ps[i], ":") {
			if xs[i] == "" {
				return nil, false
			}
			p[ps[i][1:]] = xs[i]
			continue
		}
		if ps[i] != xs[i] {
			return nil, false
		}
	}
	return p, true
}

// WriteJSON answers v with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError answers { error: msg }.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}
