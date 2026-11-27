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
	"github.com/buildytech/vibechat-desktop/server/internal/sandbox"
	"github.com/buildytech/vibechat-desktop/server/internal/store"
	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// Import ZIP (POST /projects/:id/import.zip): a BuildY Chat pack, a UI8Kit svelte/react project or a Go templ
// project (fastygo/starter), only into a workspace with no files except .git. This file is the one place of the
// checks every framework shares (emptiness, the size limits, path safety, symbolic links, environment files,
// executables, binaries) and the flow; import_adapter.go is the adapter registry, and each framework adapter adds its
// own checks, fills its core (tooling) from the template and verifies the result. docs/adr/0001-import-adapters-and-go-runner.md.

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
	importBannedNames = map[string]bool{
		"node_modules": true, ".git": true,
	}
)

func (h *Host) importZip(w http.ResponseWriter, r *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return
	}
	fail := func(err error) {
		WriteError(w, ImportErrorStatus(err), err.Error())
	}
	if err := projectFileCount(p.Path); err != nil {
		fail(err)
		return
	}
	raw, err := readImportBody(w, r)
	if err != nil {
		if _, ok := err.(*importError); !ok {
			err = importFail(http.StatusBadRequest, "Could not read the ZIP")
		}
		fail(err)
		return
	}
	im, err := ImportInto(r.Context(), p.Path, raw, h.template)
	if err != nil {
		fail(err)
		return
	}
	framework := im.Framework
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
	if err := InstallDeps(ctx, p.Path, im.Template, h.opts.Log); err != nil {
		WriteError(w, http.StatusInternalServerError, "Imported, but installing dependencies failed: "+err.Error())
		return
	}
	verifyLog, verifyErr := im.Verify(ctx, p.Path, h.opts.Log)
	out := map[string]any{
		"ok":        true,
		"framework": framework,
		"files":     im.Files,
		"layers":    im.Layers,
		"project":   h.projectJSON(p),
		"verify":    map[string]any{"ok": verifyErr == nil, "log": verifyLog},
	}
	if verifyErr != nil {
		out["verify"].(map[string]any)["error"] = verifyErr.Error()
	}
	WriteJSON(w, http.StatusOK, out)
}

// Imported is a ZIP written into a project folder (not committed, dependencies not installed).
type Imported struct {
	Framework string
	Files     int
	Layers    map[string]int // core, data, code (importLayers)
	Template  templates.Profile
	adapter   importAdapter
}

// Verify runs the adapter's check of the written project (after InstallDeps).
func (im *Imported) Verify(ctx context.Context, dir string, log func(string)) (string, error) {
	return im.adapter.Verify(ctx, dir, im.Template, log)
}

// ImportErrorStatus is the HTTP status of an ImportInto error (500 for an unexpected one).
func ImportErrorStatus(err error) int {
	if ie, ok := err.(*importError); ok {
		return ie.status
	}
	return http.StatusInternalServerError
}

// ImportInto checks a ZIP (shared checks, the adapter's) and writes it into dir, which must have no file but .git;
// template answers the template of a framework. On a write error dir is emptied again.
func ImportInto(ctx context.Context, dir string, raw []byte, template func(id string) (templates.Profile, bool)) (*Imported, error) {
	if err := projectFileCount(dir); err != nil {
		return nil, err
	}
	files, err := unzipImport(raw)
	if err != nil {
		if _, ok := err.(*importError); !ok {
			err = importFail(http.StatusBadRequest, "The file is not a valid ZIP")
		}
		return nil, err
	}
	in := newImportInput(files)
	ad, err := selectImportAdapter(in)
	if err != nil {
		return nil, err
	}
	framework := ad.ID()
	tpl, ok := template(framework)
	if !ok || !tpl.Enabled {
		return nil, importFail(http.StatusInternalServerError, "This build has no "+framework+" template to finish the import")
	}
	if tpl.Runner != "" && tpl.Runner != ad.Runner() {
		return nil, importFail(http.StatusInternalServerError, "The "+framework+" template runs with "+tpl.Runner+", the import needs "+ad.Runner())
	}
	vctx, vcancel := context.WithTimeout(ctx, 6*time.Minute)
	err = ad.Validate(vctx, in, tpl)
	vcancel()
	if err != nil {
		if _, ok := err.(*importError); !ok {
			err = importFail(http.StatusBadRequest, err.Error())
		}
		return nil, err
	}
	// Recheck emptiness right before the write (another tab may have added a file).
	if err := projectFileCount(dir); err != nil {
		return nil, err
	}
	if err := materializeImport(in, dir, ad, tpl); err != nil {
		_ = wipeImportedFiles(dir)
		return nil, importFail(http.StatusInternalServerError, "Could not write the imported files: "+err.Error())
	}
	return &Imported{Framework: framework, Files: len(files), Layers: importLayers(in, ad, tpl), Template: tpl, adapter: ad}, nil
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

// materializeImport writes the ZIP into a staging folder inside the project, lets the adapter fill its core there,
// and moves the result into the project (executable bits kept: the Go starter's scripts).
func materializeImport(in *importInput, root string, ad importAdapter, tpl templates.Profile) error {
	staging, err := os.MkdirTemp(root, ".import-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	rootReal, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	for _, f := range in.files {
		if err := writeInside(staging, f.path, f.data, 0o644); err != nil {
			return err
		}
	}
	if err := ad.Fill(staging, in, tpl); err != nil {
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
		info, err := d.Info()
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o100 != 0 {
			mode = 0o755
		}
		if err := os.WriteFile(dest, raw, mode); err != nil {
			return err
		}
		return os.Chmod(dest, mode)
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

// sharedImportChecks are the content checks of every framework (the path checks run while unzipping): no
// executable binaries (ELF, Mach-O, PE) anywhere in a site project.
func sharedImportChecks(in *importInput) error {
	for _, f := range in.files {
		if isExecutableBinary(f.data) {
			return importFail(http.StatusBadRequest, "The ZIP must not include executable binaries: "+f.path)
		}
	}
	return nil
}

func isExecutableBinary(b []byte) bool {
	switch {
	case bytes.HasPrefix(b, []byte("\x7fELF")):
		return true
	case bytes.HasPrefix(b, []byte{0xcf, 0xfa, 0xed, 0xfe}), bytes.HasPrefix(b, []byte{0xce, 0xfa, 0xed, 0xfe}),
		bytes.HasPrefix(b, []byte{0xfe, 0xed, 0xfa, 0xcf}), bytes.HasPrefix(b, []byte{0xca, 0xfe, 0xba, 0xbe}):
		return true
	case len(b) > 0x40 && b[0] == 'M' && b[1] == 'Z':
		off := int(b[0x3c]) | int(b[0x3d])<<8 | int(b[0x3e])<<16 | int(b[0x3f])<<24
		return off > 0 && off+4 <= len(b) && bytes.Equal(b[off:off+4], []byte("PE\x00\x00"))
	}
	return false
}
