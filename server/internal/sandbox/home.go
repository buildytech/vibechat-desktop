// Package sandbox is the host's agent boundary on top of the Cursor SDK sandbox (which closes the network and writes
// outside the workspace for shells): a private agent home whose user-level .cursor/hooks.json runs this binary as a
// fail-closed hook on preToolUse and beforeReadFile (reads and file tools outside the workspace, writes to the agent's
// own config), and a check that the workspace carries no project-level .cursor/hooks.json or sandbox.json the agent
// could have written to loosen the boundary. Hooks are a filter; the SDK sandbox is the boundary.
package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// HookEvents are the hook events the host installs. preToolUse sees every tool (Shell and MCP included), so the
// per-kind events are not installed: each extra hook is one more process per tool call, and the SDK's hook runner
// occasionally fails to spawn one (EBADF from epoll_ctl), which — fail closed — blocks that tool call.
var HookEvents = []string{"preToolUse", "beforeReadFile"}

// HookCommand is the command line Cursor runs for event: this executable with --hook <event>.
func HookCommand(exe, event string) string {
	if runtime.GOOS == "windows" {
		return `"` + exe + `" --hook ` + event
	}
	return "'" + strings.ReplaceAll(exe, "'", `'\''`) + "' --hook " + event
}

// SandboxJSON is the user-level sandbox.json: shell reads limited to the workspace (plus the SDK's system runtime
// paths and readPaths), network closed. readPaths are the tool runtimes the agent's shell needs: the folders of bun
// and node and, on Linux, /proc (Bun's JavaScriptCore reads /proc/self/maps; without it bun aborts and `bun run`
// cannot find package.json). Other processes' /proc entries stay closed: the sandbox may not ptrace-read outside
// its domain, so e.g. the bridge's environment is not readable.
func SandboxJSON(readPaths []string) []byte {
	v := map[string]any{"readBoundary": "workspace", "networkAccess": false}
	if len(readPaths) > 0 {
		v["additionalReadPaths"] = readPaths
	}
	raw, _ := json.MarshalIndent(v, "", "  ")
	return append(raw, '\n')
}

// WriteHome writes <home>/.cursor/hooks.json and sandbox.json (0600) for exe.
func WriteHome(home, exe string, readPaths []string) error {
	dir := filepath.Join(home, ".cursor")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	hooks := map[string][]map[string]any{}
	for _, ev := range HookEvents {
		hooks[ev] = []map[string]any{{"command": HookCommand(exe, ev), "failClosed": true, "timeout": 15}}
	}
	raw, err := json.MarshalIndent(map[string]any{"version": 1, "hooks": hooks}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "sandbox.json"), SandboxJSON(readPaths), 0o600)
}

// projectOverrides are workspace files that would change the agent's hooks, sandbox or permissions.
var projectOverrides = []string{
	filepath.Join(".cursor", "hooks.json"),
	filepath.Join(".cursor", "sandbox.json"),
	filepath.Join(".cursor", "cli.json"),
	filepath.Join(".cursor", "permissions.json"),
	filepath.Join(".cursor", "mcp.json"),
	filepath.Join(".claude", "settings.json"),
	filepath.Join(".claude", "settings.local.json"),
}

// CheckWorkspace fails when the workspace carries a file that could loosen the boundary. The host runs it before
// every agent run (fail closed): templates never ship these files, and the hook denies writing them.
func CheckWorkspace(root string) error {
	for _, rel := range projectOverrides {
		if _, err := os.Lstat(filepath.Join(root, rel)); err == nil {
			return fmt.Errorf("workspace has %s; remove it to run the agent (it could change the agent's sandbox or hooks)", filepath.ToSlash(rel))
		}
	}
	return nil
}
