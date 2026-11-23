package host

import (
	"net/http"
)

// Capabilities is HostCapabilities of the seam (vibecode packages/agent-contract/src/host.ts).
type Capabilities struct {
	Seam         string `json:"seam"`
	Version      string `json:"version"`
	LocalFolder  bool   `json:"local_folder"`
	Worktrees    bool   `json:"worktrees"`
	PreviewLocal bool   `json:"preview_local"`
	AgentSandbox bool   `json:"agent_sandbox"`
	Templates    bool   `json:"templates"`
}

// Options configure a Host.
type Options struct {
	Seam    Seam
	Version string
}

// Host is the seam of one desktop app.
type Host struct {
	opts   Options
	router *Router
}

// New builds the host and registers the routes this build implements.
func New(opts Options) *Host {
	h := &Host{opts: opts, router: NewRouter(opts.Seam)}
	h.router.Handle("GET /host/capabilities", h.capabilities)
	return h
}

// ServeHTTP answers /runtime/v1/*.
func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.router.ServeHTTP(w, r) }

// Router exposes the route table (tests list what is still missing).
func (h *Host) Router() *Router { return h.router }

// Capabilities answers what this host can do now.
func (h *Host) Capabilities() Capabilities {
	return Capabilities{Seam: h.opts.Seam.Seam, Version: h.opts.Version}
}

func (h *Host) capabilities(w http.ResponseWriter, _ *http.Request, _ Params) {
	WriteJSON(w, http.StatusOK, h.Capabilities())
}
