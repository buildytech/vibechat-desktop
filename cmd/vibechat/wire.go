package main

import (
	"log"
	"os"
	"path/filepath"
	"sync"

	vibechat "github.com/buildytech/vibechat-desktop"
	"github.com/buildytech/vibechat-desktop/internal/agent"
	"github.com/buildytech/vibechat-desktop/internal/appid"
	"github.com/buildytech/vibechat-desktop/internal/bridge"
	"github.com/buildytech/vibechat-desktop/internal/host"
	"github.com/buildytech/vibechat-desktop/internal/secrets"
	"github.com/buildytech/vibechat-desktop/internal/store"
)

// dirs answers the data folder (state, transcripts, agent home, worktrees) and the projects folder.
// VIBECHAT_DATA_DIR / VIBECHAT_PROJECTS_DIR override them (tests, the exam, portable installs).
func dirs() (data, projects string) {
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

// newHost builds the host seam with its store, the Cursor agent runner (bridge + mandatory SDK sandbox) and the
// templates. The returned func stops the bridge and dev servers; it is safe to call twice.
func newHost(seam host.Seam) (*host.Host, func()) {
	logf := func(s string) { log.Print(s) }
	data, projects := dirs()
	st, err := store.Open(filepath.Join(data, "state"))
	if err != nil {
		log.Fatalf("data folder %s: %v", data, err)
	}
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	install, locErr := bridge.Locate(exe, os.Getenv)
	keys := secrets.Keys{}
	runner := agent.NewRunner(agent.RunnerOptions{Install: install, Located: locErr, Keys: keys, DataDir: data, HookExe: exe, Log: logf})
	if err := runner.SandboxError(); err != nil {
		log.Printf("agent disabled (fail closed): %v", err)
	} else {
		log.Printf("agent: cursor-sdk-bridge %s (sdk %s) with the SDK sandbox", install.Manifest.BridgeVersion, install.Manifest.SDKVersion)
	}
	h := host.New(host.Options{
		Seam: seam, Version: appid.Version, Store: st, Agent: runner, Keys: keys, Templates: vibechat.Templates(),
		ProjectsDir: projects, WorktreesDir: filepath.Join(data, "worktrees"), Log: logf,
	})
	var once sync.Once
	return h, func() {
		once.Do(func() {
			h.Close()
			runner.Close()
		})
	}
}
