package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/gitx"
	"github.com/buildytech/vibechat-desktop/server/internal/store"
)

// Sessions work in their own git worktree (branch vibe/<session>) under WorktreesDir, outside the project folder, so
// two chats never see each other's half-done files. After every run the host commits the worktree and merges it into
// the project folder (main), where the preview runs; before every run the worktree takes in main. Without
// WorktreesDir the agent works in the project folder itself.

func branchFor(sid string) string { return "vibe/" + sid[:min(12, len(sid))] }

// createWorkspace prepares the session's working folder.
func (h *Host) createWorkspace(ctx context.Context, p store.Project, sid string) (dir, branch string, err error) {
	if h.opts.WorktreesDir == "" {
		return p.Path, "", nil
	}
	dir = filepath.Join(h.opts.WorktreesDir, p.ID, sid)
	branch = branchFor(sid)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", "", err
	}
	if err := gitx.CommitAll(ctx, p.Path, "Studio edits"); err != nil {
		return "", "", err
	}
	if _, err := gitx.Run(ctx, p.Path, "worktree", "add", "-q", "-b", branch, dir, "main"); err != nil {
		return "", "", err
	}
	if tpl, ok := h.template(p.TemplateID); ok && len(tpl.Install) > 0 {
		ictx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if err := InstallDeps(ictx, dir, tpl, h.opts.Log); err != nil {
			h.opts.Log("worktree install: " + err.Error())
		}
	}
	return dir, branch, nil
}

func (h *Host) sessionDir(p store.Project, s store.Session) string {
	if s.Worktree != "" {
		return s.Worktree
	}
	return p.Path
}

// removeWorktree drops the session's worktree and branch (the merged commits stay on main).
func (h *Host) removeWorktree(p store.Project, s store.Session) {
	if s.Worktree == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := gitx.Run(ctx, p.Path, "worktree", "remove", "--force", s.Worktree); err != nil {
		_ = os.RemoveAll(s.Worktree)
		_, _ = gitx.Run(ctx, p.Path, "worktree", "prune")
	}
	if s.Branch != "" {
		_, _ = gitx.Run(ctx, p.Path, "branch", "-D", s.Branch)
	}
}

// syncBeforeRun brings main's latest state (other sessions, editor saves) into the session's worktree.
func (h *Host) syncBeforeRun(ctx context.Context, p store.Project, s store.Session) error {
	if s.Worktree == "" {
		return nil
	}
	if err := gitx.GuardWorktree(p.Path, s.Worktree); err != nil {
		return err
	}
	if err := gitx.CommitAll(ctx, p.Path, "Studio edits"); err != nil {
		return err
	}
	if err := gitx.CommitAll(ctx, s.Worktree, "Agent work in progress"); err != nil {
		return err
	}
	if _, err := gitx.Run(ctx, s.Worktree, "merge", "-q", "--no-edit", "main"); err != nil {
		_, _ = gitx.Run(ctx, s.Worktree, "merge", "--abort")
		return fmt.Errorf("could not bring the latest project files into this chat's worktree: %v", err)
	}
	return nil
}

// landRun commits the run in the worktree and merges it into main; it answers the files that changed on main.
func (h *Host) landRun(ctx context.Context, p store.Project, s store.Session, title string) ([]string, error) {
	dir := h.sessionDir(p, s)
	before, _ := gitx.Run(ctx, p.Path, "rev-parse", "HEAD")
	msg := "Agent: " + oneLineTitle(title)
	if s.Worktree == "" {
		if err := gitx.CommitAll(ctx, dir, msg); err != nil {
			return nil, err
		}
	} else {
		// the agent wrote in this folder: git there must read the project's own repository, nothing the agent left
		if err := gitx.GuardWorktree(p.Path, s.Worktree); err != nil {
			return nil, err
		}
		if err := gitx.CommitAll(ctx, s.Worktree, msg); err != nil {
			return nil, err
		}
		if err := gitx.CommitAll(ctx, p.Path, "Studio edits"); err != nil {
			return nil, err
		}
		if _, err := gitx.Run(ctx, p.Path, "merge", "-q", "--no-edit", s.Branch); err != nil {
			_, _ = gitx.Run(ctx, p.Path, "merge", "--abort")
			return nil, fmt.Errorf("the changes conflict with the project and stay in branch %s: %v", s.Branch, err)
		}
	}
	after, _ := gitx.Run(ctx, p.Path, "rev-parse", "HEAD")
	if before == "" || before == after {
		return nil, nil
	}
	changed := gitx.Changed(ctx, p.Path, before, after)
	for _, f := range changed {
		if f == "package.json" || strings.HasSuffix(f, "bun.lock") {
			if tpl, ok := h.template(p.TemplateID); ok && len(tpl.Install) > 0 {
				if err := InstallDeps(ctx, p.Path, tpl, h.opts.Log); err != nil {
					h.opts.Log("reinstall after agent change: " + err.Error())
				}
			}
			break
		}
	}
	return changed, nil
}

func oneLineTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 72 {
		s = string(r[:72]) + "…"
	}
	return s
}
