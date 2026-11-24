// Package gitx runs git for the host: project repositories, per-session worktrees and the merge back. Every call has
// a fixed identity and no hooks or user config (core.hooksPath off), so a workspace cannot run code through git.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/buildytech/vibechat-desktop/internal/procgroup"
)

// Run runs git -C dir args… and answers trimmed stdout.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	full := append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main", "-c", "core.autocrlf=false"}, args...)
	cmd := procgroup.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=VibeChat", "GIT_AUTHOR_EMAIL=vibechat@localhost",
		"GIT_COMMITTER_NAME=VibeChat", "GIT_COMMITTER_EMAIL=vibechat@localhost",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Available reports whether git is on PATH.
func Available() bool { _, err := exec.LookPath("git"); return err == nil }

// InitCommit makes dir a repository with everything committed on main.
func InitCommit(ctx context.Context, dir, msg string) error {
	if _, err := Run(ctx, dir, "init", "-q", "-b", "main"); err != nil {
		return err
	}
	return CommitAll(ctx, dir, msg)
}

// CommitAll stages everything and commits when something changed; it answers whether a commit was made.
func CommitAll(ctx context.Context, dir, msg string) error {
	if _, err := Run(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	if st, _ := Run(ctx, dir, "status", "--porcelain"); st == "" {
		return nil
	}
	_, err := Run(ctx, dir, "commit", "-q", "-m", msg)
	return err
}

// Changed lists paths that differ between two commits.
func Changed(ctx context.Context, dir, from, to string) []string {
	out, err := Run(ctx, dir, "diff", "--name-only", from, to)
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}
