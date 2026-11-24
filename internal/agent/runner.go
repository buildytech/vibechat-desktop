package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/buildytech/vibechat-desktop/internal/bridge"
	"github.com/buildytech/vibechat-desktop/internal/procgroup"
	"github.com/buildytech/vibechat-desktop/internal/sandbox"
)

// KeySource yields the Cursor key ("" when none).
type KeySource interface {
	Key() (key string, source string)
}

// RunnerOptions configure a Runner.
type RunnerOptions struct {
	Install   bridge.Install
	Located   error // why no bridge was found (nil when Install is valid)
	Keys      KeySource
	DataDir   string // agent-home/ and bridge-state/ live here
	HookExe   string // this executable (the hook command)
	Log       func(string)
	Preflight func(helper string) error // nil: run `cursorsandbox --preflight-only`
}

// Runner owns the bridge process and the Cursor agents.
type Runner struct {
	o        RunnerOptions
	mu       sync.Mutex
	proc     *bridge.Process
	procKey  string
	sandbox  error // nil when the SDK sandbox is usable
	checked  bool
	models   []string
	modelsAt time.Time
}

// NewRunner checks the sandbox once (helper present + preflight) and prepares the private agent home.
func NewRunner(o RunnerOptions) *Runner {
	r := &Runner{o: o}
	r.sandbox = r.checkSandbox()
	return r
}

// ErrSandboxUnsupported means the OS lacks the kernel features of the SDK sandbox (preflight exit 2).
var ErrSandboxUnsupported = errors.New("this system does not support the agent sandbox")

// ErrNoKey means no Cursor key is configured.
var ErrNoKey = errors.New("No Cursor API key configured — add one in Settings")

func (r *Runner) checkSandbox() error {
	if r.o.Located != nil {
		return fmt.Errorf("agent runtime missing: %v", r.o.Located)
	}
	if !r.o.Install.HasSandboxHelper() {
		return fmt.Errorf("sandbox helper %s missing next to the bridge", bridge.HelperPackage())
	}
	pre := r.o.Preflight
	if pre == nil {
		pre = preflight
	}
	if err := pre(r.o.Install.SandboxHelper()); err != nil {
		return fmt.Errorf("sandbox preflight failed: %w", err)
	}
	if err := sandbox.WriteHome(r.home(), r.o.HookExe, RuntimeReadPaths(exec.LookPath)); err != nil {
		return fmt.Errorf("agent home: %v", err)
	}
	return nil
}

// preflight runs the helper the way the SDK does (sandbox support check): a workspace_readwrite policy with the
// network closed, --preflight-only, and a no-op command. Exit 2 means the OS lacks the kernel features.
func preflight(helper string) error {
	dir, err := os.MkdirTemp("", "vibechat-preflight-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	policy, _ := json.Marshal(map[string]any{"sandbox": map[string]any{
		"type": "workspace_readwrite", "cwd": dir, "readBoundary": "system", "hardcodedReadPaths": []string{},
		"additionalReadonlyPaths": map[string]any{}, "networkAccess": false, "additionalReadwritePaths": []string{}, "disableTmpWrite": false,
	}})
	file := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(file, policy, 0o600); err != nil {
		return err
	}
	noop := []string{"/bin/true"}
	if runtime.GOOS == "windows" {
		noop = []string{filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), "/c", "exit", "0"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := procgroup.CommandContext(ctx, helper, append([]string{"--policy", file, "--preflight-only", "--"}, noop...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 2 {
			return fmt.Errorf("%w: %s", ErrSandboxUnsupported, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (r *Runner) home() string      { return filepath.Join(r.o.DataDir, "agent-home") }
func (r *Runner) stateRoot() string { return filepath.Join(r.o.DataDir, "bridge-state") }

// SandboxError is nil when the agent may run (bridge + helper + preflight ok); the agent never runs otherwise.
func (r *Runner) SandboxError() error { return r.sandbox }

// HasKey reports whether a key is configured.
func (r *Runner) HasKey() bool { k, _ := r.o.Keys.Key(); return k != "" }

// Probe starts the bridge the way the first chat message does (sandbox check, private home, handshake and Ping) and
// returns its URL; the CI smoke (vibechat --bridge-smoke) uses it without a real key.
func (r *Runner) Probe(ctx context.Context) (string, error) {
	p, err := r.client(ctx)
	if err != nil {
		return "", err
	}
	return p.Ready.URL, nil
}

// client returns a running bridge, starting (or restarting on a key change) as needed.
func (r *Runner) client(ctx context.Context) (*bridge.Process, error) {
	if r.sandbox != nil {
		return nil, r.sandbox
	}
	key, _ := r.o.Keys.Key()
	if key == "" {
		return nil, ErrNoKey
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.proc != nil && r.proc.Alive() && r.procKey == key {
		return r.proc, nil
	}
	if r.proc != nil {
		r.proc.Close()
		r.proc = nil
	}
	bridge.CleanStale(os.TempDir(), time.Hour)
	p, err := bridge.Start(ctx, bridge.StartOptions{Install: r.o.Install, Key: key, Home: r.home(), StateRoot: r.stateRoot(), Log: r.o.Log})
	if err != nil {
		return nil, err
	}
	r.proc, r.procKey = p, key
	return p, nil
}

// Close stops the bridge (Shutdown → SIGTERM → kill of the group).
func (r *Runner) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.proc != nil {
		r.proc.Close()
		r.proc = nil
	}
}

// Account is the Cursor account of the key (SdkUser).
type Account struct {
	APIKeyName    string `json:"apiKeyName"`
	UserEmail     string `json:"userEmail"`
	UserFirstName string `json:"userFirstName"`
	UserLastName  string `json:"userLastName"`
}

// Me answers the key's account.
func (r *Runner) Me(ctx context.Context) (Account, error) {
	p, err := r.client(ctx)
	if err != nil {
		return Account{}, err
	}
	var res struct {
		User Account `json:"user"`
	}
	err = p.Unary(ctx, "SdkCursorService", "Me", r.catalogRequest(), &res)
	return res.User, err
}

// catalogRequest carries the key for the catalog calls (Me, ListModels): unlike agent runs the bridge does not fall
// back to its CURSOR_API_KEY env there. The key travels only in this loopback request body (bearer-authenticated),
// never in argv, a URL or a file.
func (r *Runner) catalogRequest() map[string]any {
	key, _ := r.o.Keys.Key()
	return map[string]any{"options": map[string]any{"apiKey": key}}
}

// ModelIDs lists the Cursor model ids the key can use (cached for 10 minutes).
func (r *Runner) ModelIDs(ctx context.Context) ([]string, error) {
	r.mu.Lock()
	if r.models != nil && time.Since(r.modelsAt) < 10*time.Minute {
		m := r.models
		r.mu.Unlock()
		return m, nil
	}
	r.mu.Unlock()
	p, err := r.client(ctx)
	if err != nil {
		return nil, err
	}
	var res struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := p.Unary(ctx, "SdkCursorService", "ListModels", r.catalogRequest(), &res); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(res.Items))
	for _, it := range res.Items {
		ids = append(ids, it.ID)
	}
	r.mu.Lock()
	r.models, r.modelsAt = ids, time.Now()
	r.mu.Unlock()
	return ids, nil
}

func agentOptions(cwd string, model Selection) map[string]any {
	return map[string]any{
		"model": model,
		"local": map[string]any{
			"cwd": []string{cwd},
			// PROJECT: the template's AGENTS.md / .cursor/rules guide the agent (project hooks.json / sandbox.json are
			// refused by sandbox.CheckWorkspace before every run). USER: the private agent home's hooks + sandbox.json.
			"settingSources": []string{"SETTING_SOURCE_PROJECT", "SETTING_SOURCE_USER"},
			"sandboxOptions": map[string]any{"enabled": true},
		},
	}
}

// EnsureAgent creates a Cursor agent for cwd, or resumes agentID (in this or a restarted bridge).
func (r *Runner) EnsureAgent(ctx context.Context, agentID, cwd string, model Selection) (string, error) {
	p, err := r.client(ctx)
	if err != nil {
		return "", err
	}
	if agentID != "" {
		var res struct {
			AgentID string `json:"agentId"`
		}
		if err := p.Unary(ctx, "SdkAgentService", "ResumeAgent", map[string]any{"agentId": agentID, "options": agentOptions(cwd, model)}, &res); err == nil {
			return firstNonEmpty(res.AgentID, agentID), nil
		}
	}
	var res struct {
		AgentID string `json:"agentId"`
	}
	if err := p.Unary(ctx, "SdkAgentService", "CreateAgent", map[string]any{"options": agentOptions(cwd, model)}, &res); err != nil {
		return "", err
	}
	if res.AgentID == "" {
		return "", errors.New("bridge returned no agent id")
	}
	return res.AgentID, nil
}

// Image is a prompt attachment (base64 data).
type Image struct {
	MediaType string `json:"mediaType"`
	Data      string `json:"data"`
}

// Send runs one prompt and feeds every frame to tr, publishing the translated events through emit.
func (r *Runner) Send(ctx context.Context, agentID, text string, images []Image, model Selection, tr *Translator, emit func(Event)) error {
	p, err := r.client(ctx)
	if err != nil {
		return err
	}
	msg := map[string]any{"text": text}
	if len(images) > 0 {
		var imgs []map[string]any
		for _, im := range images {
			imgs = append(imgs, map[string]any{"data": map[string]any{"data": im.Data, "mimeType": im.MediaType}})
		}
		msg["images"] = imgs
	}
	req := map[string]any{"agentId": agentID, "message": msg, "options": map[string]any{"model": model, "enableDeltas": true}}
	return p.Stream(ctx, "SdkAgentService", "Send", req, func(raw json.RawMessage) error {
		for _, e := range tr.Frame(raw) {
			emit(e)
		}
		return nil
	})
}

// Cancel asks the bridge to cancel a run.
func (r *Runner) Cancel(ctx context.Context, agentID, runID string) error {
	p, err := r.client(ctx)
	if err != nil {
		return err
	}
	return p.Unary(ctx, "SdkAgentService", "CancelRun", map[string]any{"runId": runID, "agentId": agentID}, nil)
}

// RuntimeReadPaths are the extra read-only paths of the agent's shell sandbox: the folders holding bun and node (when
// they are outside the SDK's system paths) and, on Linux, /proc and the THP setting Bun reads at start.
func RuntimeReadPaths(lookPath func(string) (string, error)) []string {
	var out []string
	if runtime.GOOS == "linux" {
		out = append(out, "/proc", "/sys/kernel/mm/transparent_hugepage")
	}
	seen := map[string]bool{}
	for _, tool := range []string{"bun", "node"} {
		p, err := lookPath(tool)
		if err != nil {
			continue
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		dir := filepath.Dir(p)
		switch dir {
		case "/bin", "/usr/bin", "/sbin", "/usr/sbin":
			continue
		}
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	return out
}
