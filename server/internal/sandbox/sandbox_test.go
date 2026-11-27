package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func in(tool string, input map[string]any) HookInput {
	raw, _ := json.Marshal(input)
	return HookInput{ToolName: tool, ToolInput: raw}
}

func TestDecide(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "secret.txt")
	_ = os.Symlink(filepath.Dir(root), filepath.Join(root, "escape"))
	cases := []struct {
		name  string
		event string
		in    HookInput
		allow bool
	}{
		{"read inside", "beforeReadFile", HookInput{FilePath: filepath.Join(root, "src/a.ts")}, true},
		{"read relative inside", "beforeReadFile", HookInput{FilePath: "src/a.ts"}, true},
		{"read outside", "beforeReadFile", HookInput{FilePath: outside}, false},
		{"read via symlink", "beforeReadFile", HookInput{FilePath: filepath.Join(root, "escape", "secret.txt")}, false},
		{"read dotdot", "beforeReadFile", HookInput{FilePath: "../secret.txt"}, false},
		{"Read tool outside", "preToolUse", in("Read", map[string]any{"file_path": outside}), false},
		{"Write inside", "preToolUse", in("Write", map[string]any{"file_path": "src/App.svelte"}), true},
		{"Write hooks.json", "preToolUse", in("Write", map[string]any{"file_path": ".cursor/hooks.json"}), false},
		{"Edit sandbox.json", "preToolUse", in("Edit", map[string]any{"path": filepath.Join(root, ".cursor", "sandbox.json")}), false},
		{"Grep outside", "preToolUse", in("Grep", map[string]any{"pattern": "x", "path": "/etc"}), false},
		{"Glob relative", "preToolUse", in("Glob", map[string]any{"glob_pattern": "**/*.svelte"}), true},
		{"Glob absolute outside", "preToolUse", in("Glob", map[string]any{"glob_pattern": "/home/**"}), false},
		{"Shell build", "preToolUse", in("Shell", map[string]any{"command": "bun run build"}), true},
		{"Shell curl", "preToolUse", in("Shell", map[string]any{"command": "curl https://example.com"}), false},
		{"Shell cwd outside", "preToolUse", in("Shell", map[string]any{"command": "ls", "working_directory": "/"}), false},
		{"Shell writes hooks", "beforeShellExecution", HookInput{Command: "echo {} > .cursor/hooks.json"}, false},
		{"MCP", "preToolUse", in("MCP:fetch", map[string]any{}), false},
		{"home path", "preToolUse", in("Read", map[string]any{"file_path": "~/.ssh/id_rsa"}), false},
	}
	for _, c := range cases {
		d := Decide(c.event, c.in, root)
		if (d.Permission == "allow") != c.allow {
			t.Errorf("%s: %s (%s)", c.name, d.Permission, d.UserMessage)
		}
	}
	if Decide("beforeReadFile", HookInput{FilePath: "a"}, "").Permission != "deny" {
		t.Error("unknown root allowed")
	}
}

func TestRunHookFailsClosed(t *testing.T) {
	out := RunHook("preToolUse", []byte("not json"), func(string) string { return "/x" })
	if !strings.Contains(string(out), `"deny"`) {
		t.Fatal(string(out))
	}
	out = RunHook("beforeReadFile", []byte(`{"file_path":"/etc/passwd"}`), func(k string) string {
		if k == "CURSOR_PROJECT_DIR" {
			return t.TempDir()
		}
		return ""
	})
	if !strings.Contains(string(out), `"deny"`) {
		t.Fatal(string(out))
	}
}

func TestWriteHomeAndCheckWorkspace(t *testing.T) {
	home := t.TempDir()
	if err := WriteHome(home, "/opt/vibe chat/vibechat", []string{"/proc"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".cursor", "hooks.json"))
	var cfg struct {
		Version int `json:"version"`
		Hooks   map[string][]struct {
			Command    string `json:"command"`
			FailClosed bool   `json:"failClosed"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.Version != 1 {
		t.Fatal(err, string(raw))
	}
	for _, ev := range HookEvents {
		h := cfg.Hooks[ev]
		if len(h) != 1 || !h[0].FailClosed || !strings.Contains(h[0].Command, "--hook "+ev) {
			t.Errorf("%s: %+v", ev, h)
		}
	}
	ws := t.TempDir()
	if err := CheckWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(ws, ".cursor"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, ".cursor", "sandbox.json"), []byte(`{"networkPolicy":{"allow":["*"]}}`), 0o644)
	if err := CheckWorkspace(ws); err == nil {
		t.Fatal("loosened workspace accepted")
	}
}

func TestSandboxJSONKeepsNetworkClosed(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal(SandboxJSON([]string{"/proc"}), &v); err != nil {
		t.Fatal(err)
	}
	if v["readBoundary"] != "workspace" || v["networkAccess"] != false || v["additionalReadPaths"].([]any)[0] != "/proc" {
		t.Fatal(v)
	}
	if _, ok := v["networkPolicy"]; ok {
		t.Fatal("network policy must not be set")
	}
}

func TestEnsureNodeLinksBun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no symlink shim on Windows")
	}
	dir := t.TempDir()
	bun := filepath.Join(dir, "bun")
	if err := os.WriteFile(bun, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	look := func(name string) (string, error) {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			return "", os.ErrNotExist
		}
		return p, nil
	}
	got, err := EnsureNode(look)
	if err != nil || got != filepath.Join(dir, "node") {
		t.Fatalf("EnsureNode = %q, %v", got, err)
	}
	if target, _ := os.Readlink(got); target != "bun" {
		t.Fatalf("node links to %q", target)
	}
	if again, err := EnsureNode(look); err != nil || again != got {
		t.Fatalf("second EnsureNode = %q, %v", again, err)
	}
}
