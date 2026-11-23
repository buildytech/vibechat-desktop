package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Decision is a permission hook's answer.
type Decision struct {
	Permission   string `json:"permission"`
	UserMessage  string `json:"user_message,omitempty"`
	AgentMessage string `json:"agent_message,omitempty"`
}

func allow() Decision { return Decision{Permission: "allow"} }

func deny(why string) Decision {
	return Decision{Permission: "deny", UserMessage: why, AgentMessage: "Blocked by the VibeChat host: " + why + ". Stay inside the project folder; the network is closed."}
}

// HookInput is the part of a hook's stdin the host reads.
type HookInput struct {
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	FilePath       string          `json:"file_path"`
	Command        string          `json:"command"`
	Cwd            string          `json:"cwd"`
	WorkspaceRoots []string        `json:"workspace_roots"`
}

// Root picks the workspace root: CURSOR_PROJECT_DIR, else the first workspace root. "" means unknown (deny).
func Root(in HookInput, getenv func(string) string) string {
	if r := getenv("CURSOR_PROJECT_DIR"); r != "" {
		return r
	}
	if len(in.WorkspaceRoots) > 0 {
		return in.WorkspaceRoots[0]
	}
	return ""
}

// Decide answers one hook event.
func Decide(event string, in HookInput, root string) Decision {
	if root == "" {
		return deny("unknown workspace")
	}
	rootReal := realish(root)
	switch event {
	case "beforeReadFile":
		if in.FilePath == "" || !Inside(rootReal, resolve(rootReal, in.FilePath)) {
			return deny("read outside the project: " + in.FilePath)
		}
		return allow()
	case "beforeMCPExecution":
		return deny("MCP tools are off in VibeChat")
	case "beforeShellExecution":
		return shellDecision(in.Command)
	case "preToolUse":
		return toolDecision(in, rootReal)
	}
	return allow()
}

var netCommand = regexp.MustCompile(`(^|[\s;|&(])(curl|wget|nc|ncat|ssh|scp|sftp|telnet|ftp)(\s|$)`)

// configPath matches the agent-boundary files in the workspace (see projectOverrides).
var configPath = regexp.MustCompile(`\.cursor[/\\](hooks|sandbox|cli|permissions|mcp)\.json|\.claude[/\\]settings|\.git[/\\]hooks`)

func shellDecision(cmd string) Decision {
	if netCommand.MatchString(cmd) {
		return deny("network commands are blocked")
	}
	if configPath.MatchString(cmd) {
		return deny("the agent's own config is read-only")
	}
	return allow()
}

// pathKeys are tool_input fields that name a file or folder.
var pathKeys = []string{"file_path", "path", "target_file", "target_directory", "directory", "dir", "cwd", "target_notebook", "glob_pattern"}

func toolDecision(in HookInput, root string) Decision {
	name := in.ToolName
	if strings.HasPrefix(name, "MCP:") || strings.HasPrefix(strings.ToLower(name), "mcp") {
		return deny("MCP tools are off in VibeChat")
	}
	var input map[string]any
	_ = json.Unmarshal(in.ToolInput, &input)
	if strings.EqualFold(name, "Shell") {
		cmd, _ := input["command"].(string)
		if d := shellDecision(cmd); d.Permission == "deny" {
			return d
		}
		if wd, _ := input["working_directory"].(string); wd != "" && !Inside(root, resolve(root, wd)) {
			return deny("shell outside the project: " + wd)
		}
		return allow()
	}
	for _, k := range pathKeys {
		var vals []string
		switch v := input[k].(type) {
		case string:
			vals = []string{v}
		case []any:
			for _, x := range v {
				if s, ok := x.(string); ok {
					vals = append(vals, s)
				}
			}
		}
		for _, p := range vals {
			if p == "" {
				continue
			}
			if k == "glob_pattern" && !filepath.IsAbs(p) && !strings.HasPrefix(p, "..") {
				continue
			}
			abs := resolve(root, p)
			if !Inside(root, abs) {
				return deny(name + " outside the project: " + p)
			}
			rel, _ := filepath.Rel(root, abs)
			if configPath.MatchString(filepath.ToSlash(rel)) && !strings.EqualFold(name, "Read") {
				return deny("the agent's own config is read-only: " + filepath.ToSlash(rel))
			}
		}
	}
	for _, k := range []string{"paths", "files"} {
		if arr, ok := input[k].([]any); ok {
			for _, x := range arr {
				if s, ok := x.(string); ok && !Inside(root, resolve(root, s)) {
					return deny(name + " outside the project: " + s)
				}
			}
		}
	}
	return allow()
}

// resolve makes p absolute against root and follows symlinks of the part that exists.
func resolve(root, p string) string {
	if strings.HasPrefix(p, "~") {
		return "/nonexistent-home" + p[1:]
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return realish(filepath.Clean(p))
}

// realish is EvalSymlinks of the longest existing prefix of p, with the rest appended.
func realish(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Inside reports whether p is root or below it (both cleaned, absolute).
func Inside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// RunHook is the --hook entry point: read stdin, decide, write the JSON answer. Any failure answers deny.
func RunHook(event string, stdin []byte, getenv func(string) string) []byte {
	var in HookInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		out, _ := json.Marshal(deny("unreadable hook input"))
		return out
	}
	out, _ := json.Marshal(Decide(event, in, Root(in, getenv)))
	return out
}

// Getenv is os.Getenv (a seam for tests).
var Getenv = os.Getenv
