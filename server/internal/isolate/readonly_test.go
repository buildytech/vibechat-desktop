package isolate

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

// RequireJail is for programs that must never run outside bubblewrap (the RU contour's Pi), even where the process-wide
// Require is off: without a working jail Command refuses, with a clean environment as the only alternative it still
// refuses.
func TestRequireJailRefusesWithoutBwrapEvenWhenRequireIsOff(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	mu.Lock()
	oldProbed, oldErr, oldCfg := probed, jailOK, cfg
	probed, jailOK, cfg = true, os.ErrNotExist, Config{}
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); probed, jailOK, cfg = oldProbed, oldErr, oldCfg; mu.Unlock() })
	if _, _, mode, err := Command([]string{"/bin/true"}, Spec{Dir: t.TempDir()}); err != nil || mode != ModeCleanEnv {
		t.Fatalf("an ordinary tool still runs with a clean environment: %q %v", mode, err)
	}
	_, _, _, err := Command([]string{"/bin/true"}, Spec{Dir: t.TempDir(), RequireJail: true})
	if err == nil || !strings.Contains(err.Error(), "must run in the bubblewrap jail") {
		t.Fatalf("%v", err)
	}
	if JailAvailable() == nil {
		t.Fatal("JailAvailable lies")
	}
}

// ReadOnly binds a runtime that lives under the hidden data folder (Node, Pi) into the jail without making it
// writable.
func TestReadOnlyFoldersAreVisibleButNotWritable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux only")
	}
	if _, err := probe(); err != nil {
		t.Skipf("bwrap not usable here: %v", err)
	}
	data := t.TempDir()
	pi := filepath.Join(data, "pi")
	proj := filepath.Join(data, "projects", "p")
	for _, d := range []string{pi, proj} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pi, "cli.js"), []byte("// pi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "secret"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	Configure(Config{Hide: []string{data}, Require: true})
	t.Cleanup(func() { Configure(Config{}) })
	script := `cat "$1/cli.js" >/dev/null 2>&1 || echo "MISSING:pi"
if (echo x > "$1/cli.js") 2>/dev/null; then echo "LEAK:writable"; fi
if (echo x > "$1/new") 2>/dev/null; then echo "LEAK:new-file"; fi
if cat "$2" >/dev/null 2>&1; then echo "LEAK:secret"; fi
echo done`
	argv, env, _, err := Command([]string{"/bin/sh", "-c", script, "sh", pi, filepath.Join(data, "secret")}, Spec{Dir: proj, ReadOnly: []string{pi, ""}, RequireJail: true})
	if err != nil {
		t.Fatal(err)
	}
	cmd := procgroup.Command(argv[0], argv[1:]...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.Contains(string(out), "LEAK") || strings.Contains(string(out), "MISSING") || !strings.Contains(string(out), "done") {
		t.Fatalf("jail output: %s", out)
	}
}
