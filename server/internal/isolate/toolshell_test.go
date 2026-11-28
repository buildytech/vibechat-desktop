package isolate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

func needLinuxJail(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	if err := JailAvailable(); err != nil {
		t.Skipf("no bubblewrap here: %v", err)
	}
}

func runJail(t *testing.T, s Spec, script string) string {
	t.Helper()
	argv, env, _, err := Command([]string{"/bin/sh", "-c", script}, s)
	if err != nil {
		t.Fatal(err)
	}
	cmd := procgroup.Command(argv[0], argv[1:]...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// NoNet: the jail has a network namespace of its own with only a loopback, so the host's loopback ports and the
// internet are not there; a Unix socket bound in is the one way out.
func TestNoNetJailHasOnlyItsOwnLoopbackAndAnInheritedSocket(t *testing.T) {
	needLinuxJail(t)
	dir := t.TempDir()
	got := runJail(t, Spec{Dir: dir, NoNet: true}, `tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d ' '`)
	if got != "lo" {
		t.Fatalf("interfaces in the jail: %q", got)
	}
	sockDir, err := os.MkdirTemp("", "vcsock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	if err := os.WriteFile(filepath.Join(sockDir, "marker"), []byte("bound"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = runJail(t, Spec{Dir: dir, NoNet: true, Binds: []Bind{{Src: sockDir, Dst: "/tmp/vc-gw"}}}, `cat /tmp/vc-gw/marker; echo; { echo x > /tmp/vc-gw/new; } 2>/dev/null && echo writable || echo readonly`)
	if got != "bound\nreadonly" {
		t.Fatalf("bind: %q", got)
	}
}

// A Bind may sit inside a writable folder and keep one file read-only (the agent's settings inside its own folder).
func TestBindMakesOneFileReadOnlyInsideAWritableFolder(t *testing.T) {
	needLinuxJail(t)
	dir, w := t.TempDir(), t.TempDir()
	f := filepath.Join(w, "settings.json")
	if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runJail(t, Spec{Dir: dir, Writable: []string{w}, Binds: []Bind{{Src: f, Dst: f}}},
		`echo a > `+w+`/other && echo other-ok; { echo b >> `+f+`; } 2>/dev/null && echo settings-written || echo settings-protected`)
	if got != "other-ok\nsettings-protected" {
		t.Fatalf("%q", got)
	}
}

func TestToolShellScriptQuotesAndValidates(t *testing.T) {
	s, err := ToolShellScript("/usr/bin/bwrap", "/usr/bin/bash", []string{"/tmp/vc-gw", "/data/it's here"}, "VIBECHAT_GW_SOCK")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#!/bin/sh", "unset VIBECHAT_GW_SOCK", "--unshare-net", "--unshare-pid", "--proc /proc", "--tmpfs '/tmp/vc-gw'", `--tmpfs '/data/it'\''s here'`, `-- '/usr/bin/bash' "$@"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	for _, bad := range []struct {
		bwrap, bash string
		mask        []string
		env         []string
	}{
		{"bwrap", "/bin/bash", nil, nil}, {"/usr/bin/bwrap", "bash", nil, nil}, {"/usr/bin/bwrap", "/bin/bash", []string{"rel"}, nil},
		{"/usr/bin/bwrap", "/bin/bash", []string{"/a\nb"}, nil}, {"/usr/bin/bwrap", "/bin/bash", nil, []string{"A;B"}},
	} {
		if _, err := ToolShellScript(bad.bwrap, bad.bash, bad.mask, bad.env...); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// The wrapper does what it says: its command has no network, sees only its own processes, does not see the masked
// folder or the dropped variable, and the exit status of the command comes back.
func TestToolShellRunsTheCommandInANestedJail(t *testing.T) {
	needLinuxJail(t)
	if err := NestedJailAvailable(); err != nil {
		t.Skipf("no nested jail here: %v", err)
	}
	bash, err := ResolveBash()
	if err != nil {
		t.Skip(err)
	}
	dir, secret := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(secret, "key"), []byte("TOP"), 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := ToolShellScript(BwrapPath(), bash, []string{secret}, "VC_TEST_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	sh := filepath.Join(t.TempDir(), "tool-shell")
	if err := os.WriteFile(sh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VC_TEST_SECRET", "SHOULD-NOT-SEE")
	cmd := exec.Command(sh, "-c", `echo "if=$(tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d ' ' | tr '\n' ,)"; echo "key=$(cat `+secret+`/key 2>&1 | head -c 40)"; echo "env=${VC_TEST_SECRET:-gone}"; echo "pids=$(ls /proc | grep -c '^[0-9]')"; pwd; exit 7`)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	ee, _ := err.(*exec.ExitError)
	if ee == nil || ee.ExitCode() != 7 {
		t.Fatalf("exit status lost: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "if=lo,") || strings.Contains(got, "TOP") || !strings.Contains(got, "env=gone") {
		t.Fatalf("%s", got)
	}
	if !strings.Contains(got, dir) {
		t.Fatalf("the working directory is not kept: %s", got)
	}
}
