package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/buildytech/vibechat-desktop/internal/templates"
)

// InstallDeps installs a project's dependencies with its template's Install argv: the code path of project
// creation, a clone and a session worktree (and of the Windows CI e2e). For Bun it first checks the version against
// package.json engines.bun, so an old Bun fails with «update Bun» instead of a lockfile or extract error.
func InstallDeps(ctx context.Context, dir string, tpl templates.Profile, log func(string)) error {
	if len(tpl.Install) == 0 {
		return nil
	}
	if tpl.Install[0] == "bun" {
		if err := checkBun(ctx, dir, log); err != nil {
			return err
		}
	}
	return runTool(ctx, dir, tpl.Install, nil, log)
}

// minBun reads `"engines": {"bun": ">=X.Y.Z"}` from dir/package.json ("" when not set).
func minBun(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Engines map[string]string `json:"engines"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(pkg.Engines["bun"]), ">="))
}

func checkBun(ctx context.Context, dir string, log func(string)) error {
	var out bytes.Buffer
	if err := runToolOutput(ctx, dir, []string{"bun", "--version"}, nil, log, &out); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("Bun is not installed or not on PATH: install it from https://bun.sh and restart the app")
		}
		return err
	}
	have := strings.TrimSpace(out.String())
	if need := minBun(dir); need != "" && versionLess(have, need) {
		return fmt.Errorf("Bun %s is too old for this template: update Bun to %s or newer (run `bun upgrade`) and try again", have, need)
	}
	return nil
}

// versionLess compares dotted numeric versions (pre-release suffixes ignored).
func versionLess(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func versionParts(v string) [3]int {
	var p [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	for i, s := range strings.SplitN(v, ".", 3) {
		p[i], _ = strconv.Atoi(s)
	}
	return p
}
