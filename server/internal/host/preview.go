package host

import (
	"context"
	"net/http"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// PreviewStatus is a project's dev server state.
type PreviewStatus struct {
	// Status: stopped | starting | running | error.
	Status string
	Ready  bool
	// URL is the preview proxy origin (no token); "" when stopped.
	URL   string
	Error string
}

// Preview runs one dev server per project behind a loopback proxy with one-time tokens (internal/preview).
type Preview interface {
	Ensure(projectID, dir string, profile templates.Profile) (PreviewStatus, error)
	Status(projectID string) PreviewStatus
	// Grant mints a one-time URL that opens the preview (the proxy trades it for a cookie).
	Grant(projectID string) (url string, expires time.Time, err error)
	Stop(projectID string)
	StopAll()
}

func (h *Host) registerPreview() {
	h.router.Handle("POST /projects/:id/ensure", h.ensurePreview)
	h.router.Handle("POST /projects/:id/stop", h.stopPreview)
	h.router.Handle("GET /projects/:id/status", h.previewStatus)
	h.router.Handle("POST /projects/:id/preview/grant", h.previewGrant)
}

func (h *Host) noPreview(w http.ResponseWriter) bool {
	if h.opts.Preview == nil {
		WriteError(w, http.StatusServiceUnavailable, "Live preview is not available in this build")
		return true
	}
	if !h.hasTool("bun") {
		WriteError(w, http.StatusServiceUnavailable, "Live preview needs Bun (https://bun.sh); install it and restart VibeChat")
		return true
	}
	return false
}

func (h *Host) ensurePreview(w http.ResponseWriter, r *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok || h.noPreview(w) {
		return
	}
	tpl, ok := h.template(p.TemplateID)
	if !ok {
		WriteError(w, http.StatusConflict, "This project's template has no preview")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	if err := h.waitSetup(ctx, p); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	st, err := h.opts.Preview.Ensure(p.ID, p.Path, tpl)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "Could not start the preview: "+err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"status": st.Status, "previewUrl": st.URL})
}

func (h *Host) stopPreview(w http.ResponseWriter, _ *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return
	}
	if h.opts.Preview != nil {
		h.opts.Preview.Stop(p.ID)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"status": "stopped"})
}

func (h *Host) previewStatus(w http.ResponseWriter, _ *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return
	}
	if h.opts.Preview == nil {
		WriteJSON(w, http.StatusOK, map[string]any{"status": "stopped", "previewUrl": nil, "ready": false})
		return
	}
	st := h.opts.Preview.Status(p.ID)
	var url any
	if st.URL != "" {
		url = st.URL
	}
	out := map[string]any{"status": st.Status, "previewUrl": url, "ready": st.Ready}
	if st.Error != "" {
		out["error"] = st.Error
	}
	WriteJSON(w, http.StatusOK, out)
}

func (h *Host) previewGrant(w http.ResponseWriter, _ *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok || h.noPreview(w) {
		return
	}
	url, exp, err := h.opts.Preview.Grant(p.ID)
	if err != nil {
		WriteError(w, http.StatusConflict, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"url": url, "expiresAt": exp.UTC().Format(time.RFC3339)})
}
