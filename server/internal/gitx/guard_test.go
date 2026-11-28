package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repoWithWorktree(t *testing.T) (project, wt string) {
	t.Helper()
	if !Available() {
		t.Skip("no git")
	}
	root := t.TempDir()
	project, wt = filepath.Join(root, "project"), filepath.Join(root, "wt")
	ctx := context.Background()
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InitCommit(ctx, project, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, project, "worktree", "add", "-q", "-b", "vibe/s", wt, "main"); err != nil {
		t.Fatal(err)
	}
	return project, wt
}

// What the agent can write in its worktree must not make git on the host run a program: this is the attack, in its
// simplest form (a ".git" folder of its own whose config names core.fsmonitor), before and after the guards.
func TestAWorktreeTheAgentRewroteCannotRunAProgramOnTheHost(t *testing.T) {
	project, wt := repoWithWorktree(t)
	proof := filepath.Join(t.TempDir(), "PWNED")
	evil := "[core]\n\trepositoryformatversion = 0\n\tfsmonitor = touch " + proof + "; echo\n"
	// 1. a .git folder of its own with an evil config
	if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"objects", "refs"} {
		os.MkdirAll(filepath.Join(wt, ".git", d), 0o755)
	}
	os.WriteFile(filepath.Join(wt, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	os.WriteFile(filepath.Join(wt, ".git", "config"), []byte(evil), 0o644)
	if _, err := Run(context.Background(), wt, "status", "--porcelain"); err == nil || !strings.Contains(err.Error(), "can run a program") {
		t.Fatalf("git ran in a repository with an evil config: %v", err)
	}
	if _, err := os.Stat(proof); err == nil {
		t.Fatal("the program ran")
	}
	// the guard puts the plumbing back: git works again, on the project's own repository
	if err := GuardWorktree(project, wt); err != nil {
		t.Fatal(err)
	}
	if out, err := Run(context.Background(), wt, "status", "--porcelain"); err != nil || out != "" {
		t.Fatalf("after the guard: %q %v", out, err)
	}
	if b, _ := Run(context.Background(), wt, "branch", "--show-current"); b != "vibe/s" {
		t.Fatalf("branch %q", b)
	}
	// 2. commondir pointed at a repository of its own
	fake := filepath.Join(wt, "fake")
	os.MkdirAll(filepath.Join(fake, "objects"), 0o755)
	os.MkdirAll(filepath.Join(fake, "refs"), 0o755)
	os.WriteFile(filepath.Join(fake, "config"), []byte(evil), 0o644)
	gd := strings.TrimPrefix(readGitdir(t, wt), "gitdir: ")
	os.WriteFile(filepath.Join(gd, "commondir"), []byte(fake+"\n"), 0o644)
	os.WriteFile(filepath.Join(gd, "config.worktree"), []byte(evil), 0o644)
	if _, err := Run(context.Background(), wt, "status", "--porcelain"); err == nil {
		t.Fatal("git ran with a commondir of the agent's choice")
	}
	if err := GuardWorktree(project, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), wt, "status", "--porcelain"); err != nil {
		t.Fatalf("after the second guard: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gd, "config.worktree")); err == nil {
		t.Fatal("config.worktree left")
	}
	// 3. the .git file pointing somewhere else
	os.Remove(filepath.Join(wt, ".git"))
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+fake+"\n"), 0o644)
	if err := GuardWorktree(project, wt); err != nil {
		t.Fatal(err)
	}
	if got := readGitdir(t, wt); got != "gitdir: "+gd {
		t.Fatalf(".git file: %q", got)
	}
	if _, err := os.Stat(proof); err == nil {
		t.Fatal("the program ran at some point")
	}
}

func readGitdir(t *testing.T, wt string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestGuardWorktreeRefusesAFolderThatIsNotAWorktreeOfTheProject(t *testing.T) {
	project, _ := repoWithWorktree(t)
	if err := GuardWorktree(project, t.TempDir()); err == nil {
		t.Fatal("guarded a stranger")
	}
}

func TestConfigCheck(t *testing.T) {
	ok := "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n[branch \"main\"]\n\tremote = origin\n[remote \"origin\"]\n\turl = https://example.com/x.git\n"
	bad := map[string]string{
		"fsmonitor":       "[core]\n\tfsmonitor = x\n",
		"fsmonitor caps":  "[Core]\n\tFsMonitor=x\n",
		"one line":        "[core] fsmonitor = x\n",
		"hooks":           "[core]\n\thooksPath = /tmp/h\n",
		"filter":          "[filter \"x\"]\n\tclean = rm -rf\n",
		"include":         "[include]\n\tpath = /tmp/x\n",
		"includeIf":       "[includeIf \"gitdir:/\"]\n\tpath = /tmp/x\n",
		"alias":           "[alias]\n\tst = !x\n",
		"merge driver":    "[merge \"d\"]\n\tdriver = x\n",
		"diff textconv":   "[diff \"d\"]\n\ttextconv = x\n",
		"ssh":             "[core]\n\tsshCommand = x\n",
		"worktree config": "[extensions]\n\tworktreeConfig = true\n",
		"credential":      "[credential]\n\thelper = x\n",
		"broken":          "[core\n",
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config")
	os.WriteFile(p, []byte(ok), 0o644)
	if err := checkConfig(p); err != nil {
		t.Fatalf("an ordinary config: %v", err)
	}
	if err := checkConfig(filepath.Join(dir, "missing")); err != nil {
		t.Fatalf("no config: %v", err)
	}
	for name, doc := range bad {
		os.WriteFile(p, []byte(doc), 0o644)
		if checkConfig(p) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
