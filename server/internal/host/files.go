package host

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/buildytech/vibechat-desktop/server/internal/sandbox"
	"github.com/buildytech/vibechat-desktop/server/internal/store"
)

// fileMaxBytes caps the Studio editor's reads and writes (vibecode WORKSPACE_FILE_MAX_BYTES).
const fileMaxBytes = 1 << 20

var hiddenDirs = map[string]bool{".git": true, "node_modules": true, "dist": true, "coverage": true, ".vite": true}

func (h *Host) registerFiles() {
	h.router.Handle("GET /projects/:id/files", h.listFiles)
	h.router.Handle("GET /projects/:id/file", h.readFile)
	h.router.Handle("PUT /projects/:id/file", h.writeFile)
	h.router.Handle("GET /projects/:id/brief", h.getBrief)
	h.router.Handle("PUT /projects/:id/brief", h.putBrief)
	h.router.Handle("GET /projects/:id/archive.zip", h.archive)
	h.router.Handle("POST /projects/:id/import.zip", h.importZip)
}

type treeNode struct {
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	Type     string      `json:"type"`
	Children []*treeNode `json:"children,omitempty"`
}

func (h *Host) project(w http.ResponseWriter, id string) (store.Project, bool) {
	p, err := h.opts.Store.Project(id)
	if err != nil {
		WriteError(w, http.StatusNotFound, "Project not found")
		return p, false
	}
	return p, true
}

func walkTree(root, rel string, depth int, count *int) []*treeNode {
	if depth > 12 || *count > 5000 {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(root, rel))
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})
	out := []*treeNode{}
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		p := filepath.ToSlash(filepath.Join(rel, e.Name()))
		*count++
		if e.IsDir() {
			if hiddenDirs[e.Name()] {
				continue
			}
			out = append(out, &treeNode{Name: e.Name(), Path: p, Type: "dir", Children: walkTree(root, p, depth+1, count)})
			continue
		}
		out = append(out, &treeNode{Name: e.Name(), Path: p, Type: "file"})
	}
	return out
}

func (h *Host) listFiles(w http.ResponseWriter, _ *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return
	}
	n := 0
	WriteJSON(w, http.StatusOK, map[string]any{"tree": walkTree(p.Path, "", 0, &n)})
}

// resolveInProject maps a workspace-relative path to an absolute one inside the project (no .., no symlink escape).
func resolveInProject(root, rel string) (string, bool) {
	rel = strings.TrimSpace(rel)
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) || filepath.VolumeName(rel) != "" || strings.Contains(rel, "\x00") {
		return "", false
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	// Resolve the nearest existing ancestor so new folders work and symlinks cannot lead out.
	real, rest := abs, ""
	for {
		if r, err := filepath.EvalSymlinks(real); err == nil {
			real = filepath.Join(r, rest)
			break
		}
		parent := filepath.Dir(real)
		if parent == real {
			return "", false
		}
		rest = filepath.Join(filepath.Base(real), rest)
		real = parent
	}
	if !sandbox.Inside(rootReal, real) || real == rootReal {
		return "", false
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == ".git" {
			return "", false
		}
	}
	return abs, true
}

func (h *Host) readFile(w http.ResponseWriter, r *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	abs, ok := resolveInProject(p.Path, rel)
	if !ok {
		WriteError(w, http.StatusBadRequest, "Invalid path")
		return
	}
	st, err := os.Stat(abs)
	if err != nil || st.IsDir() {
		WriteError(w, http.StatusNotFound, "File not found")
		return
	}
	if st.Size() > fileMaxBytes {
		WriteError(w, http.StatusRequestEntityTooLarge, "File is too large for the editor")
		return
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !utf8.Valid(raw) {
		WriteError(w, http.StatusUnsupportedMediaType, "Binary file")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"path": filepath.ToSlash(rel), "content": string(raw), "bytes": len(raw), "encoding": "utf8"})
}

func (h *Host) writeFile(w http.ResponseWriter, r *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return
	}
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := readJSON(r, fileMaxBytes+64<<10, &body); err != nil {
		WriteError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	abs, ok := resolveInProject(p.Path, body.Path)
	if !ok || len(body.Content) > fileMaxBytes {
		WriteError(w, http.StatusBadRequest, "Invalid path or content")
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(abs, []byte(body.Content), 0o644); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "path": filepath.ToSlash(body.Path), "bytes": len(body.Content)})
}

// The product brief (buildy.brief.v1) lives at .buildy/brief.json in the project.
const briefPath = ".buildy/brief.json"

func (h *Host) getBrief(w http.ResponseWriter, _ *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return
	}
	raw, err := os.ReadFile(filepath.Join(p.Path, filepath.FromSlash(briefPath)))
	if err != nil {
		WriteError(w, http.StatusNotFound, "This project has no brief yet")
		return
	}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		WriteError(w, http.StatusUnprocessableEntity, "The brief is not valid JSON")
		return
	}
	WriteJSON(w, http.StatusOK, v)
}

func (h *Host) putBrief(w http.ResponseWriter, r *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return
	}
	var v map[string]any
	if err := readJSON(r, 512<<10, &v); err != nil || v == nil {
		WriteError(w, http.StatusBadRequest, "The brief must be a JSON object")
		return
	}
	raw, _ := json.MarshalIndent(v, "", "  ")
	dst := filepath.Join(p.Path, filepath.FromSlash(briefPath))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(dst, append(raw, '\n'), 0o644); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	identity := map[string]any{"id": p.ID, "name": p.Name}
	if id, ok := v["identity"].(map[string]any); ok {
		identity = id
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "path": briefPath, "identity": identity, "brief": v})
}
