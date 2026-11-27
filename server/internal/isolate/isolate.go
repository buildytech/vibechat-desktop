// Package isolate runs a project's own tools (bun install, the Vite dev server, a build) away from the server's
// secrets. Those tools execute code the agent can write (package.json scripts, vite.config.*), so they get:
//
//   - a clean environment: an allowlist of names (PATH, HOME, locale, proxies, Bun's own), never the server's
//     environment, so no key or token set there reaches them;
//   - on Linux, a bubblewrap jail (the same bwrap the agent's SDK sandbox uses): the system read-only, the server's
//     data folder (secrets, auth, state, the agent home, other projects) replaced by an empty tmpfs, only the
//     project folder (and, for an install, the Bun cache) writable, a private /tmp and its own PID namespace, so
//     /proc of the server and the agent bridge (whose environment carries the Cursor key) is not visible either.
//
// The network stays shared: bun install needs the registry and the preview proxy reaches the dev server on
// 127.0.0.1. Without a usable bwrap the tools run with the clean environment only, unless Config.Require is set
// (the packaged service sets it): then they do not run at all.
package isolate

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

// Config is the process-wide isolation setting (Configure, once at start).
type Config struct {
	// Hide are folders the tools must not see at all (the server's data folder).
	Hide []string
	// Require: on Linux, refuse to run a tool when bubblewrap is not usable (instead of the clean environment only).
	Require bool
}

// Spec is one tool run.
type Spec struct {
	Dir      string   // the project folder: the working directory, read-write
	Writable []string // more read-write folders (the Bun cache for an install)
	Env      []string // KEY=VALUE on top of the clean environment
}

// Modes reported by Command.
const (
	ModeJail     = "bwrap"
	ModeCleanEnv = "clean-env"
)

var (
	mu     sync.Mutex
	cfg    Config
	probed bool
	jailOK error // nil when bwrap works
	bwrap  string
)

// Configure sets the isolation for the process.
func Configure(c Config) {
	mu.Lock()
	defer mu.Unlock()
	cfg = Config{Require: c.Require}
	for _, h := range c.Hide {
		if h == "" {
			continue
		}
		if abs, err := filepath.Abs(h); err == nil {
			h = abs
		}
		cfg.Hide = append(cfg.Hide, filepath.Clean(h))
	}
}

// RequireFromEnv reads VIBECHAT_TOOLS_ISOLATION=require (the packaged service's unit sets it).
func RequireFromEnv() bool { return os.Getenv("VIBECHAT_TOOLS_ISOLATION") == "require" }

// allowed are the environment names a tool may see (case-insensitive on Windows).
var allowed = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TZ", "TERM", "NO_COLOR", "FORCE_COLOR",
	"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "LC_MESSAGES",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	"BUN_INSTALL", "BUN_INSTALL_CACHE_DIR", "BUN_CONFIG_REGISTRY", "DO_NOT_TRACK",
	"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR",
	// Windows
	"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "TEMP", "TMP", "USERPROFILE", "USERNAME",
	"APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)", "COMMONPROGRAMFILES",
	"HOMEDRIVE", "HOMEPATH", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "OS",
}

// CleanEnv is environ filtered to the allowlist (nothing named like a key or token passes), then extra.
func CleanEnv(environ []string, extra ...string) []string {
	var out []string
	for _, kv := range environ {
		name, _, found := strings.Cut(kv, "=")
		if !found || name == "" {
			continue
		}
		for _, n := range allowed {
			if sameName(name, n) {
				out = append(out, kv)
				break
			}
		}
	}
	return setAll(out, extra)
}

// sameName compares environment names the way the system does (case-insensitive on Windows).
func sameName(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// setAll sets each KEY=VALUE of extra, replacing an earlier value of the same name.
func setAll(env, extra []string) []string {
	for _, kv := range extra {
		name, _, _ := strings.Cut(kv, "=")
		kept := make([]string, 0, len(env)+1)
		for _, e := range env {
			if n, _, _ := strings.Cut(e, "="); !sameName(n, name) {
				kept = append(kept, e)
			}
		}
		env = append(kept, kv)
	}
	return env
}

func getenv(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if n, v, _ := strings.Cut(env[i], "="); sameName(n, name) {
			return v
		}
	}
	return ""
}

// BunCache is the Bun install cache of the server user (BUN_INSTALL_CACHE_DIR, else $BUN_INSTALL/install/cache,
// else ~/.bun/install/cache): an install binds it read-write and points Bun at it.
func BunCache() string {
	if d := os.Getenv("BUN_INSTALL_CACHE_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("BUN_INSTALL"); d != "" {
		return filepath.Join(d, "install", "cache")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".bun", "install", "cache")
}

// GoCache is the shared Go folder of the server user (VIBECHAT_GO_CACHE, else ~/.cache/vibechat-go): the module cache,
// the build cache and GOPATH of every Go project's tools, bound read-write into their jail.
func GoCache() string {
	if d := os.Getenv("VIBECHAT_GO_CACHE"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cache", "vibechat-go")
}

// GoEnv is the environment of a Go tool run: the shared caches, modules read-only to the build (-mod=readonly: a
// build never rewrites go.mod or go.sum), the installed toolchain only (GOTOOLCHAIN=local: go.mod cannot make the
// tool download another one), no cgo, modules only through the Go proxy and checked against the checksum database.
func GoEnv(cache string) []string {
	return []string{
		"GOPATH=" + filepath.Join(cache, "path"),
		"GOMODCACHE=" + filepath.Join(cache, "mod"),
		"GOCACHE=" + filepath.Join(cache, "build"),
		"GOFLAGS=-mod=readonly",
		"GOTOOLCHAIN=local",
		"GOTELEMETRY=off",
		"GOENV=off",
		"CGO_ENABLED=0",
		"GOPROXY=https://proxy.golang.org",
		"GOSUMDB=sum.golang.org",
	}
}

// Mode answers how tools run on this machine now: ModeJail, or ModeCleanEnv with why the jail is not used
// (an error when Config.Require makes that fatal).
func Mode() (string, error) {
	mu.Lock()
	defer mu.Unlock()
	return modeLocked()
}

func modeLocked() (string, error) {
	if runtime.GOOS != "linux" {
		return ModeCleanEnv, nil
	}
	if !probed {
		probed = true
		bwrap, jailOK = probe()
	}
	if jailOK == nil {
		return ModeJail, nil
	}
	if cfg.Require {
		return "", fmt.Errorf("project tools need bubblewrap isolation and it does not work here: %v", jailOK)
	}
	return ModeCleanEnv, jailOK
}

func probe() (string, error) {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return "", errors.New("bwrap not found (install the bubblewrap package)")
	}
	cmd := procgroup.Command(path, "--die-with-parent", "--unshare-pid", "--unshare-ipc", "--ro-bind", "/", "/",
		"--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--", "/bin/true")
	cmd.Env = CleanEnv(os.Environ())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %v %s", path, err, strings.TrimSpace(out.String()))
	}
	return path, nil
}

// Command answers the argv and environment that run argv for s: wrapped in bwrap (ModeJail) or as is with the
// clean environment (ModeCleanEnv). The returned mode is for logs.
func Command(argv []string, s Spec) (out []string, env []string, mode string, err error) {
	if len(argv) == 0 {
		return nil, nil, "", errors.New("isolate: empty argv")
	}
	mu.Lock()
	defer mu.Unlock()
	mode, merr := modeLocked()
	if mode == "" {
		return nil, nil, "", merr
	}
	env = CleanEnv(os.Environ(), s.Env...)
	if mode != ModeJail {
		return argv, env, mode, nil
	}
	dir, err := filepath.Abs(s.Dir)
	if err != nil {
		return nil, nil, "", err
	}
	hidden := func(p string) bool {
		for _, h := range cfg.Hide {
			if p == h || strings.HasPrefix(p, h+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	// HOME inside a hidden folder (the service's HOME is its data folder) becomes the jail's private /tmp.
	if h := getenv(env, "HOME"); h == "" || hidden(filepath.Clean(h)) {
		env = setAll(env, []string{"HOME=/tmp"})
	}
	a := []string{bwrap, "--die-with-parent", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup-try",
		"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp"}
	for _, h := range cfg.Hide {
		if st, err := os.Stat(h); err == nil && st.IsDir() {
			a = append(a, "--tmpfs", h)
		}
	}
	// The runtimes on PATH that live in a hidden folder (Bun in <data>/.bun/bin) come back read-only.
	seen := map[string]bool{}
	for _, d := range filepath.SplitList(getenv(env, "PATH")) {
		if d == "" || !filepath.IsAbs(d) || seen[d] {
			continue
		}
		seen[d] = true
		if hidden(filepath.Clean(d)) && !strings.HasPrefix(dir+string(filepath.Separator), filepath.Clean(d)+string(filepath.Separator)) {
			if st, err := os.Stat(d); err == nil && st.IsDir() {
				a = append(a, "--ro-bind", d, d)
			}
		}
	}
	// An absolute program under /tmp or a hidden folder (a test's helper binary) comes back read-only too.
	if exe := argv[0]; filepath.IsAbs(exe) {
		d := filepath.Dir(filepath.Clean(exe))
		if (hidden(d) || d == "/tmp" || strings.HasPrefix(d, "/tmp/")) && !seen[d] {
			a = append(a, "--ro-bind", d, d)
		}
	}
	for _, w := range append(append([]string{}, s.Writable...), dir) {
		if w == "" {
			continue
		}
		if err := os.MkdirAll(w, 0o700); err != nil {
			return nil, nil, "", err
		}
		a = append(a, "--bind", w, w)
	}
	a = append(a, "--chdir", dir, "--")
	return append(a, argv...), env, mode, nil
}
