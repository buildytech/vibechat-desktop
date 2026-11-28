package pi

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
)

// gitMounts are the extra mounts that let the agent's own git commands work in a session worktree. A worktree's
// ".git" is a file naming <repo>/.git/worktrees/<id>, and that folder lives outside the worktree (in the project's
// repository, under the server's hidden data folder), so without these mounts git in the jail says "not a git
// repository". The repository is bound read-only (its config, hooks and info cannot be changed from the jail: the host
// runs git there later) and only what a commit must write comes back read-write: the worktree's own folder (HEAD,
// index), the object store, the refs and their logs. A folder with its own ".git" directory (a project folder, a
// fresh `git init`) needs nothing: it is inside the writable workspace.
func gitMounts(cwd string) (ro, rw []string, locked []isolate.Bind) {
	gitfile := filepath.Join(cwd, ".git")
	st, err := os.Lstat(gitfile)
	if err != nil {
		return nil, nil, nil
	}
	if st.IsDir() {
		// The project folder itself (no worktrees): its repository is inside the writable workspace, but what git on
		// the host later trusts stays read-only, mounted last so it sits over the writable folder.
		for _, p := range []string{"config", "hooks", "info"} {
			full := filepath.Join(gitfile, p)
			if _, err := os.Lstat(full); err == nil {
				locked = append(locked, isolate.Bind{Src: full, Dst: full})
			}
		}
		return nil, nil, locked
	}
	if !st.Mode().IsRegular() {
		return nil, nil, nil
	}
	raw, err := os.ReadFile(gitfile)
	if err != nil || len(raw) > 4096 {
		return nil, nil, nil
	}
	line, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return nil, nil, nil
	}
	gd := strings.TrimSpace(line)
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(cwd, gd)
	}
	gd = filepath.Clean(gd)
	wt := filepath.Dir(gd)
	if filepath.Base(wt) != "worktrees" {
		return nil, nil, nil // not a linked worktree: nothing to mount
	}
	common := filepath.Dir(wt)
	// the worktree's commondir must point where the path says, or this is not the layout git makes
	cd, err := os.ReadFile(filepath.Join(gd, "commondir"))
	if err != nil {
		return nil, nil, nil
	}
	if c := strings.TrimSpace(string(cd)); filepath.Clean(filepath.Join(gd, c)) != common {
		return nil, nil, nil
	}
	if fi, err := os.Stat(common); err != nil || !fi.IsDir() {
		return nil, nil, nil
	}
	ro = []string{common}
	for _, p := range []string{gd, filepath.Join(common, "objects"), filepath.Join(common, "refs"), filepath.Join(common, "logs")} {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			rw = append(rw, p)
		}
	}
	return ro, rw, nil
}
