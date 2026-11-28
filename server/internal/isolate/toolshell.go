package isolate

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

// A coding agent's shell tool runs what the model writes. The agent process itself (Pi) holds the way to the model
// provider (a socket, the gateway token in older setups) and its own environment and /proc; the model's shell
// commands must reach none of that. So the agent is configured to start its shell through a small wrapper
// (ToolShellScript) that puts every command in a second, nested bubblewrap jail: a network namespace with nothing in it
// (no gateway, no provider, no internet), its own PID namespace and /proc (the agent process and its environment are
// not visible), and tmpfs over the folders the command must not see (the gateway socket, the agent's own folders).
// The project folder and everything else the agent may write stays as it is.

// BwrapPath is the bubblewrap binary in use ("" when the jail is not usable here).
func BwrapPath() string {
	if JailAvailable() != nil {
		return ""
	}
	mu.Lock()
	defer mu.Unlock()
	return bwrap
}

// ToolShellScript is the wrapper the agent starts instead of bash: it runs bash (given as an absolute path) with the
// agent's arguments in the nested jail, masking each folder in mask with an empty tmpfs and dropping the named
// environment variables.
func ToolShellScript(bwrapPath, bashPath string, mask []string, unsetenv ...string) (string, error) {
	for _, p := range append([]string{bwrapPath, bashPath}, mask...) {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\n\x00") {
			return "", fmt.Errorf("tool shell: %q is not an absolute path", p)
		}
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# BuildY VibeChat: the agent's shell runs in a nested jail with no network and no sight of the agent.\n")
	if len(unsetenv) > 0 {
		for _, v := range unsetenv {
			if !envName.MatchString(v) {
				return "", fmt.Errorf("tool shell: %q is not an environment variable name", v)
			}
		}
		b.WriteString("unset " + strings.Join(unsetenv, " ") + "\n")
	}
	b.WriteString("exec " + shq(bwrapPath) + " --die-with-parent --unshare-net --unshare-pid --unshare-ipc --unshare-uts --dev-bind / / --proc /proc")
	for _, m := range mask {
		b.WriteString(" --tmpfs " + shq(m))
	}
	b.WriteString(" --chdir \"$PWD\" -- " + shq(bashPath) + " \"$@\"\n")
	return b.String(), nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var (
	nestedOnce sync.Once
	nestedErr  error
)

// NestedJailAvailable is nil when a jail with its own network namespace works here and, inside it, so does the nested
// jail of the agent's shell (user namespaces can be switched off from within, or limited in depth): what the agent
// needs to run its shell safely. Linux only; the answer is cached.
func NestedJailAvailable() error {
	if runtime.GOOS != "linux" {
		return errors.New("bubblewrap isolation is Linux only")
	}
	if err := JailAvailable(); err != nil {
		return err
	}
	nestedOnce.Do(func() {
		path := BwrapPath()
		inner := path + " --unshare-net --unshare-pid --unshare-ipc --dev-bind / / --proc /proc --tmpfs /tmp/probe -- /bin/true"
		cmd := procgroup.Command(path, "--die-with-parent", "--unshare-pid", "--unshare-ipc", "--unshare-net", "--ro-bind", "/", "/",
			"--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--", "/bin/sh", "-c", inner)
		cmd.Env = CleanEnv(os.Environ())
		if out, err := cmd.CombinedOutput(); err != nil {
			nestedErr = fmt.Errorf("the nested jail of the agent's shell does not work here: %v %s", err, strings.TrimSpace(string(out)))
		}
	})
	return nestedErr
}

// ResolveBash finds bash for the tool shell (an absolute path).
func ResolveBash() (string, error) {
	p, err := exec.LookPath("bash")
	if err != nil {
		return "", errors.New("bash not found")
	}
	return filepath.Abs(p)
}
