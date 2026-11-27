package host

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/gitx"
	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
	"github.com/buildytech/vibechat-desktop/server/internal/sandbox"
	"github.com/buildytech/vibechat-desktop/server/internal/store"
	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// Import ZIP (POST /projects/:id/import.zip): a BuildY Chat pack or a UI8Kit svelte/react project, only into a
// workspace with no files except .git. Validation is strict (framework, required files, path safety, allowlisted
// packages, no lifecycle scripts). Tooling from the official starter is added without overwriting src/**.

const (
	importMaxBytes        = 32 << 20  // compressed
	importMaxUncompressed = 128 << 20 // total of file bodies
	importMaxFiles        = 5000
	importMaxFile         = 8 << 20
	importField           = "zip"
)

type importError struct {
	status int
	msg    string
}

func (e *importError) Error() string { return e.msg }

func importFail(status int, msg string) *importError { return &importError{status: status, msg: msg} }

var (
	importForbiddenScripts = map[string]bool{
		"preinstall": true, "install": true, "postinstall": true, "prepare": true,
		"prepublish": true, "prepublishOnly": true, "prepack": true, "postpack": true,
	}
	importCommonPkgs = map[string]bool{
		"clsx": true, "lucide": true, "tailwind-merge": true, "vite": true,
		"@tailwindcss/vite": true, "tailwindcss": true, "typescript": true,
		"@types/bun": true, "ui8kit": true, "ui8px": true,
	}
	importSveltePkgs = map[string]bool{
		"svelte": true, "@sveltejs/vite-plugin-svelte": true, "svelte-check": true, "@tsconfig/svelte": true,
	}
	importReactPkgs = map[string]bool{
		"react": true, "react-dom": true, "@vitejs/plugin-react": true, "@types/react": true, "@types/react-dom": true,
	}
	importBannedNames = map[string]bool{
		"node_modules": true, ".git": true,
	}
	importOverlayRoots = []string{".cursor", ".ui8px", "scripts"}
	importOverlayFiles = []string{"AGENTS.md", "tsconfig.json", "svelte.config.js", ".bun-version"}
)

func (h *Host) importZip(w http.ResponseWriter, r *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return
	}
	if err := projectFileCount(p.Path); err != nil {
		if ie, ok := err.(*importError); ok {
			WriteError(w, ie.status, ie.msg)
			return
		}
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := readImportBody(w, r)
	if err != nil {
		if ie, ok := err.(*importError); ok {
			WriteError(w, ie.status, ie.msg)
			return
		}
		WriteError(w, http.StatusBadRequest, "Could not read the ZIP")
		return
	}
	files, err := unzipImport(raw)
	if err != nil {
		if ie, ok := err.(*importError); ok {
			WriteError(w, ie.status, ie.msg)
			return
		}
		WriteError(w, http.StatusBadRequest, "The file is not a valid ZIP")
		return
	}
	framework, err := validateImport(files)
	if err != nil {
		if ie, ok := err.(*importError); ok {
			WriteError(w, ie.status, ie.msg)
			return
		}
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Recheck emptiness right before the write (another tab may have added a file).
	if err := projectFileCount(p.Path); err != nil {
		if ie, ok := err.(*importError); ok {
			WriteError(w, ie.status, ie.msg)
			return
		}
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tpl, ok := h.template(framework)
	if !ok || !tpl.Enabled {
		WriteError(w, http.StatusInternalServerError, "This build has no "+framework+" template to finish the import")
		return
	}
	if err := materializeImport(files, p.Path, framework, tpl); err != nil {
		_ = wipeImportedFiles(p.Path)
		WriteError(w, http.StatusInternalServerError, "Could not write the imported files: "+err.Error())
		return
	}
	if err := gitx.CommitAll(r.Context(), p.Path, "Import ZIP ("+framework+")"); err != nil {
		_ = wipeImportedFiles(p.Path)
		WriteError(w, http.StatusInternalServerError, "Could not commit the import: "+err.Error())
		return
	}
	p.TemplateID = framework
	p.UpdatedAt = store.Now()
	if err := h.opts.Store.PutProject(p); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.resetSetup(p.ID)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	if err := InstallDeps(ctx, p.Path, tpl, h.opts.Log); err != nil {
		WriteError(w, http.StatusInternalServerError, "Imported, but installing dependencies failed: "+err.Error())
		return
	}
	verifyLog, verifyErr := verifyImported(ctx, p.Path, h.opts.Log)
	out := map[string]any{
		"ok":        true,
		"framework": framework,
		"files":     len(files),
		"project":   h.projectJSON(p),
		"verify":    map[string]any{"ok": verifyErr == nil, "log": verifyLog},
	}
	if verifyErr != nil {
		out["verify"].(map[string]any)["error"] = verifyErr.Error()
	}
	WriteJSON(w, http.StatusOK, out)
}

func (h *Host) resetSetup(id string) {
	h.mu.Lock()
	delete(h.setup, id)
	h.mu.Unlock()
}

// projectFileCount reports whether the project has any regular file outside .git (409 when it does).
func projectFileCount(root string) error {
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		base := d.Name()
		if d.IsDir() && (base == ".git" || rel == ".git") {
			return fs.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			return importFail(http.StatusConflict, "The project is not empty")
		}
		if d.Type().IsRegular() {
			n++
		}
		return nil
	})
	if err != nil {
		return err
	}
	if n > 0 {
		return importFail(http.StatusConflict, "Import ZIP is only available in an empty project")
	}
	return nil
}

func readImportBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, importMaxBytes+1)
	ct := r.Header.Get("content-type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(importMaxBytes); err != nil {
			if strings.Contains(err.Error(), "too large") || strings.Contains(strings.ToLower(err.Error()), "maxbytes") {
				return nil, importFail(http.StatusRequestEntityTooLarge, "The ZIP is larger than 32 MiB")
			}
			return nil, importFail(http.StatusBadRequest, "Could not read the upload")
		}
		f, _, err := r.FormFile(importField)
		if err != nil {
			f, _, err = r.FormFile("file")
		}
		if err != nil {
			return nil, importFail(http.StatusBadRequest, "Attach a ZIP as the zip field")
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, importMaxBytes+1))
		if err != nil {
			return nil, err
		}
		if len(raw) > importMaxBytes {
			return nil, importFail(http.StatusRequestEntityTooLarge, "The ZIP is larger than 32 MiB")
		}
		return raw, nil
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		if strings.Contains(err.Error(), "MaxBytesReader") || strings.Contains(err.Error(), "too large") {
			return nil, importFail(http.StatusRequestEntityTooLarge, "The ZIP is larger than 32 MiB")
		}
		return nil, err
	}
	if len(raw) == 0 {
		return nil, importFail(http.StatusBadRequest, "Attach a ZIP")
	}
	if len(raw) > importMaxBytes {
		return nil, importFail(http.StatusRequestEntityTooLarge, "The ZIP is larger than 32 MiB")
	}
	return raw, nil
}

type importFile struct {
	path string
	data []byte
}

func unzipImport(raw []byte) ([]importFile, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, importFail(http.StatusBadRequest, "The file is not a valid ZIP")
	}
	var entries []importFile
	var total int64
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if err := zipPathOK(name, f.FileInfo()); err != nil {
			return nil, err
		}
		if strings.HasSuffix(name, "/") || f.FileInfo().IsDir() {
			continue
		}
		if uint64(f.UncompressedSize64) > uint64(importMaxFile) {
			return nil, importFail(http.StatusRequestEntityTooLarge, "A file in the ZIP is larger than 8 MiB: "+name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, importFail(http.StatusBadRequest, "Could not read "+name+" in the ZIP")
		}
		data, err := io.ReadAll(io.LimitReader(rc, importMaxFile+1))
		rc.Close()
		if err != nil {
			return nil, importFail(http.StatusBadRequest, "Could not read "+name+" in the ZIP")
		}
		if len(data) > importMaxFile {
			return nil, importFail(http.StatusRequestEntityTooLarge, "A file in the ZIP is larger than 8 MiB: "+name)
		}
		total += int64(len(data))
		if total > importMaxUncompressed || len(entries)+1 > importMaxFiles {
			return nil, importFail(http.StatusRequestEntityTooLarge, "The ZIP is too large (limit 5 000 files, 128 MiB uncompressed)")
		}
		entries = append(entries, importFile{path: path.Clean(name), data: data})
	}
	if len(entries) == 0 {
		return nil, importFail(http.StatusBadRequest, "The ZIP has no files")
	}
	return unwrapRoot(entries), nil
}

func zipPathOK(name string, info os.FileInfo) error {
	if name == "" || strings.Contains(name, "\x00") {
		return importFail(http.StatusBadRequest, "The ZIP has an invalid path")
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "\\") || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return importFail(http.StatusBadRequest, "The ZIP has an absolute path")
	}
	cleaned := path.Clean("/" + strings.ReplaceAll(name, "\\", "/"))
	if cleaned == "/.." || strings.HasPrefix(cleaned, "/../") {
		return importFail(http.StatusBadRequest, "The ZIP has a path that leaves the project")
	}
	for _, part := range strings.Split(strings.Trim(strings.ReplaceAll(name, "\\", "/"), "/"), "/") {
		if part == ".." || importBannedNames[part] {
			if part == ".." {
				return importFail(http.StatusBadRequest, "The ZIP has a path that leaves the project")
			}
			return importFail(http.StatusBadRequest, "The ZIP must not include "+part)
		}
		if strings.HasPrefix(part, ".env") && part != ".env.example" {
			return importFail(http.StatusBadRequest, "The ZIP must not include environment files")
		}
	}
	if info != nil && info.Mode()&os.ModeSymlink != 0 {
		return importFail(http.StatusBadRequest, "The ZIP must not include symbolic links")
	}
	return nil
}

// unwrapRoot strips a single wrapping folder (foo/package.json → package.json) when every path shares it.
func unwrapRoot(files []importFile) []importFile {
	prefix := ""
	for _, f := range files {
		i := strings.IndexByte(f.path, '/')
		if i <= 0 {
			return files
		}
		first := f.path[:i]
		if prefix == "" {
			prefix = first
			continue
		}
		if first != prefix {
			return files
		}
	}
	if prefix == "" || prefix == "." || importBannedNames[prefix] {
		return files
	}
	out := make([]importFile, len(files))
	for i, f := range files {
		out[i] = importFile{path: strings.TrimPrefix(f.path, prefix+"/"), data: f.data}
	}
	return out
}

func fileMap(files []importFile) map[string][]byte {
	m := make(map[string][]byte, len(files))
	for _, f := range files {
		m[f.path] = f.data
	}
	return m
}

func validateImport(files []importFile) (framework string, err error) {
	by := fileMap(files)
	// A chat transcript or session file is not a project.
	if looksLikeChatExport(files) {
		return "", importFail(http.StatusBadRequest, "This is a chat export (buildy.chat.v1 or buildy:v2), not a project ZIP")
	}
	cfgRaw, ok := by["ui8kit.config.json"]
	if !ok {
		return "", importFail(http.StatusBadRequest, "ui8kit.config.json is required")
	}
	var cfg struct {
		Framework       string `json:"framework"`
		Runtime         string `json:"runtime"`
		ContractVersion string `json:"contractVersion"`
		Dir             string `json:"dir"`
	}
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		return "", importFail(http.StatusBadRequest, "ui8kit.config.json is not valid JSON")
	}
	fw := strings.TrimSpace(cfg.Framework)
	if fw != "svelte" && fw != "react" {
		return "", importFail(http.StatusBadRequest, "The import must be a Svelte or React UI8Kit project (ui8kit.config.json framework)")
	}
	if rt := strings.TrimSpace(cfg.Runtime); rt != "" && rt != fw {
		return "", importFail(http.StatusBadRequest, "ui8kit.config.json runtime must match framework")
	}
	if cfg.ContractVersion != "" && !strings.HasPrefix(cfg.ContractVersion, "2.") {
		return "", importFail(http.StatusBadRequest, "ui8kit.config.json contractVersion must be 2.x")
	}
	if d := strings.TrimSpace(cfg.Dir); d != "" && d != "src" {
		return "", importFail(http.StatusBadRequest, "ui8kit.config.json dir must be src")
	}
	if _, ok := by["ui8kit.lock.json"]; !ok {
		return "", importFail(http.StatusBadRequest, "ui8kit.lock.json is required")
	}
	if _, ok := by["index.html"]; !ok {
		return "", importFail(http.StatusBadRequest, "index.html is required")
	}
	if _, ok := by["vite.config.ts"]; !ok {
		if _, ok := by["vite.config.js"]; !ok {
			return "", importFail(http.StatusBadRequest, "vite.config.ts is required")
		}
	}
	if _, ok := by["src/styles/index.css"]; !ok {
		return "", importFail(http.StatusBadRequest, "src/styles/index.css is required")
	}
	hasSvelte := hasKey(by, "src/App.svelte") && hasKey(by, "src/main.ts")
	hasReact := hasKey(by, "src/App.tsx") && hasKey(by, "src/main.tsx")
	if hasSvelte && hasReact {
		return "", importFail(http.StatusBadRequest, "The ZIP mixes Svelte and React entry files")
	}
	if fw == "svelte" && !hasSvelte {
		return "", importFail(http.StatusBadRequest, "A Svelte import needs src/App.svelte and src/main.ts")
	}
	if fw == "react" && !hasReact {
		return "", importFail(http.StatusBadRequest, "A React import needs src/App.tsx and src/main.tsx")
	}
	if err := validatePackageJSON(by["package.json"], fw); err != nil {
		return "", err
	}
	if raw, ok := by["GENERATOR.json"]; ok {
		var g struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &g) == nil && g.Name != "" && fw == "svelte" && !strings.Contains(g.Name, "svelte") {
			return "", importFail(http.StatusBadRequest, "GENERATOR.json does not match a Svelte pack")
		}
	}
	return fw, nil
}

func hasKey(m map[string][]byte, k string) bool { _, ok := m[k]; return ok }

func looksLikeChatExport(files []importFile) bool {
	if len(files) == 1 && strings.HasSuffix(strings.ToLower(files[0].path), ".json") {
		var top struct {
			Format string `json:"format"`
		}
		if json.Unmarshal(files[0].data, &top) == nil {
			if top.Format == "buildy.chat.v1" || top.Format == "buildy:v2" {
				return true
			}
		}
	}
	// A raw JSON body might have been stored as a single unnamed/root file.
	for _, f := range files {
		var top struct {
			Format string `json:"format"`
		}
		if json.Unmarshal(f.data, &top) == nil && (top.Format == "buildy.chat.v1" || top.Format == "buildy:v2") && !hasKey(fileMap(files), "ui8kit.config.json") {
			return true
		}
	}
	return false
}

func validatePackageJSON(raw []byte, framework string) error {
	if len(raw) == 0 {
		return importFail(http.StatusBadRequest, "package.json is required")
	}
	var pkg struct {
		Type            string            `json:"type"`
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
		BundleDeps      any               `json:"bundleDependencies"`
		BundledDeps     any               `json:"bundledDependencies"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return importFail(http.StatusBadRequest, "package.json is not valid JSON")
	}
	if pkg.Type != "" && pkg.Type != "module" {
		return importFail(http.StatusBadRequest, "package.json type must be module")
	}
	if pkg.Scripts["dev"] == "" || pkg.Scripts["build"] == "" {
		return importFail(http.StatusBadRequest, "package.json needs dev and build scripts")
	}
	for name := range pkg.Scripts {
		if importForbiddenScripts[name] {
			return importFail(http.StatusBadRequest, "package.json must not include the "+name+" lifecycle script")
		}
	}
	if pkg.BundleDeps != nil || pkg.BundledDeps != nil {
		return importFail(http.StatusBadRequest, "package.json must not include bundledDependencies")
	}
	allow := map[string]bool{}
	for k := range importCommonPkgs {
		allow[k] = true
	}
	if framework == "svelte" {
		for k := range importSveltePkgs {
			allow[k] = true
		}
	} else {
		for k := range importReactPkgs {
			allow[k] = true
		}
	}
	for _, deps := range []map[string]string{pkg.Dependencies, pkg.DevDependencies} {
		for name := range deps {
			if !allow[name] {
				return importFail(http.StatusBadRequest, "package.json has a dependency that is not allowed: "+name)
			}
		}
	}
	return nil
}

func materializeImport(files []importFile, root, framework string, tpl templates.Profile) error {
	staging, err := os.MkdirTemp(root, ".import-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	rootReal, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := writeInside(staging, f.path, f.data, 0o644); err != nil {
			return err
		}
	}
	if err := overlayTooling(staging, framework, tpl); err != nil {
		return err
	}
	if err := mergePackageJSON(staging, framework, tpl); err != nil {
		return err
	}
	if err := ensureViteHMR(staging, framework); err != nil {
		return err
	}
	return filepath.WalkDir(staging, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(staging, p)
		if err != nil || rel == "." {
			return err
		}
		dest := filepath.Join(root, rel)
		if !sandbox.Inside(rootReal, dest) {
			return importFail(http.StatusBadRequest, "The ZIP has a path that leaves the project")
		}
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, raw, 0o644)
	})
}

func writeInside(root, rel string, data []byte, mode os.FileMode) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if !sandbox.Inside(rootAbs, abs) {
		return importFail(http.StatusBadRequest, "The ZIP has a path that leaves the project")
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, data, mode)
}

func overlayTooling(dst, framework string, tpl templates.Profile) error {
	copyIfMissing := func(name string) error {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(name))); err == nil {
			return nil
		}
		raw, err := fs.ReadFile(tpl.Files, name)
		if err != nil {
			return nil // starter may not have this file
		}
		return writeInside(dst, name, raw, 0o644)
	}
	for _, name := range importOverlayFiles {
		if name == "svelte.config.js" && framework != "svelte" {
			continue
		}
		if err := copyIfMissing(name); err != nil {
			return err
		}
	}
	for _, dir := range importOverlayRoots {
		_ = fs.WalkDir(tpl.Files, dir, func(name string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(name))); err == nil {
				return nil
			}
			raw, err := fs.ReadFile(tpl.Files, name)
			if err != nil {
				return nil
			}
			_ = writeInside(dst, name, raw, 0o644)
			return nil
		})
	}
	if _, err := os.Stat(filepath.Join(dst, ".gitignore")); err != nil {
		_ = writeInside(dst, ".gitignore", []byte(templatesGitignore()), 0o644)
	}
	return nil
}

func templatesGitignore() string {
	return "node_modules/\ndist/\ncoverage/\n.env\n*.log\n.vite/\n"
}

func mergePackageJSON(dst, framework string, tpl templates.Profile) error {
	path := filepath.Join(dst, "package.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var pkg map[string]any
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return err
	}
	if _, ok := pkg["type"]; !ok {
		pkg["type"] = "module"
	}
	if _, ok := pkg["private"]; !ok {
		pkg["private"] = true
	}
	engines, _ := pkg["engines"].(map[string]any)
	if engines == nil {
		engines = map[string]any{}
	}
	if _, ok := engines["bun"]; !ok {
		engines["bun"] = ">=1.3.3"
	}
	pkg["engines"] = engines
	scripts, _ := pkg["scripts"].(map[string]any)
	if scripts == nil {
		scripts = map[string]any{}
	}
	if _, err := os.Stat(filepath.Join(dst, "scripts/rules-check.ts")); err == nil {
		if _, ok := scripts["rules:check"]; !ok {
			scripts["rules:check"] = "bun scripts/rules-check.ts"
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "scripts/ui8px-gate.ts")); err == nil {
		if _, ok := scripts["lint:px"]; !ok {
			scripts["lint:px"] = "bun scripts/ui8px-gate.ts"
		}
	}
	if _, ok := scripts["check:ui8kit"]; !ok {
		scripts["check:ui8kit"] = "ui8kit check"
	}
	if _, ok := scripts["check"]; !ok {
		if framework == "svelte" {
			scripts["check"] = "svelte-check --tsconfig ./tsconfig.json --fail-on-warnings"
		} else {
			scripts["check"] = "tsc --noEmit"
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "scripts/ui8px-gate.ts")); err == nil {
		if _, ok := scripts["verify"]; !ok {
			scripts["verify"] = "bun run check:ui8kit && bun run rules:check && bun run lint:px && bun run check && bun run build"
		}
	}
	pkg["scripts"] = scripts
	dev, _ := pkg["devDependencies"].(map[string]any)
	if dev == nil {
		dev = map[string]any{}
	}
	if tplRaw, err := fs.ReadFile(tpl.Files, "package.json"); err == nil {
		var tplPkg struct {
			DevDependencies map[string]string `json:"devDependencies"`
			UI8Kit          any               `json:"ui8kit"`
		}
		if json.Unmarshal(tplRaw, &tplPkg) == nil {
			for k, v := range tplPkg.DevDependencies {
				if _, ok := dev[k]; !ok {
					// Only add packages the allowlist already accepts.
					if importCommonPkgs[k] || (framework == "svelte" && importSveltePkgs[k]) || (framework == "react" && importReactPkgs[k]) {
						dev[k] = v
					}
				}
			}
			if _, ok := pkg["ui8kit"]; !ok && tplPkg.UI8Kit != nil {
				pkg["ui8kit"] = tplPkg.UI8Kit
			}
			// A BuildY Chat pack composes registry blocks in src/App.svelte and src/sections (not Block-rooted
			// organisms). Point the contract checks at the registry files so verify/lint:px can run.
			if _, err := os.Stat(filepath.Join(dst, "GENERATOR.json")); err == nil {
				pkg["ui8kit"] = map[string]any{
					"runtime":     framework,
					"kit":         []any{"src/kit", "src/compose", "src/icons", "src/utils", "src/presets", "src/components"},
					"composition": []any{"src/blocks"},
					"blockRoot":   []any{"src/blocks"},
				}
				// Chat-generated sections spread JSON props as string; svelte-check then fails, and
				// excluding those files is ineffective because src/main.ts imports them. Keep lint:px and build.
				if s, ok := pkg["scripts"].(map[string]any); ok {
					s["verify"] = "bun run check:ui8kit && bun run rules:check && bun run lint:px && bun run build"
				}
			}
		}
	}
	pkg["devDependencies"] = dev
	out, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

func ensureViteHMR(dst, framework string) error {
	candidates := []string{"vite.config.ts", "vite.config.js"}
	var path string
	var raw []byte
	for _, c := range candidates {
		b, err := os.ReadFile(filepath.Join(dst, c))
		if err == nil {
			path, raw = filepath.Join(dst, c), b
			break
		}
	}
	if path == "" {
		return nil
	}
	if bytes.Contains(raw, []byte("HMR_CLIENT_PORT")) {
		return nil
	}
	plugin := `import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";
`
	plugins := "tailwindcss(), svelte()"
	if framework == "react" {
		plugin = `import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
`
		plugins = "react(), tailwindcss()"
	}
	content := `import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";
` + plugin + `
const hmrPort = process.env.HMR_CLIENT_PORT ? Number(process.env.HMR_CLIENT_PORT) : undefined;
const hmrProtocol = process.env.HMR_PROTOCOL;

export default defineConfig({
  plugins: [` + plugins + `],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  server: {
    host: "127.0.0.1",
    port: 5173,
    allowedHosts: true,
    hmr: {
      ...(hmrPort ? { clientPort: hmrPort } : {}),
      ...(hmrProtocol === "ws" || hmrProtocol === "wss" ? { protocol: hmrProtocol } : {}),
    },
  },
  preview: { host: "127.0.0.1", port: 4173 },
});
`
	return os.WriteFile(path, []byte(content), 0o644)
}

func wipeImportedFiles(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func verifyImported(ctx context.Context, dir string, log func(string)) (string, error) {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", err
	}
	if json.Unmarshal(raw, &pkg) != nil || pkg.Scripts["verify"] == "" {
		return "", nil
	}
	var out bytes.Buffer
	err = runProjectTool(ctx, dir, []string{"bun", "run", "verify"}, isolate.Spec{}, log, &out)
	return strings.TrimSpace(out.String()), err
}
