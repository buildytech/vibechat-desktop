package host

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/store"
	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// projectSetup is the host's dependency install for a project (outside the agent sandbox, with network).
type projectSetup struct {
	done chan struct{}
	err  error
}

func (h *Host) registerProjects() {
	h.router.Handle("GET /templates", h.listTemplates)
	h.router.Handle("GET /projects", h.listProjects)
	h.router.Handle("POST /projects", h.createProject)
	h.router.Handle("POST /projects/:id/clone", h.cloneProject)
	h.router.Handle("POST /projects/:id/delete", h.deleteProject)
}

func (h *Host) template(id string) (templates.Profile, bool) {
	for _, t := range h.opts.Templates {
		if t.Is(id) {
			return t, true
		}
	}
	return templates.Profile{}, false
}

func (h *Host) listTemplates(w http.ResponseWriter, _ *http.Request, _ Params) {
	out := make([]templates.Profile, 0, len(h.opts.Templates))
	for _, t := range h.opts.Templates {
		t.Enabled = t.Enabled && h.hasTool("git")
		out = append(out, t)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"templates": out})
}

func (h *Host) projectJSON(p store.Project) map[string]any {
	status, url := "stopped", any(nil)
	if s := h.setupState(p.ID); s == "installing" {
		status = "starting"
	}
	if h.opts.Preview != nil {
		st := h.opts.Preview.Status(p.ID)
		if st.Status != "stopped" {
			status = st.Status
		}
		if st.URL != "" {
			url = st.URL
		}
	}
	tid := p.TemplateID
	if t, ok := h.template(tid); ok {
		tid = t.ID // a former id (alias) reads as the template's current id
	}
	return map[string]any{"id": p.ID, "name": p.Name, "status": status, "preview_url": url, "template_id": tid, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt}
}

func (h *Host) listProjects(w http.ResponseWriter, _ *http.Request, _ Params) {
	out := []map[string]any{}
	for _, p := range h.opts.Store.Projects() {
		out = append(out, h.projectJSON(p))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"projects": out})
}

var slugBad = regexp.MustCompile(`[^a-z0-9]+`)

// cyrLatin transliterates Cyrillic so a Russian project name gets a readable folder ("Кофейня Зерно" →
// kofeynya-zerno) instead of the bare fallback "project".
var cyrLatin = strings.NewReplacer(
	"а", "a", "б", "b", "в", "v", "г", "g", "д", "d", "е", "e", "ё", "e", "ж", "zh", "з", "z", "и", "i", "й", "y",
	"к", "k", "л", "l", "м", "m", "н", "n", "о", "o", "п", "p", "р", "r", "с", "s", "т", "t", "у", "u", "ф", "f",
	"х", "kh", "ц", "ts", "ч", "ch", "ш", "sh", "щ", "shch", "ъ", "", "ы", "y", "ь", "", "э", "e", "ю", "yu", "я", "ya",
	"і", "i", "ї", "yi", "є", "ye", "ґ", "g",
)

// projectSlug is the folder name for a project name: lower-case ASCII words joined by "-", at most 40 bytes.
func projectSlug(name string) string {
	slug := strings.Trim(slugBad.ReplaceAllString(cyrLatin.Replace(strings.ToLower(name)), "-"), "-")
	if slug == "" {
		slug = "project"
	}
	if len(slug) > 40 {
		slug = strings.TrimRight(slug[:40], "-")
	}
	return slug
}

// folderFor picks a new folder under ProjectsDir for name.
func (h *Host) folderFor(name string) (string, error) {
	slug := projectSlug(name)
	if err := os.MkdirAll(h.opts.ProjectsDir, 0o755); err != nil {
		return "", err
	}
	for i := 0; i < 1000; i++ {
		cand := slug
		if i > 0 {
			cand = fmt.Sprintf("%s-%d", slug, i+1)
		}
		p := filepath.Join(h.opts.ProjectsDir, cand)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no free folder name for %q", name)
}

func (h *Host) createProject(w http.ResponseWriter, r *http.Request, _ Params) {
	var body struct {
		Name       string `json:"name"`
		TemplateID string `json:"templateId"`
	}
	if err := readJSON(r, 16<<10, &body); err != nil {
		WriteError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len([]rune(name)) > 80 {
		WriteError(w, http.StatusBadRequest, "Name the project (1–80 characters)")
		return
	}
	tid := body.TemplateID
	if tid == "" && len(h.opts.Templates) > 0 {
		tid = h.opts.Templates[0].ID
	}
	tpl, ok := h.template(tid)
	if !ok || !tpl.Enabled {
		WriteError(w, http.StatusBadRequest, "Unknown template: "+tid)
		return
	}
	if !h.hasTool("git") {
		WriteError(w, http.StatusServiceUnavailable, "git is required to create projects; install git and restart VibeChat")
		return
	}
	dir, err := h.folderFor(name)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tpl.Layer != nil {
		// A layer template (the Go starter): its pinned tree is downloaded and checked once, then cached.
		lctx, lcancel := context.WithTimeout(r.Context(), 6*time.Minute)
		_, err := templates.EnsureLayer(lctx, tpl.Layer)
		lcancel()
		if err != nil {
			WriteError(w, http.StatusBadGateway, "Could not get the template: "+err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := templates.Scaffold(ctx, tpl, dir); err != nil {
		_ = os.RemoveAll(dir)
		WriteError(w, http.StatusInternalServerError, "Could not create the project: "+err.Error())
		return
	}
	now := store.Now()
	p := store.Project{ID: store.NewID(), Name: name, TemplateID: tpl.ID, Path: dir, CreatedAt: now, UpdatedAt: now}
	if err := h.opts.Store.PutProject(p); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.startSetup(p, tpl)
	WriteJSON(w, http.StatusCreated, map[string]any{"project": h.projectJSON(p)})
}

// startSetup installs dependencies in the background (once per project per app run).
func (h *Host) startSetup(p store.Project, tpl templates.Profile) *projectSetup {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.setup[p.ID]; ok {
		return s
	}
	s := &projectSetup{done: make(chan struct{})}
	h.setup[p.ID] = s
	go func() {
		defer close(s.done)
		if len(tpl.Install) == 0 {
			return
		}
		if _, err := os.Stat(filepath.Join(p.Path, "node_modules")); err == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		s.err = InstallDeps(ctx, p.Path, tpl, h.opts.Log)
	}()
	return s
}

func (h *Host) setupState(id string) string {
	h.mu.Lock()
	s, ok := h.setup[id]
	h.mu.Unlock()
	if !ok {
		return ""
	}
	select {
	case <-s.done:
		if s.err != nil {
			return "error"
		}
		return "ready"
	default:
		return "installing"
	}
}

// waitSetup makes sure dependencies are installed in the project folder (starting the install when needed).
func (h *Host) waitSetup(ctx context.Context, p store.Project) error {
	tpl, ok := h.template(p.TemplateID)
	if !ok {
		return fmt.Errorf("template %s is not part of this build", p.TemplateID)
	}
	s := h.startSetup(p, tpl)
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if s.err != nil {
		h.mu.Lock()
		delete(h.setup, p.ID) // retry on the next call
		h.mu.Unlock()
		return fmt.Errorf("installing dependencies failed: %v", s.err)
	}
	return nil
}

func (h *Host) cloneProject(w http.ResponseWriter, r *http.Request, prm Params) {
	src, err := h.opts.Store.Project(prm["id"])
	if err != nil {
		WriteError(w, http.StatusNotFound, "Project not found")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	_ = readJSON(r, 16<<10, &body)
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = src.Name + " copy"
	}
	dir, err := h.folderFor(name)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if err := runTool(ctx, h.opts.ProjectsDir, []string{"git", "clone", "-q", "--no-hardlinks", src.Path, dir}, nil, h.opts.Log); err != nil {
		WriteError(w, http.StatusInternalServerError, "Could not copy the project: "+err.Error())
		return
	}
	now := store.Now()
	p := store.Project{ID: store.NewID(), Name: name, TemplateID: src.TemplateID, Path: dir, CreatedAt: now, UpdatedAt: now}
	if err := h.opts.Store.PutProject(p); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tpl, ok := h.template(p.TemplateID); ok {
		h.startSetup(p, tpl)
	}
	WriteJSON(w, http.StatusCreated, map[string]any{"project": h.projectJSON(p)})
}

func (h *Host) deleteProject(w http.ResponseWriter, r *http.Request, prm Params) {
	p, err := h.opts.Store.Project(prm["id"])
	if err != nil {
		WriteError(w, http.StatusNotFound, "Project not found")
		return
	}
	var body struct {
		ConfirmName string `json:"confirmName"`
	}
	_ = readJSON(r, 16<<10, &body)
	if strings.TrimSpace(body.ConfirmName) != p.Name {
		WriteError(w, http.StatusBadRequest, "Type the project name to confirm")
		return
	}
	for _, s := range h.opts.Store.Sessions(p.ID) {
		h.stopRun(s.ID)
		h.removeWorktree(p, s)
	}
	if h.opts.Preview != nil {
		h.opts.Preview.Stop(p.ID)
	}
	if err := h.opts.Store.DeleteProject(p.ID); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Only folders the app created under ProjectsDir are removed.
	if rel, err := filepath.Rel(h.opts.ProjectsDir, p.Path); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
		_ = os.RemoveAll(p.Path)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// runTool runs argv in dir (outside the agent sandbox) with a bounded log.
func runTool(ctx context.Context, dir string, argv []string, env []string, log func(string)) error {
	return runToolOutput(ctx, dir, argv, env, log, nil)
}
