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

	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
	"github.com/buildytech/vibechat-desktop/server/internal/templates"
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
	if tpl.Tool == "go" {
		if err := checkGo(ctx, dir, tpl, log); err != nil {
			return err
		}
	}
	spec := toolSpec(tpl)
	err := runProjectTool(ctx, dir, tpl.Install, spec, log, nil)
	if err != nil && strings.Contains(err.Error(), "Fail extracting tarball") && ctx.Err() == nil {
		// Bun on Windows can fail to extract into its cache while another process (antivirus, indexer) holds a
		// freshly written file; the cache is consistent again on a second run.
		if log != nil {
			log("bun install: tarball extraction failed, retrying once")
		}
		err = runProjectTool(ctx, dir, tpl.Install, spec, log, nil)
	}
	return err
}

// toolSpec is the jail of one of the template's tool runs: the runner's own folders and settings (the Go caches),
// and for Bun its install cache.
func toolSpec(tpl templates.Profile) isolate.Spec {
	s := isolate.Spec{Writable: append([]string{}, tpl.Writable...), Env: append([]string{}, tpl.Env...)}
	if tpl.Tool == "bun" || (tpl.Tool == "" && len(tpl.Install) > 0 && tpl.Install[0] == "bun") {
		b := installSpec()
		s.Writable, s.Env = append(s.Writable, b.Writable...), append(s.Env, b.Env...)
	}
	return s
}

// installSpec: an install writes the project and the Bun cache (BUN_INSTALL_CACHE_DIR points Bun at it, since the
// jail's HOME is a private /tmp).
func installSpec() isolate.Spec {
	cache := isolate.BunCache()
	if cache == "" {
		return isolate.Spec{}
	}
	return isolate.Spec{Writable: []string{cache}, Env: []string{"BUN_INSTALL_CACHE_DIR=" + cache}}
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
	if _, err := exec.LookPath("bun"); err != nil {
		return fmt.Errorf("Bun is not installed or not on PATH: install it from https://bun.sh and restart the app")
	}
	var out bytes.Buffer
	if err := runProjectTool(ctx, dir, []string{"bun", "--version"}, isolate.Spec{}, log, &out); err != nil {
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

// checkGo answers a clear error when Go is missing or older than the project's go.mod `go` line (GOTOOLCHAIN=local:
// the host never downloads another toolchain).
func checkGo(ctx context.Context, dir string, tpl templates.Profile, log func(string)) error {
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("Go is not installed on this server: run `sudo vibechat-server ensure-go` (or the setup wizard) and try again")
	}
	var out bytes.Buffer
	if err := runProjectTool(ctx, dir, []string{"go", "env", "GOVERSION"}, isolate.Spec{Env: tpl.Env}, log, &out); err != nil {
		return err
	}
	have := strings.TrimPrefix(strings.TrimSpace(out.String()), "go")
	if need := goModVersion(dir); need != "" && versionLess(have, need) {
		return fmt.Errorf("Go %s on this server is older than go %s in go.mod: update the server (`sudo vibechat-server ensure-go`)", have, need)
	}
	return nil
}

// goModVersion reads the `go` line of dir/go.mod ("" when absent).
func goModVersion(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "go" {
			return f[1]
		}
	}
	return ""
}
