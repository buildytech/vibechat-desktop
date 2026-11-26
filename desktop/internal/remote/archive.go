package remote

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The project's ZIP from the server's Studio (POST /runtime/v1/desktop/archive, DesktopArchiveInputSchema): the app's
// webviews do not all save downloads (WKWebView in Wails has no download delegate), so the app asks where to save
// (Options.SaveAs, the system's Save dialog) and writes GET /projects/:id/archive.zip there through the tunnel, with
// the window's own session cookie. Only the proxy answers it (a window signed in to the server).

// ArchiveInput is DesktopArchiveInputSchema.
type ArchiveInput struct {
	ProjectID string `json:"projectId"`
}

// Archive is DesktopArchiveSchema.
type Archive struct {
	Saved bool   `json:"saved"`
	File  string `json:"file,omitempty"`
}

var projectIDOK = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ArchivePath is the seam path of the archive route.
func (m *Manager) ArchivePath() string { return m.o.Prefix + "/desktop/archive" }

// archiveName is the file name the server suggests (Content-Disposition), reduced to a plain base name.
func archiveName(res *http.Response) string {
	name := "project.zip"
	if _, p, err := mime.ParseMediaType(res.Header.Get("Content-Disposition")); err == nil && p["filename"] != "" {
		name = p["filename"]
	}
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	if name == "." || name == "/" || name == "" || strings.HasPrefix(name, ".") {
		name = "project.zip"
	}
	if !strings.HasSuffix(strings.ToLower(name), ".zip") {
		name += ".zip"
	}
	return name
}

func (m *Manager) serveArchive(w http.ResponseWriter, r *http.Request, s *session) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}
	var in ArchiveInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil || !projectIDOK.MatchString(in.ProjectID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: projectId"})
		return
	}
	if m.o.SaveAs == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "This app cannot save files"})
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet,
		"http://"+ServerAPI+m.o.Prefix+"/projects/"+url.PathEscape(in.ProjectID)+"/archive.zip", nil)
	req.Header.Set("Cookie", r.Header.Get("Cookie"))
	// No client timeout: a large project streams for a while; the window's request bounds it.
	res, err := (&http.Client{Transport: s.tr}).Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "The server is unreachable over SSH: " + err.Error()})
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/zip") {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&body)
		if body.Error == "" {
			body.Error = "The server answered " + res.Status
		}
		code := res.StatusCode
		if code == http.StatusOK || code == http.StatusNotFound {
			// 404 stays the Studio's "not in the app" signal; a missing project or an old server is a gateway error.
			code = http.StatusBadGateway
		}
		writeJSON(w, code, map[string]string{"error": body.Error})
		return
	}
	dst, err := m.o.SaveAs(archiveName(res))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "The Save dialog failed: " + err.Error()})
		return
	}
	if dst == "" {
		writeJSON(w, http.StatusOK, Archive{})
		return
	}
	if err := saveStream(dst, res.Body); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "The ZIP was not saved: " + err.Error()})
		return
	}
	m.o.Log("archive of " + in.ProjectID + " saved to " + dst)
	writeJSON(w, http.StatusOK, Archive{Saved: true, File: dst})
}

// saveStream writes body next to dst and moves it there only when it is complete, so a dropped tunnel leaves no
// half ZIP under the chosen name.
func saveStream(dst string, body io.Reader) error {
	f, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*.part")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = io.Copy(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if err = os.Rename(tmp, dst); err != nil && errors.Is(err, os.ErrExist) {
			if err = os.Remove(dst); err == nil {
				err = os.Rename(tmp, dst)
			}
		}
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
