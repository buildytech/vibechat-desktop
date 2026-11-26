// Package host answers the Studio host seam (docs/adr/0002 in buildytech/studio,
// packages/agent-contract/host-seam.json) inside the desktop app: the Studio's fetch("/runtime/v1/...") reaches it
// through the Wails asset-server middleware, in process, on the app's own origin. The Cursor key stays here.
package host

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Seam is host-seam.json: the seam version, the URL prefix, the capability fields, every route the Studio calls and
// the remote-host part (vibechat-server): protocol versions, the Hello fields, the session constants and the routes
// the desktop app itself calls (Remote).
type Seam struct {
	Seam              string      `json:"seam"`
	Prefix            string      `json:"prefix"`
	Capabilities      []string    `json:"capabilities"`
	Routes            []SeamRoute `json:"routes"`
	Protocol          int         `json:"protocol"`
	MinClientProtocol int         `json:"minClientProtocol"`
	Hello             []string    `json:"hello"`
	Auth              SeamAuth    `json:"auth"`
	Remote            []SeamRoute `json:"remote"`
}

// SeamAuth are the session constants of the remote host (HOST_SESSION_PATH and friends).
type SeamAuth struct {
	SessionPath      string `json:"sessionPath"`
	Cookie           string `json:"cookie"`
	TicketParam      string `json:"ticketParam"`
	TicketTTLSeconds int    `json:"ticketTtlSeconds"`
}

// all is Routes then Remote.
func (s Seam) all() []SeamRoute {
	return append(append([]SeamRoute{}, s.Routes...), s.Remote...)
}

// SeamRoute is one method + Express-style path (":id" segments are parameters).
type SeamRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	SSE    bool   `json:"sse,omitempty"`
}

// Key is "METHOD /path".
func (r SeamRoute) Key() string { return r.Method + " " + r.Path }

// LoadSeam parses host-seam.json.
func LoadSeam(raw []byte) (Seam, error) {
	var s Seam
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("host-seam.json: %w", err)
	}
	if s.Seam == "" || !strings.HasPrefix(s.Prefix, "/") || len(s.Routes) == 0 {
		return s, fmt.Errorf("host-seam.json: incomplete (seam %q, prefix %q, %d routes)", s.Seam, s.Prefix, len(s.Routes))
	}
	return s, nil
}

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
	for _, r := range rt.seam.all() {
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
	for _, r := range rt.seam.all() {
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
	for _, route := range rt.seam.all() {
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
