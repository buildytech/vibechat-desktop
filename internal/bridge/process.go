package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/buildytech/vibechat-desktop/internal/procgroup"
)

// StartOptions configure one bridge process.
type StartOptions struct {
	Install Install
	// Key is the Cursor API key; it goes into the child's environment only.
	Key string
	// Home is the private agent home (HOME / USERPROFILE of the bridge): user-level .cursor/hooks.json and
	// sandbox.json live there, out of the agent's reach.
	Home string
	// StateRoot is the bridge's durable agent store.
	StateRoot string
	// Env is the base environment (os.Environ() unless a test sets it). CURSOR_API_KEY in it is dropped.
	Env []string
	// Log receives the bridge's stderr lines (already redacted of the key).
	Log func(string)
	// ReadyTimeout bounds the handshake (default 30 s).
	ReadyTimeout time.Duration
}

// Ready is the handshake line the bridge prints on stderr.
type Ready struct {
	SchemaVersion int    `json:"schemaVersion"`
	PID           int    `json:"pid"`
	URL           string `json:"url"`
	AuthTokenFile string `json:"authTokenFile"`
}

// Process is a running bridge.
type Process struct {
	*Client
	Ready Ready
	cmd   *exec.Cmd
	done  chan struct{}
	once  sync.Once
}

const readyPrefix = "cursor-sdk-bridge ready "

// Start launches the bridge in its own process group and completes the handshake (ready line → token → Ping).
func Start(ctx context.Context, o StartOptions) (*Process, error) {
	if o.Key == "" {
		return nil, errors.New("no Cursor API key")
	}
	for _, d := range []string{o.Home, o.StateRoot} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	cmd := exec.Command(o.Install.Executable(), "--host", "127.0.0.1", "--port", "0", "--state-root", o.StateRoot)
	cmd.Dir = o.Home
	cmd.Env = childEnv(o.Env, o.Key, o.Home)
	procgroup.Prepare(cmd)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Process{cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.done) }()

	readyCh := make(chan Ready, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		sent := false
		for sc.Scan() {
			line := sc.Text()
			if !sent && strings.HasPrefix(line, readyPrefix) {
				var r Ready
				if json.Unmarshal([]byte(strings.TrimPrefix(line, readyPrefix)), &r) == nil {
					readyCh <- r
					sent = true
				}
			}
			if o.Log != nil {
				o.Log(Redact(line, o.Key))
			}
		}
	}()
	timeout := o.ReadyTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	var ready Ready
	select {
	case ready = <-readyCh:
	case <-p.done:
		return nil, errors.New("cursor-sdk-bridge exited before it was ready")
	case <-time.After(timeout):
		p.kill()
		return nil, errors.New("cursor-sdk-bridge did not become ready")
	case <-ctx.Done():
		p.kill()
		return nil, ctx.Err()
	}
	token, err := os.ReadFile(ready.AuthTokenFile)
	if err != nil {
		p.kill()
		return nil, fmt.Errorf("bridge token: %w", err)
	}
	p.Ready = ready
	p.Client = &Client{BaseURL: ready.URL, Token: strings.TrimSpace(string(token)), HTTP: &http.Client{}}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := p.Unary(pctx, "SdkBridgeControlService", "Ping", struct{}{}, nil); err != nil {
		p.kill()
		return nil, fmt.Errorf("bridge ping: %w", err)
	}
	return p, nil
}

// Alive reports whether the process still runs.
func (p *Process) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Close shuts the bridge down: Shutdown RPC with a 5 s budget, then SIGTERM and a kill of the whole process group
// (a sandboxed shell can hold the bridge open), then removes the bridge's token folder.
func (p *Process) Close() {
	p.once.Do(func() {
		if p.Client != nil && p.Alive() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = p.Unary(ctx, "SdkBridgeControlService", "Shutdown", map[string]any{"graceSeconds": 2}, nil)
			cancel()
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
			}
		}
		procgroup.Stop(p.cmd, p.done, 3*time.Second)
		if f := p.Ready.AuthTokenFile; f != "" && strings.HasPrefix(filepath.Base(filepath.Dir(f)), "cursor-sdk-bridge-") {
			_ = os.RemoveAll(filepath.Dir(f))
		}
	})
}

func (p *Process) kill() { procgroup.Stop(p.cmd, p.done, time.Second) }

// childEnv is base without any CURSOR_API_KEY / HOME / USERPROFILE, plus the key and the private home.
func childEnv(base []string, key, home string) []string {
	if base == nil {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+4)
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "CURSOR_API_KEY", "HOME", "USERPROFILE", "XDG_CONFIG_HOME", "CURSOR_SDK_BRIDGE_LOG":
			continue
		}
		out = append(out, kv)
	}
	return append(out, "CURSOR_API_KEY="+key, "HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
}

// Redact removes key from s.
func Redact(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[redacted]")
}

// CleanStale removes token folders of bridges that died without cleaning up (left in the temp dir).
func CleanStale(tmp string, olderThan time.Duration) {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "cursor-sdk-bridge-") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > olderThan {
			_ = os.RemoveAll(filepath.Join(tmp, e.Name()))
		}
	}
}
