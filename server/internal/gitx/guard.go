package gitx

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The agent writes in its worktree (and in the project folder when there are no worktrees), and git on the host
// trusts what it finds there: a ".git" folder or file, the repository config in it. A config can name programs git
// runs (core.fsmonitor, a filter's clean and smudge commands, a merge driver, a hook path, an editor, an include of
// another file): written by the agent's shell, it would run on the host, outside every jail, with the server's
// environment. Two guards:
//
//   - GuardWorktree puts a session worktree's git plumbing back the way `git worktree add` made it (the ".git" file
//     and the worktree's commondir), so git there always reads the project's own repository config;
//   - Run refuses to run in a repository whose config holds a setting that can run a program (checkConfig), and
//     switches the program-running settings off on its command line.

// GuardWorktree restores the plumbing of the linked worktree wt of the repository at project: wt/.git is a regular
// file naming project/.git/worktrees/<id> (not a folder, not a link, not another place), that folder's commondir is
// "../.." and it holds no worktree-specific config. It returns an error when wt is not a linked worktree of project.
func GuardWorktree(project, wt string) error {
	project, wt = filepath.Clean(project), filepath.Clean(wt)
	common := filepath.Join(project, ".git")
	admin := filepath.Join(common, "worktrees")
	// which admin folder belongs to wt: the one whose gitdir file names wt/.git (git wrote it; we read, never trust the
	// file in wt)
	entries, err := os.ReadDir(admin)
	if err != nil {
		return fmt.Errorf("guard worktree: %v", err)
	}
	var gd string
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(admin, e.Name(), "gitdir"))
		if err == nil && filepath.Clean(strings.TrimSpace(string(raw))) == filepath.Join(wt, ".git") {
			gd = filepath.Join(admin, e.Name())
			break
		}
	}
	if gd == "" {
		return errors.New("guard worktree: " + wt + " is not a linked worktree of " + project)
	}
	if st, err := os.Lstat(gd); err != nil || !st.IsDir() {
		return errors.New("guard worktree: the worktree's administrative folder is not a folder")
	}
	// wt/.git: a regular file with the expected content; anything else the agent put there is removed
	gitfile := filepath.Join(wt, ".git")
	want := "gitdir: " + gd + "\n"
	if st, err := os.Lstat(gitfile); err != nil || !st.Mode().IsRegular() {
		if err == nil {
			if rerr := os.RemoveAll(gitfile); rerr != nil {
				return fmt.Errorf("guard worktree: %v", rerr)
			}
		}
	}
	if got, err := os.ReadFile(gitfile); err != nil || string(got) != want {
		if err := writeFileReplace(gitfile, []byte(want)); err != nil {
			return fmt.Errorf("guard worktree: %v", err)
		}
	}
	// the common dir is the project's, and the worktree has no config of its own
	cd := filepath.Join(gd, "commondir")
	if got, err := os.ReadFile(cd); err != nil || strings.TrimSpace(string(got)) != "../.." {
		if err := writeFileReplace(cd, []byte("../..\n")); err != nil {
			return fmt.Errorf("guard worktree: %v", err)
		}
	}
	_ = os.RemoveAll(filepath.Join(gd, "config.worktree"))
	return nil
}

// writeFileReplace writes a file even where a link or a read-only file stands.
func writeFileReplace(path string, b []byte) error {
	_ = os.Remove(path)
	return os.WriteFile(path, b, 0o644)
}

// unsafeSection are config sections that carry programs or files to read, whatever their keys.
var unsafeSection = map[string]bool{"include": true, "includeif": true, "filter": true, "alias": true, "credential": true,
	"gpg": true, "sendemail": true, "uploadpack": true, "receive": true, "http": true, "difftool": true, "mergetool": true,
	"submodule": true, "trailer": true, "sequence": true, "web": true}

// unsafeKey are keys of other sections that name a program (matched on the section and the key, lower case).
var unsafeKey = map[string]bool{
	"core.fsmonitor": true, "core.sshcommand": true, "core.hookspath": true, "core.pager": true, "core.editor": true,
	"core.askpass": true, "core.gitproxy": true, "core.attributesfile": true, "core.excludesfile": true, "core.worktree": true,
	"core.alternaterefscommand": true, "core.untrackedcache": false,
	"diff.external": true, "diff.textconv": true, "diff.command": true,
	"merge.driver": true, "merge.tool": true, "merge.conflictstyle": false,
	"gc.auto": false, "pager.diff": true, "extensions.worktreeconfig": true,
}

// checkConfig reads a repository config and answers an error naming the first setting that can run a program or pull
// in another file. Sections it does not know and ordinary keys (core.filemode, branch, remote url) pass.
func checkConfig(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				return errors.New("unreadable section line")
			}
			head := line[1:end]
			name, _, _ := strings.Cut(head, " ")
			name, _, _ = strings.Cut(name, ".")
			section = strings.ToLower(name)
			if unsafeSection[section] {
				return fmt.Errorf("section [%s]", section)
			}
			if rest := strings.TrimSpace(line[end+1:]); rest != "" && rest[0] != '#' && rest[0] != ';' {
				line = rest // "[core] fsmonitor = x" on one line
			} else {
				continue
			}
		}
		key, _, _ := strings.Cut(line, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if unsafeKey[section+"."+key] {
			return fmt.Errorf("setting %s.%s", section, key)
		}
		if section == "merge" || section == "diff" {
			if key == "driver" || key == "textconv" || key == "command" || key == "external" {
				return fmt.Errorf("setting %s.%s", section, key)
			}
		}
	}
	return sc.Err()
}

// configOf is the config file git would read for dir: <dir>/.git/config, or for a linked worktree the common
// directory's config.
func configOf(dir string) string {
	g := filepath.Join(dir, ".git")
	st, err := os.Lstat(g)
	if err != nil {
		return ""
	}
	if st.IsDir() {
		return filepath.Join(g, "config")
	}
	raw, err := os.ReadFile(g)
	if err != nil || len(raw) > 4096 {
		return ""
	}
	line, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return ""
	}
	gd := filepath.Clean(strings.TrimSpace(line))
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(dir, gd)
	}
	cdRaw, err := os.ReadFile(filepath.Join(gd, "commondir"))
	if err != nil {
		return filepath.Join(gd, "config")
	}
	cd := strings.TrimSpace(string(cdRaw))
	if !filepath.IsAbs(cd) {
		cd = filepath.Join(gd, cd)
	}
	return filepath.Join(filepath.Clean(cd), "config")
}
