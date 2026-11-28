package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/gitx"
	"github.com/buildytech/vibechat-desktop/server/internal/providergw/stub"
)

func lastShellOutput(t *testing.T, c *collector) string {
	t.Helper()
	var out string
	for _, w := range c.wires() {
		if w["type"] == "tool.finished" {
			out, _ = w["detail"].(string)
		}
	}
	if out == "" {
		t.Fatalf("no shell output: %v", c.wires())
	}
	return out
}

func bashOnce(script string) stub.Script {
	return func(n int, req stub.Request) stub.Reply {
		if n == 0 {
			return stub.Reply{Calls: []stub.Call{{Name: "bash", Args: map[string]any{"command": script}}}}
		}
		return stub.Reply{Text: "done"}
	}
}

// Git as VibeChat's flows use it, with the real Pi in the real jail: a session works in a linked worktree whose
// ".git" file points into the project's repository under the hidden data folder. The agent's own git status, add and
// commit work there; the repository's config and hooks stay read-only (the host runs git there later); and what the
// agent committed is on the session's branch for the host to merge, as after a Cursor run.
func TestE2EGitInAWorktreeInsideTheJail(t *testing.T) {
	in := realInstall(t)
	if !gitx.Available() {
		t.Skip("no git")
	}
	var proj, wt string
	h := newHarness(t, in, func(n int, req stub.Request) stub.Reply {
		return bashOnce(`
set -u
echo "STATUS0=$(git status --short | wc -l) BRANCH=$(git branch --show-current)"
echo hello > a.txt
git add -A && git commit -q -m "agent commit" && echo COMMIT=ok || echo COMMIT=failed
echo "STATUS1=$(git status --short | wc -l) LOG=$(git log --oneline | wc -l)"
git diff --stat HEAD~1 | tail -1
{ echo '[core]' >> `+proj+`/.git/config; } 2>/dev/null && echo CONFIG=writable || echo CONFIG=readonly
{ echo x > `+proj+`/.git/hooks/pre-commit; } 2>/dev/null && echo HOOKS=writable || echo HOOKS=readonly
{ echo x > `+proj+`/.git/info/exclude; } 2>/dev/null && echo INFO=writable || echo INFO=readonly
echo END`)(n, req)
	})
	proj = filepath.Join(h.data, "projects", "p1")
	wt = filepath.Join(h.data, "worktrees", "p1", "s1")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	writeFile(t, filepath.Join(proj, "README.md"), "# p1\n")
	if err := gitx.InitCommit(ctx, proj, "init"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(ctx, proj, "worktree", "add", "-q", "-b", "vibe/s1", wt, "main"); err != nil {
		t.Fatal(err)
	}
	c, _, err, _ := h.run(t, ctx, wt, "", "commit a file")
	if err != nil {
		t.Fatalf("%v\n%s", err, h.logs.all())
	}
	out := lastShellOutput(t, c)
	for _, want := range []string{"STATUS0=0 BRANCH=vibe/s1", "COMMIT=ok", "STATUS1=0 LOG=2", "1 file changed", "CONFIG=readonly", "HOOKS=readonly", "INFO=readonly", "END"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	// the host sees the agent's commit on the session branch and merges it into main like after any run
	if log, err := gitx.Run(ctx, wt, "log", "--format=%s", "-n", "1"); err != nil || log != "agent commit" {
		t.Fatalf("host log: %q %v", log, err)
	}
	if _, err := gitx.Run(ctx, proj, "merge", "-q", "--no-edit", "vibe/s1"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(proj, "a.txt")); string(got) != "hello\n" {
		t.Fatalf("merged file: %q", got)
	}
	if st, _ := gitx.Run(ctx, proj, "status", "--porcelain"); st != "" {
		t.Fatalf("project not clean after the merge: %q", st)
	}
	if fsck, err := gitx.Run(ctx, proj, "fsck", "--no-progress"); err != nil {
		t.Fatalf("fsck: %v %s", err, fsck)
	}
}

// A project folder without worktrees (the agent works in the project itself) and a plain folder: `git init`, add and
// commit work inside the jail with the VibeChat identity.
func TestE2EGitInitAndCommitInAPlainFolder(t *testing.T) {
	in := realInstall(t)
	if !gitx.Available() {
		t.Skip("no git")
	}
	h := newHarness(t, in, bashOnce(`
git init -q -b main && echo INIT=ok
echo one > f.txt
git add -A && git commit -q -m first && echo COMMIT=ok
echo "AUTHOR=$(git log -1 --format='%an <%ae>')"
echo two >> f.txt
echo "STATUS=$(git status --short)"
git add f.txt && git commit -q -m second && echo COMMIT2=ok
echo "COUNT=$(git rev-list --count HEAD)"
echo END`))
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	c, _, err, _ := h.run(t, ctx, ws, "", "init a repo")
	if err != nil {
		t.Fatalf("%v\n%s", err, h.logs.all())
	}
	out := lastShellOutput(t, c)
	for _, want := range []string{"INIT=ok", "COMMIT=ok", "AUTHOR=VibeChat <vibechat@localhost>", "STATUS= M f.txt", "COMMIT2=ok", "COUNT=2", "END"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	if n, err := gitx.Run(ctx, ws, "rev-list", "--count", "HEAD"); err != nil || n != "2" {
		t.Fatalf("host: %q %v", n, err)
	}
}

// The project folder itself as the workspace: its repository is inside the writable folder, but the config, hooks and
// info (what git on the host trusts) are read-only in the jail; commits still work.
func TestE2EProjectFolderRepositoryKeepsItsConfigReadOnly(t *testing.T) {
	in := realInstall(t)
	if !gitx.Available() {
		t.Skip("no git")
	}
	h := newHarness(t, in, bashOnce(`
echo hi > n.txt && git add -A && git commit -q -m agent && echo COMMIT=ok
{ printf '[core]\n\tfsmonitor = touch /tmp/x\n' >> .git/config; } 2>/dev/null && echo CONFIG=writable || echo CONFIG=readonly
{ echo x > .git/hooks/post-commit; } 2>/dev/null && echo HOOKS=writable || echo HOOKS=readonly
echo END`))
	ws := filepath.Join(h.data, "projects", "p2")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	writeFile(t, filepath.Join(ws, "README.md"), "# p2\n")
	if err := gitx.InitCommit(ctx, ws, "init"); err != nil {
		t.Fatal(err)
	}
	c, _, err, _ := h.run(t, ctx, ws, "", "commit")
	if err != nil {
		t.Fatalf("%v\n%s", err, h.logs.all())
	}
	out := lastShellOutput(t, c)
	for _, want := range []string{"COMMIT=ok", "CONFIG=readonly", "HOOKS=readonly", "END"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	if n, err := gitx.Run(ctx, ws, "rev-list", "--count", "HEAD"); err != nil || n != "2" {
		t.Fatalf("host: %q %v", n, err)
	}
}

// The attack on the host through the worktree, run by the real agent's shell: it replaces the worktree's ".git" with a
// repository of its own whose config names a program (core.fsmonitor). The program must not run when the host
// next uses git there, and after the host's guard the worktree is the project's again.
func TestE2EAgentCannotMakeHostGitRunItsProgram(t *testing.T) {
	in := realInstall(t)
	if !gitx.Available() {
		t.Skip("no git")
	}
	proof := filepath.Join(t.TempDir(), "PWNED")
	h := newHarness(t, in, bashOnce(`
rm -f .git && mkdir -p .git/objects .git/refs && echo 'ref: refs/heads/main' > .git/HEAD
printf '[core]\n\trepositoryformatversion = 0\n\tfsmonitor = touch `+proof+`; echo\n' > .git/config
echo "PLANTED=$(wc -l < .git/config)"
echo END`))
	proj := filepath.Join(h.data, "projects", "p3")
	wt := filepath.Join(h.data, "worktrees", "p3", "s1")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	writeFile(t, filepath.Join(proj, "README.md"), "# p3\n")
	if err := gitx.InitCommit(ctx, proj, "init"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(wt), 0o755)
	if _, err := gitx.Run(ctx, proj, "worktree", "add", "-q", "-b", "vibe/s1", wt, "main"); err != nil {
		t.Fatal(err)
	}
	c, _, err, _ := h.run(t, ctx, wt, "", "plant it")
	if err != nil {
		t.Fatalf("%v\n%s", err, h.logs.all())
	}
	if out := lastShellOutput(t, c); !strings.Contains(out, "PLANTED=") {
		t.Fatalf("the attack did not run (the test proves nothing): %s", out)
	}
	if _, err := gitx.Run(ctx, wt, "status", "--porcelain"); err == nil {
		t.Fatal("host git ran in the rewritten worktree")
	}
	if err := gitx.GuardWorktree(proj, wt); err != nil {
		t.Fatal(err)
	}
	if st, err := gitx.Run(ctx, wt, "status", "--porcelain"); err != nil || st != "" {
		t.Fatalf("after the guard: %q %v", st, err)
	}
	if _, err := os.Stat(proof); err == nil {
		t.Fatal("the agent's program ran on the host")
	}
}
