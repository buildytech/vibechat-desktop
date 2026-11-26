package host

import (
	"archive/zip"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The project as a ZIP (GET /projects/:id/archive.zip): the source the agent made, without git, so the owner can take
// the site away. What stays out follows the templates' .gitignore (server/internal/templates) plus what is never the
// project's own: the git folder, installed packages, build output and caches, local env files (secrets; .env.example
// stays), logs, OS litter and the agent's project-level tool config (sandbox/home.go). Symlinks are never followed
// or stored (like the editor's file tree), so nothing outside the project folder can reach the archive.
const (
	archiveMaxBytes = 512 << 20 // uncompressed total
	archiveMaxFiles = 50000
)

var (
	archiveSkipDirs = map[string]bool{
		".git": true, "node_modules": true, "dist": true, "coverage": true, ".vite": true, ".cache": true,
		".turbo": true, ".next": true, ".nuxt": true, ".svelte-kit": true, ".parcel-cache": true, ".output": true,
	}
	// Paths relative to the project root (slash form).
	archiveSkipPaths = map[string]bool{
		".ui8px/telemetry": true, ".ui8px/reports": true,
		".cursor/hooks.json": true, ".cursor/sandbox.json": true, ".cursor/cli.json": true,
		".cursor/permissions.json": true, ".cursor/mcp.json": true,
	}
	archiveSlugBad = regexp.MustCompile(`[^a-z0-9-]+`)
)

// archiveSkipFile reports a file name that never goes into the archive.
func archiveSkipFile(name string) bool {
	switch {
	case name == ".env", strings.HasPrefix(name, ".env.") && name != ".env.example":
		return true
	case name == ".DS_Store", name == "Thumbs.db", strings.HasSuffix(name, ".log"), strings.HasSuffix(name, ".tsbuildinfo"):
		return true
	}
	return false
}

type archiveFile struct {
	rel  string // slash form
	abs  string
	info fs.FileInfo
}

type archiveError struct {
	status int
	msg    string
}

func (e *archiveError) Error() string { return e.msg }

// archiveFiles lists the regular files that go into the archive, sorted by path, within the limits.
func archiveFiles(root string) ([]archiveFile, int64, error) {
	var out []archiveFile
	var total int64
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			if abs == root {
				return err
			}
			return nil // unreadable entry: leave it out
		}
		if abs == root {
			return nil
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.SkipDir
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if archiveSkipDirs[d.Name()] || archiveSkipPaths[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || archiveSkipFile(d.Name()) || archiveSkipPaths[rel] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		out = append(out, archiveFile{rel: rel, abs: abs, info: info})
		if len(out) > archiveMaxFiles || total > archiveMaxBytes {
			return &archiveError{http.StatusRequestEntityTooLarge, "The project is too large for a ZIP download (limit " +
				strconv.Itoa(archiveMaxFiles) + " files, " + strconv.Itoa(archiveMaxBytes>>20) + " MiB)"}
		}
		return nil
	})
	return out, total, err
}

// archiveName is the download's file name: the project folder's slug.
func archiveName(path string) string {
	slug := strings.Trim(archiveSlugBad.ReplaceAllString(strings.ToLower(filepath.Base(path)), "-"), "-")
	if slug == "" {
		slug = "project"
	}
	return slug + ".zip"
}

func (h *Host) archive(w http.ResponseWriter, r *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return
	}
	root, err := filepath.EvalSymlinks(p.Path)
	if err != nil {
		WriteError(w, http.StatusNotFound, "The project folder is missing")
		return
	}
	files, total, err := archiveFiles(root)
	if err != nil {
		if ae, ok := err.(*archiveError); ok {
			WriteError(w, ae.status, ae.msg)
			return
		}
		WriteError(w, http.StatusInternalServerError, "Cannot read the project folder")
		return
	}
	name := archiveName(p.Path)
	// ?check=1: what a download would carry, as JSON (the Studio asks before a browser download).
	if r.URL.Query().Get("check") != "" {
		WriteJSON(w, http.StatusOK, map[string]any{"name": name, "files": len(files), "bytes": total})
		return
	}
	w.Header().Set("content-type", "application/zip")
	w.Header().Set("content-disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("cache-control", "no-store")
	w.Header().Set("x-content-type-options", "nosniff")
	w.WriteHeader(http.StatusOK)
	zw := zip.NewWriter(w)
	for _, f := range files {
		if err := addArchiveFile(zw, root, f); err != nil {
			// Headers are gone: stop here, the client sees a truncated (invalid) ZIP.
			h.opts.Log("archive " + p.ID + ": " + err.Error())
			return
		}
	}
	if err := zw.Close(); err != nil {
		h.opts.Log("archive " + p.ID + ": " + err.Error())
	}
}

// addArchiveFile streams one file; it refuses a file that turned into a symlink or another file since the walk.
func addArchiveFile(zw *zip.Writer, root string, f archiveFile) error {
	fh, err := os.Open(f.abs)
	if err != nil {
		return nil // gone since the walk: leave it out
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil || !st.Mode().IsRegular() || !os.SameFile(st, f.info) {
		return nil
	}
	if real, err := filepath.EvalSymlinks(f.abs); err != nil || !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return nil
	}
	hdr := &zip.FileHeader{Name: f.rel, Method: zip.Deflate, Modified: st.ModTime().UTC()}
	if hdr.Modified.IsZero() {
		hdr.Modified = time.Now().UTC()
	}
	hdr.SetMode(st.Mode().Perm())
	zw2, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(zw2, io.LimitReader(fh, st.Size()))
	return err
}
