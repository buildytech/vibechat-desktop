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

	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

// Run runs git -C dir args… and answers trimmed stdout.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if cfg := configOf(dir); cfg != "" {
		if err := checkConfig(cfg); err != nil {
			return "", fmt.Errorf("git %s: the repository config has a setting that can run a program (%v); not running git there", strings.Join(args, " "), err)
		}
	}
	// What can run a program is switched off on the command line too, whatever a config says.
	full := append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main", "-c", "core.autocrlf=false",
		"-c", "core.fsmonitor=false", "-c", "core.pager=cat", "-c", "core.editor=true", "-c", "core.sshCommand=false", "-c", "core.askPass=",
		"-c", "diff.external=", "-c", "credential.helper=", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "protocol.ext.allow=never"}, args...)
	cmd := procgroup.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_PAGER=cat", "GIT_EDITOR=true", "GIT_ASKPASS=", "GIT_SSH_COMMAND=false",
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

// InitCommit makes dir a repository with everything committed on main. A folder with no files (the empty
// workspace) still gets an empty commit, so worktrees have a tip to branch from.
func InitCommit(ctx context.Context, dir, msg string) error {
	if _, err := Run(ctx, dir, "init", "-q", "-b", "main"); err != nil {
		return err
	}
	if _, err := Run(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	if st, _ := Run(ctx, dir, "status", "--porcelain"); st == "" {
		_, err := Run(ctx, dir, "commit", "-q", "--allow-empty", "-m", msg)
		return err
	}
	_, err := Run(ctx, dir, "commit", "-q", "-m", msg)
	return err
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
