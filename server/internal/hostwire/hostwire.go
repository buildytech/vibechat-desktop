// Package hostwire assembles the host seam of vibechat-server (server/cmd/vibechat-server: net/http, key file,
// preview gateway, host token). Nothing here imports Wails.
package hostwire

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/buildytech/vibechat-desktop/internal/appid"
	assets "github.com/buildytech/vibechat-desktop/server"
	"github.com/buildytech/vibechat-desktop/server/internal/bridge"
	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/codingagent/cursor"
	"github.com/buildytech/vibechat-desktop/server/internal/contour"
	"github.com/buildytech/vibechat-desktop/server/internal/host"
	"github.com/buildytech/vibechat-desktop/server/internal/secrets"
	"github.com/buildytech/vibechat-desktop/server/internal/store"
)

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
	// Keys stores the Cursor key (secrets.File).
	Keys codingagent.KeyStore
	// Runtime is the coding-agent runtime of new chats: "" = the contour's (cursor-sdk without a contour file). A runtime
	// this build does not have is an error at start, never a silent fallback to another provider.
	Runtime string
	// Contour is the server's contour config (contour.json): EU/US run the Cursor SDK, RU runs Pi (ADR 0002). The zero
	// value is the Cursor contour. Pi is registered alongside the Cursor adapter, never instead of it.
	Contour contour.Config
	// Preview runs dev servers (preview.NewGateway: one gateway port for every preview).
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
	runner := cursor.NewRunner(cursor.RunnerOptions{Install: install, Located: locErr, Keys: c.Keys, DataDir: c.DataDir, HookExe: exe, Log: logf})
	if err := runner.SandboxError(); err != nil {
		log.Printf("agent disabled (fail closed): %v", err)
	} else {
		log.Printf("agent: cursor-sdk-bridge %s with the SDK sandbox", install.Manifest.Describe())
	}
	adapters := []codingagent.Adapter{cursor.NewAdapter(runner, c.Keys)}
	def := c.Runtime
	if def == "" {
		def = c.Contour.RuntimeID()
	}
	if c.Contour.UsesPi() {
		pa, err := contour.Build(c.Contour, contour.Env{LookPath: exec.LookPath, DataDir: c.DataDir, Log: logf})
		if err != nil {
			runner.Close()
			return nil, nil, fmt.Errorf("contour config: %w", err)
		}
		if err := pa.Ready(); err != nil {
			log.Printf("agent pi disabled (fail closed): %v", err)
		} else {
			log.Printf("agent: %s, %s", pa.Capabilities().Label, pa.Install().Describe())
		}
		adapters = append(adapters, pa)
	}
	agents, err := codingagent.NewRegistry(def, adapters...)
	if err != nil {
		runner.Close()
		return nil, nil, fmt.Errorf("coding agent runtime: %w", err)
	}
	h := host.New(host.Options{
		Seam: seam, Version: appid.Version, Store: st, Agents: agents, Templates: assets.Templates(),
		Preview: c.Preview, ProjectsDir: c.ProjectsDir, WorktreesDir: filepath.Join(c.DataDir, "worktrees"), Log: logf,
		InstanceID: c.InstanceID, PreviewPort: c.PreviewPort, Tickets: c.Tickets,
	})
	var once sync.Once
	return h, func() {
		once.Do(func() {
			h.Close()
			agents.Close()
		})
	}, nil
}

type fixedKey string

func (k fixedKey) Key() (string, string) { return string(k), "smoke" }

// BridgeSmoke starts the bundled bridge exactly like the first chat message (located next to this executable, SDK
// sandbox check, private agent home, handshake, Ping) with a placeholder key, prints its URL and stops it. Exit
// codes: 0 ready, 1 failed, 3 no SDK sandbox on this machine.
func BridgeSmoke(data, logFile string) int {
	exe := Exe()
	install, locErr := bridge.Locate(exe, os.Getenv)
	runner := cursor.NewRunner(cursor.RunnerOptions{Install: install, Located: locErr, Keys: fixedKey("smoke-placeholder-key"), DataDir: data, HookExe: exe, Log: func(s string) { log.Print(s) }})
	defer runner.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	url, err := runner.Probe(ctx)
	if errors.Is(err, cursor.ErrSandboxUnsupported) {
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
func CheckKey(data, logFile string, keys codingagent.KeyStore, setKeyHint string) int {
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
	runner := cursor.NewRunner(cursor.RunnerOptions{Install: install, Located: locErr, Keys: keys, DataDir: data, HookExe: exe, Log: func(s string) { log.Print(s) }})
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
		if cursor.IsUnauthenticated(err) {
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
	sel, err := runner.Select(ctx, cursor.AutoID, nil)
	if err != nil {
		sel = cursor.Selection{ID: "default"}
	}
	id, err := runner.EnsureAgent(ctx, "", ws, sel)
	report("agent create", err)
	if err == nil {
		say("ok agent create: %s", id)
	}
	say("log: %s", logFile)
	return code
}
