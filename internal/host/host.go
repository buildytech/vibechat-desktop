package host

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"sync"

	"github.com/buildytech/vibechat-desktop/internal/agent"
	"github.com/buildytech/vibechat-desktop/internal/store"
	"github.com/buildytech/vibechat-desktop/internal/templates"
)

// Capabilities is HostCapabilities of the seam (studio packages/agent-contract/src/host.ts).
type Capabilities struct {
	Seam         string `json:"seam"`
	Version      string `json:"version"`
	LocalFolder  bool   `json:"local_folder"`
	Worktrees    bool   `json:"worktrees"`
	PreviewLocal bool   `json:"preview_local"`
	AgentSandbox bool   `json:"agent_sandbox"`
	Templates    bool   `json:"templates"`
}

// Agent is what the host needs from the agent runtime (agent.Runner; a fake in tests).
type Agent interface {
	SandboxError() error
	HasKey() bool
	Me(ctx context.Context) (agent.Account, error)
	// CheckKey asks Cursor whether a key works before it is stored.
	CheckKey(ctx context.Context, key string) (agent.Account, error)
	// Models is the model menu of the current key (Cursor's list, nothing hard-coded); Select resolves a menu id;
	// MarkUnavailable drops a model that failed with an access or plan error from this key's menu.
	Models(ctx context.Context) ([]agent.Model, error)
	Select(ctx context.Context, modelID string, params []agent.ModelParam) (agent.Selection, error)
	MarkUnavailable(modelID string, err error) bool
	EnsureAgent(ctx context.Context, agentID, cwd string, model agent.Selection) (string, error)
	Send(ctx context.Context, agentID, text string, images []agent.Image, model agent.Selection, tr *agent.Translator, emit func(agent.Event)) error
	Cancel(ctx context.Context, agentID, runID string) error
}

// KeyStore is the Cursor key store (secrets.Keys).
type KeyStore interface {
	Key() (string, string)
	Store(string) error
	Forget() error
}

// Options configure a Host.
type Options struct {
	Seam        Seam
	Version     string
	Store       *store.Store
	Agent       Agent
	Keys        KeyStore
	Templates   []templates.Profile
	ProjectsDir string
	// WorktreesDir holds per-session git worktrees (outside the project folder); "" turns worktrees off.
	WorktreesDir string
	// Preview runs dev servers; nil turns the preview off.
	Preview Preview
	// LookPath finds tools (exec.LookPath unless a test sets it).
	LookPath func(string) (string, error)
	// Log receives host diagnostics (never keys).
	Log func(string)
}

// Host is the seam of one desktop app.
type Host struct {
	opts   Options
	router *Router
	bus    *Bus
	mu     sync.Mutex
	runs   map[string]*activeRun // by session id
	setup  map[string]*projectSetup
}

// New builds the host and registers the routes this build implements.
func New(opts Options) *Host {
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	if opts.Log == nil {
		opts.Log = func(string) {}
	}
	h := &Host{opts: opts, router: NewRouter(opts.Seam), bus: NewBus(), runs: map[string]*activeRun{}, setup: map[string]*projectSetup{}}
	h.router.Handle("GET /host/capabilities", h.capabilities)
	if opts.Store == nil {
		return h
	}
	h.registerAccount()
	h.registerProjects()
	h.registerFiles()
	h.registerAgent()
	h.registerPreview()
	return h
}

// ServeHTTP answers /runtime/v1/*.
func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.router.ServeHTTP(w, r) }

// Router exposes the route table (tests list what is still missing).
func (h *Host) Router() *Router { return h.router }

func (h *Host) hasTool(name string) bool {
	_, err := h.opts.LookPath(name)
	return err == nil
}

// Capabilities answers what this host can do now.
func (h *Host) Capabilities() Capabilities {
	c := Capabilities{Seam: h.opts.Seam.Seam, Version: h.opts.Version}
	if h.opts.Store == nil {
		return c
	}
	c.LocalFolder = true
	c.Templates = len(h.opts.Templates) > 0 && h.hasTool("git")
	c.Worktrees = h.opts.WorktreesDir != "" && h.hasTool("git")
	c.PreviewLocal = h.opts.Preview != nil && h.hasTool("bun")
	c.AgentSandbox = h.opts.Agent != nil && h.opts.Agent.SandboxError() == nil
	return c
}

func (h *Host) capabilities(w http.ResponseWriter, _ *http.Request, _ Params) {
	WriteJSON(w, http.StatusOK, h.Capabilities())
}

// Close stops running agents and previews.
func (h *Host) Close() {
	h.mu.Lock()
	runs := make([]*activeRun, 0, len(h.runs))
	for _, r := range h.runs {
		runs = append(runs, r)
	}
	h.mu.Unlock()
	for _, r := range runs {
		r.cancel()
	}
	if h.opts.Preview != nil {
		h.opts.Preview.StopAll()
	}
}

func readJSON(r *http.Request, limit int64, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > limit {
		return errTooLarge
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	return json.Unmarshal(raw, v)
}

type hostError string

func (e hostError) Error() string { return string(e) }

const errTooLarge = hostError("request body too large")
