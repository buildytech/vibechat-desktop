// Package bridge runs the official cursor-sdk-bridge (github.com/cursor/sdk-bridge, protocol sdk.v1) as a child process
// and talks Connect JSON to it over loopback HTTP/1.1. The Cursor key reaches the bridge only through its environment
// (CURSOR_API_KEY); request bodies leave api_key empty so the bridge falls back to it. The SDK sandbox needs the native
// helper @cursor/sdk-<os>-<arch> next to the bridge executable (bin/node_modules/@cursor/...); without it the host
// reports agent_sandbox=false and refuses to start the agent (fail closed).
package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Manifest is the bridge archive's manifest.json.
type Manifest struct {
	BridgeVersion string `json:"bridgeVersion"`
	SDKVersion    string `json:"sdkVersion"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	Entrypoint    string `json:"entrypoint"`
	Protocol      string `json:"protocol"`
	Runtime       string `json:"runtime"`
}

// Install is a located bridge directory (the extracted standalone archive plus the sandbox helper).
type Install struct {
	Dir      string
	Manifest Manifest
}

// Executable is the bridge binary.
func (i Install) Executable() string {
	return filepath.Join(i.Dir, filepath.FromSlash(i.Manifest.Entrypoint))
}

// HelperPackage is "@cursor/sdk-<os>-<arch>" for this platform.
func HelperPackage() string { return "sdk-" + sdkOS() + "-" + sdkArch() }

// HelperDir is where the sandbox helper must sit: next to the bridge executable.
func (i Install) HelperDir() string {
	return filepath.Join(filepath.Dir(i.Executable()), "node_modules", "@cursor", HelperPackage())
}

// SandboxHelper is the helper's cursorsandbox binary.
func (i Install) SandboxHelper() string {
	name := "cursorsandbox"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(i.HelperDir(), "bin", name)
}

// HasSandboxHelper reports whether the helper is in place.
func (i Install) HasSandboxHelper() bool {
	st, err := os.Stat(i.SandboxHelper())
	return err == nil && !st.IsDir()
}

// ErrNotFound means no bridge directory was found.
var ErrNotFound = errors.New("cursor-sdk-bridge not found")

// Candidates are the directories searched for manifest.json, in order: VIBECHAT_BRIDGE_DIR, then next to the
// executable (bridge/), the Linux package layout (../lib/vibechat/bridge) and the macOS bundle (../Resources/bridge).
func Candidates(exe string, getenv func(string) string) []string {
	var out []string
	if d := getenv("VIBECHAT_BRIDGE_DIR"); d != "" {
		out = append(out, d)
	}
	dir := filepath.Dir(exe)
	out = append(out,
		filepath.Join(dir, "bridge"),
		filepath.Join(dir, "..", "lib", "vibechat", "bridge"),
		filepath.Join(dir, "..", "Resources", "bridge"),
	)
	return out
}

// Locate finds the bridge for this platform and checks its manifest (protocol sdk.v1, matching os/arch).
func Locate(exe string, getenv func(string) string) (Install, error) {
	var problems []string
	for _, dir := range Candidates(exe, getenv) {
		raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err != nil {
			continue
		}
		var m Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			problems = append(problems, dir+": bad manifest")
			continue
		}
		if m.Protocol != "sdk.v1" || m.OS != sdkOS() || m.Arch != sdkArch() || m.Entrypoint == "" {
			problems = append(problems, fmt.Sprintf("%s: %s %s/%s", dir, m.Protocol, m.OS, m.Arch))
			continue
		}
		abs, _ := filepath.Abs(dir)
		inst := Install{Dir: abs, Manifest: m}
		if _, err := os.Stat(inst.Executable()); err != nil {
			problems = append(problems, dir+": entrypoint missing")
			continue
		}
		return inst, nil
	}
	if len(problems) > 0 {
		return Install{}, fmt.Errorf("%w (%v)", ErrNotFound, problems)
	}
	return Install{}, ErrNotFound
}

func sdkOS() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

func sdkArch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}
