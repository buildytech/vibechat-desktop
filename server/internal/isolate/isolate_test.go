package isolate

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

func TestCleanEnvKeepsOnlyTheAllowlist(t *testing.T) {
	env := CleanEnv([]string{"PATH=/bin", "HOME=/h", "CURSOR_API_KEY=k", "XAI_API_KEY=x", "NPM_TOKEN=n", "GITHUB_TOKEN=g",
		"VIBECHAT_DATA_DIR=/d", "LANG=C.UTF-8", "https_proxy=http://p"}, "PORT=1", "HOME=/tmp")
	got := strings.Join(env, " ")
	for _, want := range []string{"PATH=/bin", "LANG=C.UTF-8", "https_proxy=http://p", "PORT=1", "HOME=/tmp"} {
		if !strings.Contains(" "+got+" ", " "+want+" ") {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	for _, bad := range []string{"CURSOR_API_KEY", "XAI_API_KEY", "NPM_TOKEN", "GITHUB_TOKEN", "VIBECHAT_DATA_DIR", "HOME=/h"} {
		if strings.Contains(got, bad) {
			t.Errorf("%s passed: %s", bad, got)
		}
	}
}

// TestJailHidesTheDataFolder: a tool run (what a postinstall script or vite.config could do) cannot read the
// secrets, cannot see the server's /proc entry, does not get the server's environment, and can write the project.
func TestJailHidesTheDataFolder(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux only")
	}
	if _, err := probe(); err != nil {
		t.Skipf("bwrap not usable here: %v", err)
	}
	data := t.TempDir()
	key := filepath.Join(data, "secrets", "cursor-key")
	proj := filepath.Join(data, "projects", "p")
	for _, d := range []string{filepath.Dir(key), proj} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(key, []byte("crsr_not_a_real_key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CURSOR_API_KEY", "crsr_env_value")
	Configure(Config{Hide: []string{data}, Require: true})
	t.Cleanup(func() { Configure(Config{}) })
	script := `set -u
if cat "$1" 2>/dev/null; then echo "LEAK:key"; fi
if ls "$(dirname "$1")" >/dev/null 2>&1; then echo "LEAK:secrets-dir"; fi
if [ -r "/proc/$2/environ" ]; then echo "LEAK:proc"; fi
if env | grep -q CURSOR_API_KEY; then echo "LEAK:env"; fi
echo ok > written
echo done`
	argv, env, mode, err := Command([]string{"/bin/sh", "-c", script, "sh", key, strconv.Itoa(os.Getpid())}, Spec{Dir: proj})
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeJail {
		t.Fatalf("mode %s", mode)
	}
	cmd := procgroup.Command(argv[0], argv[1:]...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.Contains(string(out), "LEAK") || !strings.Contains(string(out), "done") {
		t.Fatalf("jail output: %s", out)
	}
	if b, err := os.ReadFile(filepath.Join(proj, "written")); err != nil || strings.TrimSpace(string(b)) != "ok" {
		t.Fatalf("the project is not writable from the jail: %v", err)
	}
}

func TestRequireFailsWithoutBwrap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	mu.Lock()
	oldProbed, oldErr, oldCfg := probed, jailOK, cfg
	probed, jailOK, cfg = true, os.ErrNotExist, Config{Require: true}
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); probed, jailOK, cfg = oldProbed, oldErr, oldCfg; mu.Unlock() })
	if _, _, _, err := Command([]string{"/bin/true"}, Spec{Dir: t.TempDir()}); err == nil {
		t.Fatal("Require ran a tool without the jail")
	}
	mu.Lock()
	cfg.Require = false
	mu.Unlock()
	if _, _, mode, err := Command([]string{"/bin/true"}, Spec{Dir: t.TempDir()}); err != nil || mode != ModeCleanEnv {
		t.Fatalf("without Require: mode %q err %v", mode, err)
	}
}
