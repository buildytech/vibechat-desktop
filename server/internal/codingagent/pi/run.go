package pi

import (
	"context"
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
	base, err := a.gateway()
	if err != nil {
		return fmt.Errorf("provider gateway: %w", err)
	}
	if err := a.Ready(); err != nil {
		return err
	}
	token, revoke := a.o.Gateway.Mint()
	defer revoke()

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
	models, err := ModelsJSON(a.o.Label, base, a.o.Models, a.o.Compat)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), models, 0o600); err != nil {
		return err
	}

	in := a.o.Install
	argv := append([]string{in.Node}, args(in.Entry, agentDir, sessionDir, r.s.handle, r.s.model.ID)...)
	full, env, _, err := isolate.Command(argv, isolate.Spec{
		Dir: r.s.cwd, Writable: []string{agentDir, sessionDir}, ReadOnly: []string{in.Dir, filepath.Dir(in.Node)}, RequireJail: true,
		Env: []string{
			"PI_CODING_AGENT_DIR=" + agentDir, "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1", GatewayEnv + "=" + token,
			// The gateway is on loopback: a proxy of the server's environment must not be asked for it.
			"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost",
		},
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
	clean := func(s string) string { return strings.ReplaceAll(s, token, "<gateway-token>") }

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
