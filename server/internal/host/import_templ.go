package host

import (
	"bufio"
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/buildytech/vibechat-desktop/internal/gopin"
	"github.com/buildytech/vibechat-desktop/server/internal/golock"
	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// The templ adapter: a Go project on fastygo/starter (Go + PocketBase + templ), as BuildY Chat's templ export
// writes it (manifest.json framework templ) or as the starter itself is laid out (go.mod + cmd/site/main.go).
// Core of the framework = the starter at the template's pinned commit (its modules, tooling, the runtime code);
// data = internal/page/page.json, ui/styles, the static files and the pack's metadata; the rest is page code
// (sections_gen.go, the registry blocks it uses). The module graph may only use modules the starter's go.sum
// already pins, so an import adds no dependency the template has not been checked with.

type templAdapter struct{}

func (templAdapter) ID() string     { return "templ" }
func (templAdapter) Runner() string { return "go-templ" }

func (templAdapter) Detect(in *importInput) bool {
	_, mod := in.by["go.mod"]
	_, cfg := in.by["ui8kit.config.json"]
	return mod && !cfg
}

func (templAdapter) DataPaths() []string {
	return []string{"internal/page/page.json", "ui/styles/", "internal/web/static/", "app.css", "brand.contract.json", "manifest.json", "GENERATOR.json"}
}

var (
	// templTools are the go.mod `tool` lines an import may carry.
	templTools = map[string]bool{"github.com/a-h/templ/cmd/templ": true}
	// templGenerate are the //go:generate commands an import may carry (Chat's page generator, templ itself).
	templGenerate = map[string]bool{"go run ./gen": true, "go tool templ generate": true}
	// templBannedExt are file kinds that bring native code or prebuilt objects into the build.
	templBannedExt = map[string]bool{
		".c": true, ".cc": true, ".cpp": true, ".cxx": true, ".h": true, ".hh": true, ".hpp": true, ".hxx": true,
		".m": true, ".mm": true, ".s": true, ".sx": true, ".f": true, ".for": true, ".f90": true, ".swig": true,
		".swigcxx": true, ".syso": true, ".so": true, ".a": true, ".o": true, ".dll": true, ".dylib": true, ".exe": true,
	}
	// templToolingRoots and templToolingFiles are the core files filled from the template when the ZIP has none.
	templToolingRoots = []string{".cursor", "scripts", "skills"}
	templToolingFiles = []string{"AGENTS.md", "Makefile", ".gitignore", "LICENSE", "PRIVACY.md"}
)

func (a templAdapter) Validate(ctx context.Context, in *importInput, tpl templates.Profile) error {
	for _, need := range []string{"go.mod", "go.sum", "cmd/site/main.go"} {
		if _, ok := in.by[need]; !ok {
			return importFail(http.StatusBadRequest, "A Go templ import needs "+need)
		}
	}
	if _, ok := in.by["package.json"]; ok && in.pack.present {
		return importFail(http.StatusBadRequest, "A Go templ pack must not include package.json")
	}
	if err := templPathsOK(in.files); err != nil {
		return err
	}
	allow, err := templAllowedModules(ctx, tpl)
	if err != nil {
		return err
	}
	// Every go.mod of the ZIP (the root and the local modules it replaces, ui/kit in the starter).
	mods := map[string]*modfile.File{}
	for _, f := range in.files {
		if path.Base(f.path) != "go.mod" {
			continue
		}
		mf, err := modfile.Parse(f.path, f.data, nil)
		if err != nil {
			return importFail(http.StatusBadRequest, f.path+" is not valid: "+err.Error())
		}
		if mf.Module == nil || mf.Module.Mod.Path == "" {
			return importFail(http.StatusBadRequest, f.path+" has no module line")
		}
		mods[path.Dir(f.path)] = mf
	}
	local := map[string]bool{}
	for _, mf := range mods {
		local[mf.Module.Mod.Path] = true
	}
	for dir, mf := range mods {
		if err := templModOK(dir, mf, mods, local, allow); err != nil {
			return err
		}
	}
	for _, f := range in.files {
		if path.Base(f.path) != "go.sum" {
			continue
		}
		if err := templSumOK(f, allow); err != nil {
			return err
		}
	}
	for _, f := range in.files {
		if strings.HasSuffix(f.path, ".go") {
			if err := templGoFileOK(f); err != nil {
				return err
			}
		}
	}
	return nil
}

// templPathsOK refuses what a Go import must not carry: native code and objects, a workspace file, vendored
// modules, build output, the PocketBase data folder and VibeChat's own folder.
func templPathsOK(files []importFile) error {
	for _, f := range files {
		parts := strings.Split(f.path, "/")
		switch parts[0] {
		case "bin", ".vibechat":
			return importFail(http.StatusBadRequest, "A Go templ import must not include "+parts[0]+"/")
		}
		for _, part := range parts {
			switch part {
			case "vendor":
				return importFail(http.StatusBadRequest, "A Go templ import must not include vendor/ (modules come from the module proxy)")
			case "pb_data":
				return importFail(http.StatusBadRequest, "A Go templ import must not include pb_data/ (PocketBase data)")
			case "go.work", "go.work.sum":
				return importFail(http.StatusBadRequest, "A Go templ import must not include "+part)
			}
		}
		ext := strings.ToLower(path.Ext(f.path))
		if templBannedExt[ext] {
			return importFail(http.StatusBadRequest, "A Go templ import must not include native code or objects: "+f.path)
		}
	}
	return nil
}

// templAllowedModules are the module paths of the template's go.sum files (the starter and its ui/kit module).
func templAllowedModules(ctx context.Context, tpl templates.Profile) (map[string]bool, error) {
	if tpl.Layer != nil {
		if _, err := templates.EnsureLayer(ctx, tpl.Layer); err != nil {
			return nil, importFail(http.StatusBadGateway, "Could not get the Go template to check the import: "+err.Error())
		}
	}
	allow := map[string]bool{}
	err := fs.WalkDir(tpl.Files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Base(name) != "go.sum" {
			return err
		}
		raw, err := fs.ReadFile(tpl.Files, name)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if f := strings.Fields(line); len(f) == 3 {
				allow[f[0]] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(allow) == 0 {
		return nil, importFail(http.StatusInternalServerError, "The Go template has no go.sum to check the import against")
	}
	return allow, nil
}

func templModOK(dir string, mf *modfile.File, mods map[string]*modfile.File, local, allow map[string]bool) error {
	name := path.Join(dir, "go.mod")
	if mf.Go == nil {
		return importFail(http.StatusBadRequest, name+" needs a go line")
	}
	if !goVersionAllowed(mf.Go.Version) {
		return importFail(http.StatusBadRequest, name+": go "+mf.Go.Version+" is not supported (this server builds with Go "+gopin.Version+", go 1.22 or newer)")
	}
	if mf.Toolchain != nil {
		v := strings.TrimPrefix(mf.Toolchain.Name, "go")
		if v == mf.Toolchain.Name || !goVersionAllowed(v) {
			return importFail(http.StatusBadRequest, name+": toolchain "+mf.Toolchain.Name+" is not supported (this server has go"+gopin.Version+")")
		}
	}
	for _, t := range mf.Tool {
		if !templTools[t.Path] {
			return importFail(http.StatusBadRequest, name+": tool "+t.Path+" is not allowed (allowed: github.com/a-h/templ/cmd/templ)")
		}
	}
	for _, r := range mf.Replace {
		if !modfile.IsDirectoryPath(r.New.Path) || r.New.Version != "" {
			return importFail(http.StatusBadRequest, name+": replace "+r.Old.Path+" => "+r.New.Path+" is not allowed (only a folder inside the project)")
		}
		target := path.Clean(path.Join(dir, filepath.ToSlash(r.New.Path)))
		if filepath.IsAbs(r.New.Path) || target == ".." || strings.HasPrefix(target, "../") || strings.HasPrefix(r.New.Path, "/") {
			return importFail(http.StatusBadRequest, name+": replace "+r.Old.Path+" points outside the project")
		}
		tm, ok := mods[target]
		if !ok {
			return importFail(http.StatusBadRequest, name+": replace "+r.Old.Path+" => "+r.New.Path+" has no go.mod in the ZIP")
		}
		if tm.Module.Mod.Path != r.Old.Path {
			return importFail(http.StatusBadRequest, name+": replace "+r.Old.Path+" points to module "+tm.Module.Mod.Path)
		}
	}
	for _, req := range mf.Require {
		if local[req.Mod.Path] {
			continue
		}
		if err := module.CheckPath(req.Mod.Path); err != nil || !allow[req.Mod.Path] {
			return importFail(http.StatusBadRequest, name+": module "+req.Mod.Path+" is not in the Go template's go.sum (allowed modules only)")
		}
	}
	return nil
}

func templSumOK(f importFile, allow map[string]bool) error {
	sc := bufio.NewScanner(bytes.NewReader(f.data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || !strings.HasPrefix(fields[2], "h1:") {
			return importFail(http.StatusBadRequest, f.path+":"+strconv.Itoa(n)+" is not a go.sum line")
		}
		if !allow[fields[0]] {
			return importFail(http.StatusBadRequest, f.path+": module "+fields[0]+" is not in the Go template's go.sum (allowed modules only)")
		}
	}
	return sc.Err()
}

// templGoFileOK: the file parses, does not use cgo, and its //go:generate lines are the known generators.
func templGoFileOK(f importFile) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, f.path, f.data, parser.ImportsOnly|parser.ParseComments)
	if err != nil {
		return importFail(http.StatusBadRequest, f.path+" is not valid Go: "+firstLine(err.Error()))
	}
	for _, im := range file.Imports {
		if p, _ := strconv.Unquote(im.Path.Value); p == "C" {
			return importFail(http.StatusBadRequest, f.path+" uses cgo (import \"C\"); Go projects build with CGO_ENABLED=0")
		}
	}
	for _, line := range strings.Split(string(f.data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//go:cgo_") {
			return importFail(http.StatusBadRequest, f.path+" has a cgo directive")
		}
		if cmd, ok := strings.CutPrefix(line, "//go:generate"); ok {
			if !templGenerate[strings.Join(strings.Fields(cmd), " ")] {
				return importFail(http.StatusBadRequest, f.path+": //go:generate"+cmd+" is not allowed (allowed: go run ./gen, go tool templ generate)")
			}
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// goVersionAllowed: go 1.22 up to the pinned toolchain (1.27.4 accepts "1.27", "1.27.0" … "1.27.4", "1.26.0").
func goVersionAllowed(v string) bool {
	p := versionParts(v)
	if p[0] != 1 || p[1] < 22 || strings.ContainsAny(v, "-+ ") || strings.Contains(v, "rc") || strings.Contains(v, "beta") {
		return false
	}
	return !versionLess(gopin.Version, v)
}

func (templAdapter) Fill(staging string, in *importInput, tpl templates.Profile) error {
	copyIfMissing := func(name string) error {
		if _, ok := in.by[name]; ok {
			return nil
		}
		raw, err := fs.ReadFile(tpl.Files, name)
		if err != nil {
			return nil // the template may not have it
		}
		return writeInside(staging, name, raw, 0o644)
	}
	for _, name := range templToolingFiles {
		if err := copyIfMissing(name); err != nil {
			return err
		}
	}
	for _, root := range templToolingRoots {
		err := fs.WalkDir(tpl.Files, root, func(name string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			return copyIfMissing(name)
		})
		if err != nil {
			return err
		}
	}
	if err := templates.EnsureIgnored(staging, templates.GoIgnore); err != nil {
		return err
	}
	exec := map[string]bool{}
	if tpl.Layer != nil {
		for _, e := range tpl.Layer.Executables {
			exec[e] = true
		}
	}
	return filepath.WalkDir(staging, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(staging, p)
		rel = filepath.ToSlash(rel)
		if exec[rel] || (strings.HasPrefix(rel, "scripts/") && strings.HasSuffix(rel, ".sh")) {
			return os.Chmod(p, 0o755)
		}
		return nil
	})
}

// Verify builds the site in the tools jail (one Go build at a time on the server).
func (templAdapter) Verify(ctx context.Context, dir string, tpl templates.Profile, log func(string)) (string, error) {
	release, err := golock.Lock(ctx, isolate.GoCache())
	if err != nil {
		return "", err
	}
	defer release()
	var out bytes.Buffer
	argv := tpl.Build
	if len(argv) == 0 {
		argv = []string{"go", "build", "-o", ".vibechat/bin/site", "./cmd/site"}
	}
	err = runProjectTool(ctx, dir, argv, toolSpec(tpl), log, &out)
	return strings.TrimSpace(out.String()), err
}
