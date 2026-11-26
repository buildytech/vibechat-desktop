// Package templates scaffolds projects from the buildytech/templates vibechat-server ships (embedded from the pinned
// submodule) and knows each template's desktop profile: how the host installs dependencies (outside the agent
// sandbox, with network), runs the dev server for the preview on a loopback port, and builds.
package templates

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/buildytech/vibechat-desktop/server/internal/gitx"
)

// Profile is one desktop template.
type Profile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Stack       string `json:"stack"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	// Files is the template source (fs root = template root).
	Files fs.FS `json:"-"`
	// Install, Dev and Build are argv; "{port}" in Dev is the dev-server port, "{hmrPort}" the preview proxy port.
	Install []string          `json:"-"`
	Dev     []string          `json:"-"`
	DevEnv  map[string]string `json:"-"`
	Build   []string          `json:"-"`
}

// Expand replaces {port} and {hmrPort}.
func Expand(xs []string, port, hmrPort int) []string {
	out := make([]string, len(xs))
	r := strings.NewReplacer("{port}", strconv.Itoa(port), "{hmrPort}", strconv.Itoa(hmrPort))
	for i, x := range xs {
		out[i] = r.Replace(x)
	}
	return out
}

// skip are template paths never copied into a project.
var skip = map[string]bool{"node_modules": true, "dist": true, ".git": true, "coverage": true}

// gitignore lines every project gets.
const gitignore = "node_modules/\ndist/\ncoverage/\n.env\n*.log\n.vite/\n"

// Scaffold copies p into dst (which must not exist), adds a .gitignore and commits it as the project's first commit.
func Scaffold(ctx context.Context, p Profile, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s already exists", dst)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	err := fs.WalkDir(p.Files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if skip[path.Base(name)] && d.IsDir() {
			return fs.SkipDir
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := fs.ReadFile(p.Files, name)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		return os.WriteFile(target, raw, mode)
	})
	if err != nil {
		return err
	}
	gi := filepath.Join(dst, ".gitignore")
	existing, _ := os.ReadFile(gi)
	if !strings.Contains(string(existing), "node_modules") {
		if err := os.WriteFile(gi, append(existing, []byte(gitignore)...), 0o644); err != nil {
			return err
		}
	}
	return gitx.InitCommit(ctx, dst, "Project from template "+p.ID)
}
