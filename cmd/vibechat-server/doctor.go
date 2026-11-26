package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/buildytech/vibechat-desktop/internal/agent"
	"github.com/buildytech/vibechat-desktop/internal/appid"
	"github.com/buildytech/vibechat-desktop/internal/auth"
	"github.com/buildytech/vibechat-desktop/internal/bridge"
	"github.com/buildytech/vibechat-desktop/internal/hostwire"
	"github.com/buildytech/vibechat-desktop/internal/procgroup"
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
	for _, tool := range []string{"bun", "git"} {
		if path, err := exec.LookPath(tool); err != nil {
			cs = append(cs, warn(tool, "not on PATH: projects need it"))
		} else {
			v, _ := procgroup.Command(path, "--version").Output()
			cs = append(cs, ok(tool, path+" "+strings.TrimSpace(string(v))))
		}
	}
	code := 0
	for _, c := range cs {
		fmt.Fprintf(out, "%-4s %-11s %s\n", c.level, c.name, c.detail)
		if c.level == "FAIL" {
			code = 1
		}
	}
	return code
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
	return ok("sandbox", "cursorsandbox --preflight-only passed (bridge "+install.Manifest.BridgeVersion+")")
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
