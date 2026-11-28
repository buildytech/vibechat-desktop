package pi

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

// args is Pi's command line for a run. Nothing project-local is trusted or loaded (extensions, skills, prompt
// templates, themes, MCP servers, .pi files): they are code or config the workspace could carry, and an extension could
// open another provider. Pi talks only to the generated "vibechat" provider.
func args(entry, agentDir, sessionDir, handle, model string) []string {
	return []string{entry, "--mode", "rpc",
		"--agent-dir", agentDir, "--session-dir", sessionDir, "--session-id", handle,
		"--provider", ProviderID, "--model", model, "--thinking", "off",
		"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-mcp",
		"--no-approve", "--offline"}
}

// Start begins one run: one Pi process in the jail for this prompt.
func (s *session) Start(ctx context.Context, sessionID, runID string, p codingagent.Prompt, emit func(codingagent.Event)) codingagent.Run {
	r := &run{
		s: s, done: make(chan struct{}), started: make(chan struct{}),
		tr: &Translator{SessionID: sessionID, RunID: runID, Cwd: s.cwd},
	}
	go func() {
		defer close(r.done)
		r.err = r.drive(ctx, runID, p, emit)
	}()
	return r
}

type run struct {
	s       *session
	tr      *Translator
	done    chan struct{}
	started chan struct{} // closed when the process is up and stdin is usable (or the run failed to start)
	err     error

	mu        sync.Mutex
	stdin     *cmdWriter
	startOnce sync.Once
	promptErr error
}

func (r *run) markStarted() { r.startOnce.Do(func() { close(r.started) }) }

func (r *run) setPromptErr(err error) {
	r.mu.Lock()
	r.promptErr = err
	r.mu.Unlock()
}

func (r *run) getPromptErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.promptErr
}

func (r *run) Wait() (codingagent.Result, error) {
	<-r.done
	return r.tr.Result(), r.err
}

// Cancel asks Pi to abort the current operation (the host's context is the hard stop).
func (r *run) Cancel(ctx context.Context) error {
	select {
	case <-r.started:
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	r.mu.Lock()
	w := r.stdin
	r.mu.Unlock()
	if w == nil {
		return nil
	}
	if err := w.send(map[string]any{"type": "abort"}); err != nil {
		select {
		case <-r.done:
			return nil // the run ended meanwhile: nothing left to stop
		default:
		}
		return err
	}
	return nil
}

// tail keeps the last bytes written to it (Pi's stderr).
type tail struct {
	mu  sync.Mutex
	buf []byte
}

const tailMax = 4096

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > tailMax {
		t.buf = t.buf[len(t.buf)-tailMax:]
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

func (r *run) drive(ctx context.Context, runID string, p codingagent.Prompt, emit func(codingagent.Event)) (err error) {
	defer r.markStarted()
	a := r.s.a
	if err := a.Ready(); err != nil {
		return err
	}
	// The agent reaches the provider gateway through a Unix socket made for this run alone, bound into a jail whose
	// network namespace holds nothing else. No token exists: the socket is the credential, and the agent's shell
	// (a nested jail, see isolate.ToolShellScript) has neither the socket nor any network.
	gwDir, err := os.MkdirTemp("", "vcgw-")
	if err != nil {
		return fmt.Errorf("provider gateway: %w", err)
	}
	defer os.RemoveAll(gwDir)
	stopGW, err := a.o.Gateway.ServeUnix(filepath.Join(gwDir, "gw.sock"), "run-"+shortID(runID))
	if err != nil {
		return fmt.Errorf("provider gateway: %w", err)
	}
	defer stopGW()

	// The per-run agent folder holds the generated models.json (no secret in it: the key is the token in the
	// environment) and is removed afterwards. The session folder is the chat's and stays for the next prompt.
	root := filepath.Join(a.o.DataDir, "agents", "pi")
	agentDir := filepath.Join(root, "run", runID)
	sessionDir := filepath.Join(root, "sessions", r.s.handle)
	for _, d := range []string{agentDir, sessionDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	defer os.RemoveAll(agentDir)
	models, err := ModelsJSON(a.o.Label, "http://127.0.0.1:"+relayPort+"/v1", a.o.Models, a.o.Compat)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), models, 0o600); err != nil {
		return err
	}
	// The read-only folder of the run: the relay that carries Pi to the socket, and the wrapper that runs the agent's
	// shell in its nested jail. The agent cannot change either.
	binDir := filepath.Join(root, "run", runID+"-bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(binDir)
	relayPath := filepath.Join(binDir, "relay.js")
	if err := os.WriteFile(relayPath, relayJS, 0o444); err != nil {
		return err
	}
	toolShell, err := a.toolShell(binDir, []string{jailSockDir, agentDir, sessionDir, binDir})
	if err != nil {
		return err
	}
	settings, _ := json.Marshal(map[string]any{"shellPath": toolShell})
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), settings, 0o600); err != nil {
		return err
	}
	gitRO, gitRW, gitLocked := gitMounts(r.s.cwd)

	in := a.o.Install
	argv := append([]string{in.Node, "--require", relayPath}, args(in.Entry, agentDir, sessionDir, r.s.handle, r.s.model.ID)...)
	full, env, _, err := isolate.Command(argv, isolate.Spec{
		Dir: r.s.cwd, Writable: append([]string{agentDir, sessionDir}, gitRW...), RequireJail: true, NoNet: true,
		ReadOnly: append([]string{in.Dir, filepath.Dir(in.Node), binDir}, gitRO...),
		// Pi writes its own bookkeeping in the agent folder, but the settings (which shell it starts) and the models
		// are read-only: the agent cannot take its shell wrapper away.
		Binds: append([]isolate.Bind{{Src: gwDir, Dst: jailSockDir},
			{Src: filepath.Join(agentDir, "settings.json"), Dst: filepath.Join(agentDir, "settings.json")},
			{Src: filepath.Join(agentDir, "models.json"), Dst: filepath.Join(agentDir, "models.json")}}, gitLocked...),
		Env: append([]string{
			"PI_CODING_AGENT_DIR=" + agentDir, "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1",
			"VIBECHAT_GW_SOCK=" + jailSockDir + "/gw.sock", "VIBECHAT_GW_PORT=" + relayPort,
			// Everything is on loopback: a proxy of the server's environment must not be asked for it.
			"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost",
		}, gitEnv...),
	})
	if err != nil {
		return err
	}
	cmd := procgroup.Command(full[0], full[1:]...)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	// Stdout is our own pipe, not cmd.StdoutPipe: Wait closes that one as soon as the process exits, possibly before
	// the last records were read.
	stdout, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer stdout.Close()
	cmd.Stdout = outW
	var stderr tail
	cmd.Stderr = &stderr
	procgroup.Prepare(cmd)
	err = cmd.Start()
	outW.Close() // the child holds its own copy; EOF on our end means every writer is gone
	if err != nil {
		return fmt.Errorf("start pi: %w", err)
	}
	procgroup.Attach(cmd)
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(exited) }()
	defer func() {
		_ = stdin.Close()
		procgroup.Stop(cmd, exited, a.o.Grace)
	}()
	clean := func(s string) string { return s }

	w := &cmdWriter{w: stdin}
	r.mu.Lock()
	r.stdin = w
	r.mu.Unlock()
	r.markStarted()

	// Records are read continuously (Pi stalls on a full pipe) and handled in order by this one goroutine.
	const promptID = "vc-prompt"
	prompt := map[string]any{"id": promptID, "type": "prompt", "message": p.Text}
	if len(p.Images) > 0 {
		imgs := make([]any, len(p.Images))
		for i, im := range p.Images {
			imgs[i] = map[string]any{"type": "image", "data": im.Data, "mimeType": im.MediaType}
		}
		prompt["images"] = imgs
	}
	settled := make(chan struct{})
	var once sync.Once
	settle := func() { once.Do(func() { close(settled) }) }
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		err := readRecords(stdout, func(rec Record) bool {
			if a.o.Tap != nil {
				a.o.Tap(rec.Raw)
			}
			switch rec.Type {
			case "response":
				var resp struct {
					ID      string `json:"id"`
					Command string `json:"command"`
					Success bool   `json:"success"`
					Error   string `json:"error"`
					Data    struct {
						Disposition string `json:"disposition"`
					} `json:"data"`
				}
				if json.Unmarshal(rec.Raw, &resp) != nil || resp.ID != promptID {
					return true
				}
				if !resp.Success {
					r.setPromptErr(fmt.Errorf("pi refused the prompt: %s", clip(clean(resp.Error), 400)))
					settle()
				} else if resp.Data.Disposition == "handled" {
					// No run started for this prompt (an extension command), so no agent_settled will come.
					settle()
				}
			case "extension_ui_request":
				// Nothing can answer a dialog here: cancel it, or Pi would wait for it forever.
				var req struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}
				if json.Unmarshal(rec.Raw, &req) == nil && req.ID != "" {
					switch req.Method {
					case "select", "confirm", "input", "editor":
						_ = w.send(map[string]any{"type": "extension_ui_response", "id": req.ID, "cancelled": true})
					}
				}
			default:
				for _, e := range r.tr.Record(rec) {
					emit(e)
				}
				if r.tr.Settled() {
					settle()
				}
			}
			return true
		})
		if err != nil {
			// Unreadable output (a record over the limit): stop here, the deferred Stop ends the process.
			r.setPromptErr(fmt.Errorf("pi output: %v", err))
			settle()
		}
	}()

	if err := w.send(prompt); err != nil {
		return fmt.Errorf("send prompt to pi: %w", err)
	}
	aborted, forced := false, false
	select {
	case <-settled:
	case <-exited:
		// Drain what the pipe still holds before judging the exit (a stray grandchild may keep it open: bounded wait).
		select {
		case <-readDone:
		case <-time.After(2 * time.Second):
		}
		if !r.tr.Settled() && r.getPromptErr() == nil {
			msg := clean(stderr.String())
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("pi exited before the run finished (%v)%s", waitErr, withTail(msg))
		}
	case <-ctx.Done():
		aborted = true
		_ = w.send(map[string]any{"type": "abort"})
		select {
		case <-settled:
		case <-exited:
		case <-time.After(a.o.Grace):
			forced = true // it ignores abort: the deferred Stop ends the process group
		}
	}
	// Orderly shutdown: close stdin, let Pi finish writing, then the deferred Stop kills what is left.
	_ = stdin.Close()
	if !forced {
		select {
		case <-exited:
		case <-time.After(a.o.Grace):
		}
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
	}
	if s := clean(stderr.String()); s != "" {
		a.o.Log("pi stderr: " + clip(oneLine(s), 600))
	}
	switch {
	case r.getPromptErr() != nil:
		return r.getPromptErr()
	case aborted && ctx.Err() != nil:
		return ctx.Err()
	}
	return nil
}

func withTail(s string) string {
	if s == "" {
		return ""
	}
	return ": " + clip(oneLine(s), 600)
}

//go:embed relay.js
var relayJS []byte

// relayPort is the port of Pi's gateway inside the jail's own network namespace (nothing else lives there).
const relayPort = "18199"

// jailSockDir is where the run's gateway socket folder appears in the jail.
const jailSockDir = "/tmp/vc-gw"

// gitEnv is the identity of the agent's own git commands in the jail: the host's (gitx), no config of the machine.
var gitEnv = []string{
	"GIT_AUTHOR_NAME=VibeChat", "GIT_AUTHOR_EMAIL=vibechat@localhost",
	"GIT_COMMITTER_NAME=VibeChat", "GIT_COMMITTER_EMAIL=vibechat@localhost",
	"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// toolShell writes the wrapper that runs the agent's shell commands in the nested jail and answers its path.
func (a *Adapter) toolShell(binDir string, mask []string) (string, error) {
	bash, err := isolate.ResolveBash()
	if err != nil {
		return "", fmt.Errorf("the agent's shell: %w", err)
	}
	script, err := isolate.ToolShellScript(isolate.BwrapPath(), bash, mask, "VIBECHAT_GW_SOCK", "VIBECHAT_GW_PORT", "PI_CODING_AGENT_DIR")
	if err != nil {
		return "", err
	}
	p := filepath.Join(binDir, "tool-shell")
	if err := os.WriteFile(p, []byte(script), 0o555); err != nil {
		return "", err
	}
	return p, nil
}
