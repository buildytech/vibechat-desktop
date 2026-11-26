// Package hostwire assembles the host seam for both binaries: the desktop app (cmd/vibechat, Wails, OS keychain,
// per-project preview ports) and the server (cmd/vibechat-server, net/http, key file, preview gateway, host token).
// Nothing here imports Wails.
package hostwire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	vibechat "github.com/buildytech/vibechat-desktop"
	"github.com/buildytech/vibechat-desktop/internal/agent"
	"github.com/buildytech/vibechat-desktop/internal/appid"
	"github.com/buildytech/vibechat-desktop/internal/bridge"
	"github.com/buildytech/vibechat-desktop/internal/host"
	"github.com/buildytech/vibechat-desktop/internal/secrets"
	"github.com/buildytech/vibechat-desktop/internal/store"
)

// DesktopDirs answers the desktop app's data folder (state, transcripts, agent home, worktrees) and projects folder.
// VIBECHAT_DATA_DIR / VIBECHAT_PROJECTS_DIR override them (tests, the exam, portable installs).
func DesktopDirs() (data, projects string) {
	data = os.Getenv("VIBECHAT_DATA_DIR")
	if data == "" {
		base, err := os.UserConfigDir()
		if err != nil || base == "" {
			base = os.TempDir()
		}
		data = filepath.Join(base, appid.ID)
	}
	projects = os.Getenv("VIBECHAT_PROJECTS_DIR")
	if projects == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = data
		}
		projects = filepath.Join(home, "VibeChat")
	}
	return data, projects
}

// Exe is this executable with symlinks resolved (the bridge is located next to it; the agent hook runs it).
func Exe() string {
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe
}

// Config is what differs between the desktop app and the server.
type Config struct {
	DataDir, ProjectsDir string
	// Keys stores the Cursor key (secrets.Keys on the desktop, secrets.File on the server).
	Keys host.KeyStore
	// Preview runs dev servers (preview.New on the desktop, preview.NewGateway on the server).
	Preview host.Preview
	// InstanceID, PreviewPort and Tickets: see host.Options (server only).
	InstanceID  string
	PreviewPort int
	Tickets     host.TicketIssuer
}

// New builds the host seam with its store, the Cursor agent runner (bridge + mandatory SDK sandbox) and the
// templates. The returned func stops the bridge and dev servers; it is safe to call twice.
func New(seam host.Seam, c Config) (*host.Host, func(), error) {
	logf := func(s string) { log.Print(s) }
	st, err := store.Open(filepath.Join(c.DataDir, "state"))
	if err != nil {
		return nil, nil, fmt.Errorf("data folder %s: %w", c.DataDir, err)
	}
	exe := Exe()
	install, locErr := bridge.Locate(exe, os.Getenv)
	runner := agent.NewRunner(agent.RunnerOptions{Install: install, Located: locErr, Keys: c.Keys, DataDir: c.DataDir, HookExe: exe, Log: logf})
	if err := runner.SandboxError(); err != nil {
		log.Printf("agent disabled (fail closed): %v", err)
	} else {
		log.Printf("agent: cursor-sdk-bridge %s (sdk %s) with the SDK sandbox", install.Manifest.BridgeVersion, install.Manifest.SDKVersion)
	}
	h := host.New(host.Options{
		Seam: seam, Version: appid.Version, Store: st, Agent: runner, Keys: c.Keys, Templates: vibechat.Templates(),
		Preview: c.Preview, ProjectsDir: c.ProjectsDir, WorktreesDir: filepath.Join(c.DataDir, "worktrees"), Log: logf,
		InstanceID: c.InstanceID, PreviewPort: c.PreviewPort, Tickets: c.Tickets,
	})
	var once sync.Once
	return h, func() {
		once.Do(func() {
			h.Close()
			runner.Close()
		})
	}, nil
}

// SetupLog sends the log to <data>/logs/<name> as well (the GUI build has no console; the bridge output goes there
// with the key redacted); the file rolls over at 5 MB. It answers the file, or "" when it cannot write there.
func SetupLog(data, name string) string {
	dir := filepath.Join(data, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	file := filepath.Join(dir, name)
	if st, err := os.Stat(file); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(file, file+".1")
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return ""
	}
	log.SetOutput(&logWriter{file: f, console: os.Stderr})
	return file
}

// logWriter writes every log line to the file first and syncs it, then to the console. A windowsgui app has no
// valid stderr: io.MultiWriter(os.Stderr, f) stopped at that error and never wrote the file (an empty vibechat.log).
type logWriter struct {
	mu      sync.Mutex
	file    *os.File
	console io.Writer
}

func (w *logWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.file.Write(b)
	_ = w.file.Sync()
	if w.console != nil {
		_, _ = w.console.Write(b) // ignored: no console in the GUI build
	}
	return n, err
}

type fixedKey string

func (k fixedKey) Key() (string, string) { return string(k), "smoke" }

// BridgeSmoke starts the bundled bridge exactly like the first chat message (located next to this executable, SDK
// sandbox check, private agent home, handshake, Ping) with a placeholder key, prints its URL and stops it. Exit
// codes: 0 ready, 1 failed, 3 no SDK sandbox on this machine.
func BridgeSmoke(data, logFile string) int {
	exe := Exe()
	install, locErr := bridge.Locate(exe, os.Getenv)
	runner := agent.NewRunner(agent.RunnerOptions{Install: install, Located: locErr, Keys: fixedKey("smoke-placeholder-key"), DataDir: data, HookExe: exe, Log: func(s string) { log.Print(s) }})
	defer runner.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	url, err := runner.Probe(ctx)
	if errors.Is(err, agent.ErrSandboxUnsupported) {
		fmt.Printf("bridge smoke: SKIP (no SDK sandbox on this machine): %v\nlog: %s\n", err, logFile)
		return 3
	}
	if err != nil {
		fmt.Printf("bridge smoke: FAIL: %v\nlog: %s\n", err, logFile)
		return 1
	}
	fmt.Printf("bridge smoke: ready at %s (bridge %s, sdk %s)\nlog: %s\n", url, install.Manifest.BridgeVersion, install.Manifest.SDKVersion, logFile)
	return 0
}

// CheckKey is `--check-key` for bug reports: the stored key's non-secret diagnostic, then Me and an agent create
// through the bundled bridge, each with Cursor's answer. Output goes to stdout and the log. setKeyHint names how to
// store a key in this binary.
func CheckKey(data, logFile string, keys host.KeyStore, setKeyHint string) int {
	say := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		fmt.Println(line)
		log.Print("check-key: " + line)
	}
	key, source := keys.Key()
	if key == "" {
		say("no Cursor key: %s; %s", setKeyHint, secrets.WhereToGet)
		return 2
	}
	say("vibechat %s, %s", appid.Version, secrets.Describe(key, source))
	if err := secrets.CheckFormat(key); err != nil {
		say("FAIL format: %v", err)
	}
	if f := secrets.Foreign(key); f != "" {
		say("FAIL: this is a %s key; %s", f, secrets.WhereToGet)
	}
	exe := Exe()
	install, locErr := bridge.Locate(exe, os.Getenv)
	runner := agent.NewRunner(agent.RunnerOptions{Install: install, Located: locErr, Keys: keys, DataDir: data, HookExe: exe, Log: func(s string) { log.Print(s) }})
	defer runner.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code := 0
	report := func(step string, err error) {
		if err == nil {
			return
		}
		code = 1
		say("FAIL %s: %v", step, err)
		if agent.IsUnauthenticated(err) {
			say("hint: %s", secrets.AuthHint(key, source))
		}
	}
	acc, err := runner.CheckKey(ctx, key)
	report("Me", err)
	if err == nil {
		say("ok Me: key %q of %s", acc.APIKeyName, acc.UserEmail)
	}
	ws, _ := os.MkdirTemp("", "vibechat-check-")
	defer os.RemoveAll(ws)
	menu, err := runner.Models(ctx)
	report("model list", err)
	if err == nil {
		say("ok model list: %d entries", len(menu))
	}
	sel, err := runner.Select(ctx, agent.AutoID, nil)
	if err != nil {
		sel = agent.Selection{ID: "default"}
	}
	id, err := runner.EnsureAgent(ctx, "", ws, sel)
	report("agent create", err)
	if err == nil {
		say("ok agent create: %s", id)
	}
	say("log: %s", logFile)
	return code
}
