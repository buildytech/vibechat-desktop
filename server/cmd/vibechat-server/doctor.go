package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/buildytech/vibechat-desktop/internal/appid"
	"github.com/buildytech/vibechat-desktop/internal/gopin"
	"github.com/buildytech/vibechat-desktop/protocol/auth"
	"github.com/buildytech/vibechat-desktop/server/internal/agent"
	"github.com/buildytech/vibechat-desktop/server/internal/bridge"
	"github.com/buildytech/vibechat-desktop/server/internal/hostwire"
	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

// check is one doctor line.
type check struct {
	level  string // ok | warn | FAIL
	name   string
	detail string
}

func ok(name, detail string) check   { return check{"ok", name, detail} }
func warn(name, detail string) check { return check{"warn", name, detail} }
func bad(name, detail string) check  { return check{"FAIL", name, detail} }

// doctor checks that this machine can host agents safely; exit 1 on any FAIL.
func doctor(p paths, listen string, out io.Writer) int {
	var cs []check
	cs = append(cs, ok("version", "vibechat-server "+appid.Version+" "+runtime.GOOS+"/"+runtime.GOARCH))
	if runtime.GOOS != "linux" {
		cs = append(cs, bad("system", "the server runs on Debian 13 only"))
	} else {
		raw, err := os.ReadFile("/etc/os-release")
		if err != nil {
			raw, _ = os.ReadFile("/usr/lib/os-release")
		}
		cs = append(cs, osCheck(string(raw)))
	}
	if _, err := loopbackAddr(listen); err != nil {
		cs = append(cs, bad("listen", err.Error()))
	} else {
		cs = append(cs, ok("listen", listen+" (loopback only)"))
	}
	if st, err := os.Stat(p.data); err != nil {
		cs = append(cs, bad("data", err.Error()))
	} else if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		cs = append(cs, warn("data", fmt.Sprintf("%s is %v; 0700 keeps projects private", p.data, st.Mode().Perm())))
	} else {
		cs = append(cs, ok("data", p.data))
	}
	if _, err := auth.LoadTokenAuth(p.tokenHash()); err != nil {
		cs = append(cs, bad("host token", err.Error()))
	} else {
		cs = append(cs, ok("host token", p.tokenHash()+" (sha256 only)"))
	}
	keys := p.keys()
	if err := keys.Check(); err != nil {
		cs = append(cs, bad("cursor key", err.Error()))
	} else if k, _ := keys.Key(); k == "" {
		cs = append(cs, warn("cursor key", "not set yet: Settings in the app or `vibechat-server set-key`"))
	} else {
		cs = append(cs, ok("cursor key", p.cursorKey()+" (0600)"))
	}
	cs = append(cs, kernelChecks()...)
	cs = append(cs, sandboxCheck(p))
	cs = append(cs, isolationCheck(p))
	for _, tool := range []string{"bun", "git"} {
		if path, err := lookTool(p, tool); err != nil {
			cs = append(cs, warn(tool, "not on PATH: projects need it"))
		} else {
			v, _ := procgroup.Command(path, "--version").Output()
			cs = append(cs, ok(tool, path+" "+strings.TrimSpace(string(v))))
		}
	}
	cs = append(cs, goCheck(p))
	code := 0
	for _, c := range cs {
		fmt.Fprintf(out, "%-4s %-11s %s\n", c.level, c.name, c.detail)
		if c.level == "FAIL" {
			code = 1
		}
	}
	return code
}

// goCheck: the pinned Go of Go templ projects (/opt/vibechat/go, on the service's PATH).
func goCheck(p paths) check {
	path, err := lookTool(p, "go")
	if err != nil {
		return warn("go", "not installed: Go templ projects need Go "+gopin.Version+" (`sudo vibechat-server ensure-go`)")
	}
	cmd := procgroup.Command(path, "env", "GOVERSION")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOENV=off")
	v, _ := cmd.Output()
	have := strings.TrimSpace(string(v))
	if have != "go"+gopin.Version {
		return warn("go", path+" is "+have+", this release pins go"+gopin.Version+" (`sudo vibechat-server ensure-go`)")
	}
	return ok("go", path+" "+have)
}

// lookTool finds a project tool the way the service does: its PATH starts with <data>/.bun/bin (where the setup
// wizard puts Bun, HOME of the vibechat user), which a doctor run from a shell usually does not have.
func lookTool(p paths, tool string) (string, error) {
	if path, err := exec.LookPath(tool); err == nil {
		return path, nil
	}
	if tool == "go" {
		return exec.LookPath(filepath.Join(gopin.Bin, "go"))
	}
	return exec.LookPath(filepath.Join(p.data, ".bun", "bin", tool))
}

// sandboxCheck runs the agent runtime's own gate: bridge found, helper present, `cursorsandbox --preflight-only`.
func sandboxCheck(p paths) check {
	exe := hostwire.Exe()
	install, locErr := bridge.Locate(exe, os.Getenv)
	tmp, err := os.MkdirTemp("", "vibechat-doctor-")
	if err != nil {
		return bad("sandbox", err.Error())
	}
	defer os.RemoveAll(tmp)
	r := agent.NewRunner(agent.RunnerOptions{Install: install, Located: locErr, Keys: p.keys(), DataDir: tmp, HookExe: exe, Log: func(s string) { log.Print(s) }})
	defer r.Close()
	if err := r.SandboxError(); err != nil {
		return bad("sandbox", err.Error())
	}
	if r.Unsandboxed() {
		return bad("sandbox", "the agent would run without the SDK sandbox")
	}
	return ok("sandbox", "cursorsandbox --preflight-only passed (bridge "+install.Manifest.Describe()+")")
}

// isolationCheck runs a probe the way the server runs bun install and the dev server (package isolate): it must
// not see the secrets folder, the server's /proc entry or a key in its environment.
func isolationCheck(p paths) check {
	if runtime.GOOS != "linux" {
		return warn("tools", "project tools run with a clean environment only (no bubblewrap on "+runtime.GOOS+")")
	}
	isolate.Configure(isolate.Config{Hide: []string{p.data}, Require: true})
	dir, err := os.MkdirTemp("", "vibechat-doctor-tools-")
	if err != nil {
		return bad("tools", err.Error())
	}
	defer os.RemoveAll(dir)
	probe := `[ -e "$1" ] && echo "secrets visible"; [ -r "/proc/$2/environ" ] && echo "server /proc visible"; env | grep -q '^CURSOR_API_KEY=' && echo "key in env"; true`
	argv, env, mode, err := isolate.Command([]string{"/bin/sh", "-c", probe, "sh", filepath.Join(p.data, "secrets"), strconv.Itoa(os.Getpid())}, isolate.Spec{Dir: dir})
	if err != nil {
		return bad("tools", err.Error())
	}
	cmd := procgroup.Command(argv[0], argv[1:]...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return bad("tools", fmt.Sprintf("%s probe: %v %s", mode, err, strings.TrimSpace(string(out))))
	}
	if leak := strings.TrimSpace(string(out)); leak != "" {
		return bad("tools", mode+": "+strings.ReplaceAll(leak, "\n", ", "))
	}
	return ok("tools", "bun install and the dev server run in "+mode+" without "+p.data)
}

// supportedOS is the only server system for now (the owner's decision; Ubuntu is not planned).
const supportedOS = "Debian 13 (trixie)"

// osCheck reads os-release: ok on Debian 13, a clear warning on anything else.
func osCheck(osRelease string) check {
	kv := map[string]string{}
	for _, line := range strings.Split(osRelease, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			kv[k] = strings.Trim(v, `"'`)
		}
	}
	name := kv["PRETTY_NAME"]
	if name == "" {
		name = strings.TrimSpace(kv["ID"] + " " + kv["VERSION_ID"])
	}
	if name == "" {
		name = "unknown (no /etc/os-release)"
	}
	if kv["ID"] == "debian" && kv["VERSION_ID"] == "13" {
		return ok("system", name)
	}
	return warn("system", name+": NOT SUPPORTED. VibeChat server requires "+supportedOS+"; other systems (Ubuntu included) are not supported for now")
}
