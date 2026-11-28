package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/buildytech/vibechat-desktop/server/internal/bridge"
	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
	"github.com/buildytech/vibechat-desktop/server/internal/sandbox"
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
	o          RunnerOptions
	mu         sync.Mutex
	proc       *bridge.Process
	procKey    string
	sandbox    error // nil when the SDK sandbox is usable
	checked    bool
	catalog    Catalog
	catalogKey string
	catalogAt  time.Time
	// unavailable: per key (sha256 prefix, memory only), menu ids that failed with an access or plan error.
	unavailable map[string]map[string]bool
	// noSandbox: agents run without the SDK sandbox (Windows, or the bridge refused it once).
	noSandbox bool
}

// NewRunner checks the sandbox once (helper present + preflight) and prepares the private agent home.
func NewRunner(o RunnerOptions) *Runner {
	r := &Runner{o: o, noSandbox: runtime.GOOS == "windows"}
	r.sandbox = r.checkSandbox()
	if r.noSandbox && o.Log != nil {
		o.Log("agent: Windows — agents run without the SDK sandbox (cursor-sdk-bridge has no filesystem sandbox on win32); permission hooks still apply")
	}
	return r
}

// ErrSandboxUnsupported means the OS lacks the kernel features of the SDK sandbox (preflight exit 2).
var ErrSandboxUnsupported = errors.New("this system does not support the agent sandbox")

// ErrNoKey means no Cursor key is configured.
var ErrNoKey error = noKey{}

type noKey struct{}

func (noKey) Error() string        { return "No Cursor API key configured — add one in Settings" }
func (noKey) Is(target error) bool { return target == codingagent.ErrNoCredential }

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

// Unsandboxed reports that agents run without the SDK sandbox (Windows, or the bridge refused it once).
func (r *Runner) Unsandboxed() bool { return !r.sandboxed() }

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
	return r.clientFor(ctx, key)
}

// clientFor returns a bridge running with key (CURSOR_API_KEY in its env), restarting the one of another key.
func (r *Runner) clientFor(ctx context.Context, key string) (*bridge.Process, error) {
	if r.sandbox != nil {
		return nil, r.sandbox
	}
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

// CheckKey asks Cursor whether key works (Me through the bridge) before it is stored. The bridge is started with
// key, so after saving it the first run reuses that process.
func (r *Runner) CheckKey(ctx context.Context, key string) (Account, error) {
	p, err := r.clientFor(ctx, key)
	if err != nil {
		return Account{}, err
	}
	var res struct {
		User Account `json:"user"`
	}
	err = p.Unary(ctx, "SdkCursorService", "Me", map[string]any{"options": map[string]any{"apiKey": key}}, &res)
	return res.User, err
}

// IsUnauthenticated reports a key Cursor rejected: the bridge answers unauthenticated ("Invalid User API Key",
// "Invalid Service Token", ...) or permission_denied.
func IsUnauthenticated(err error) bool {
	var be *bridge.Error
	if errors.As(err, &be) {
		// permission_denied about a model or plan is a model access error (ModelAccessError), not a bad key
		return be.Code == "unauthenticated" || (be.Code == "permission_denied" && !mentionsModelAccess(be.Message))
	}
	return false
}

// catalogRequest carries the key for the catalog calls (Me, ListModels): unlike agent runs the bridge does not fall
// back to its CURSOR_API_KEY env there. The key travels only in this loopback request body (bearer-authenticated),
// never in argv, a URL or a file.
func (r *Runner) catalogRequest() map[string]any {
	key, _ := r.o.Keys.Key()
	return map[string]any{"options": map[string]any{"apiKey": key}}
}

// Catalog is the key's model list from Cursor (ListModels through the bridge), cached for 10 minutes per key.
func (r *Runner) Catalog(ctx context.Context) (Catalog, error) {
	key, _ := r.o.Keys.Key()
	r.mu.Lock()
	if r.catalog != nil && r.catalogKey == key && time.Since(r.catalogAt) < 10*time.Minute {
		c := r.catalog
		r.mu.Unlock()
		return c, nil
	}
	r.mu.Unlock()
	p, err := r.client(ctx)
	if err != nil {
		return nil, err
	}
	var res struct {
		Items Catalog `json:"items"`
	}
	if err := p.Unary(ctx, "SdkCursorService", "ListModels", r.catalogRequest(), &res); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.catalog, r.catalogKey, r.catalogAt = res.Items, key, time.Now()
	r.mu.Unlock()
	return res.Items, nil
}

func keyID(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:8])
}

// Models is the Studio menu for the current key: the catalog minus the models marked unavailable for it.
func (r *Runner) Models(ctx context.Context) ([]Model, error) {
	c, err := r.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	key, _ := r.o.Keys.Key()
	r.mu.Lock()
	defer r.mu.Unlock()
	return c.Menu(r.unavailable[keyID(key)]), nil
}

// Select resolves a menu id for the current key.
func (r *Runner) Select(ctx context.Context, modelID string, params []ModelParam) (Selection, error) {
	c, err := r.Catalog(ctx)
	if err != nil {
		return Selection{}, err
	}
	key, _ := r.o.Keys.Key()
	r.mu.Lock()
	bad := r.unavailable[keyID(key)][modelID]
	r.mu.Unlock()
	if bad {
		return Selection{}, fmt.Errorf("%w: %s failed for this key before", ErrUnknownModel, modelID)
	}
	return c.Select(modelID, params)
}

// MarkUnavailable removes modelID from this key's menu when err is an access or plan error; it reports whether it did.
func (r *Runner) MarkUnavailable(modelID string, err error) bool {
	if modelID == AutoID || !ModelAccessError(err) {
		return false
	}
	key, _ := r.o.Keys.Key()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unavailable == nil {
		r.unavailable = map[string]map[string]bool{}
	}
	k := keyID(key)
	if r.unavailable[k] == nil {
		r.unavailable[k] = map[string]bool{}
	}
	// the plan covers a model, not a variant: the plain and the Fast entry go together
	base := strings.TrimSuffix(modelID, "-fast")
	r.unavailable[k][base], r.unavailable[k][base+"-fast"] = true, true
	if r.o.Log != nil {
		r.o.Log(fmt.Sprintf("model %s marked unavailable for this key: %v", modelID, err))
	}
	return true
}

func agentOptions(cwd string, model Selection, sandboxed bool) map[string]any {
	return map[string]any{
		"model": model,
		"local": map[string]any{
			"cwd": []string{cwd},
			// PROJECT: the template's AGENTS.md / .cursor/rules guide the agent (project hooks.json / sandbox.json are
			// refused by sandbox.CheckWorkspace before every run). USER: the private agent home's hooks + sandbox.json.
			"settingSources": []string{"SETTING_SOURCE_PROJECT", "SETTING_SOURCE_USER"},
			// enabled=false is explicit: otherwise the private home's sandbox.json would request the sandbox again.
			"sandboxOptions": map[string]any{"enabled": sandboxed},
		},
	}
}

// sandboxUnsupported is the bridge's refusal of a sandboxed local agent ("Local SDK sandboxing was requested, but
// sandboxing is not supported in this environment"). cursor-sdk-bridge 1.2.0 on Windows always answers it: its win32
// sandbox helper is proxy-only, without filesystem isolation.
func sandboxUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "sandboxing is not supported in this environment")
}

// sandboxed reports whether agents are created with the SDK sandbox: not on Windows (see sandboxUnsupported), and not
// after the bridge refused it once in this process.
func (r *Runner) sandboxed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.noSandbox
}

// withSandboxFallback runs call with the agent options; when the bridge refuses the sandbox it logs that, remembers
// it for this process and runs call once more without it. The permission hooks of the private agent home and the
// workspace checks apply either way.
func (r *Runner) withSandboxFallback(cwd string, model Selection, call func(map[string]any) error) error {
	on := r.sandboxed()
	err := call(agentOptions(cwd, model, on))
	if on && sandboxUnsupported(err) {
		r.mu.Lock()
		r.noSandbox = true
		r.mu.Unlock()
		if r.o.Log != nil {
			r.o.Log("agent: the bridge does not support the SDK sandbox here (" + err.Error() + "); running agents without it (permission hooks still apply)")
		}
		err = call(agentOptions(cwd, model, false))
	}
	return err
}

// EnsureAgent creates a Cursor agent for cwd, or resumes agentID (in this or a restarted bridge).
func (r *Runner) EnsureAgent(ctx context.Context, agentID, cwd string, model Selection) (string, error) {
	p, err := r.client(ctx)
	if err != nil {
		return "", err
	}
	type idRes struct {
		AgentID string `json:"agentId"`
	}
	if agentID != "" {
		var res idRes
		if err := r.withSandboxFallback(cwd, model, func(opts map[string]any) error {
			return p.Unary(ctx, "SdkAgentService", "ResumeAgent", map[string]any{"agentId": agentID, "options": opts}, &res)
		}); err == nil {
			return firstNonEmpty(res.AgentID, agentID), nil
		}
	}
	var res idRes
	if err := r.withSandboxFallback(cwd, model, func(opts map[string]any) error {
		return p.Unary(ctx, "SdkAgentService", "CreateAgent", map[string]any{"options": opts}, &res)
	}); err != nil {
		return "", err
	}
	if res.AgentID == "" {
		return "", errors.New("bridge returned no agent id")
	}
	return res.AgentID, nil
}

// Image is a prompt attachment (base64 data).
type Image = codingagent.Image

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
