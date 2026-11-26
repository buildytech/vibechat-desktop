package host

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func getArchive(t *testing.T, h http.Handler, id, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/runtime/v1/projects/"+id+"/archive.zip"+query, nil))
	return rec
}

func zipEntries(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

// TestProjectArchive: the ZIP carries the project's own files under their relative paths and leaves out git,
// packages, build output and caches, local env files, logs and the agent's tool config; symlinks (in or out of the
// project) are never followed or stored.
func TestProjectArchive(t *testing.T) {
	e := newEnv(t, pinnedSeam(t), false)
	p := mustProject(t, e, "Мой лендинг")
	id := p["id"].(string)
	sp, err := e.host.opts.Store.Project(id)
	if err != nil {
		t.Fatal(err)
	}
	root := sp.Path
	writeTree(t, root, map[string]string{
		"src/App.svelte":                 "<h1>app</h1>",
		".env.example":                   "API_URL=",
		".cursor/rules/ui.mdc":           "rule",
		".buildy/brief.json":             "{}",
		".env":                           "SECRET=1",
		".env.local":                     "SECRET=2",
		"node_modules/x/index.js":        "x",
		"dist/index.html":                "built",
		".vite/deps/a.js":                "cache",
		"src/.cache/b":                   "cache",
		"debug.log":                      "log",
		".DS_Store":                      "mac",
		".cursor/hooks.json":             "{}",
		".cursor/mcp.json":               "{}",
		".ui8px/telemetry/t.json":        "{}",
		"packages/web/node_modules/y.js": "y",
		"packages/web/src/main.ts":       "main",
	})
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"secret.txt": "outside", "dir/inner.txt": "outside"})
	if runtime.GOOS != "windows" {
		for link, target := range map[string]string{
			"leak.txt":        filepath.Join(outside, "secret.txt"),
			"leakdir":         filepath.Join(outside, "dir"),
			"src/inside-link": filepath.Join(root, "src", "App.svelte"),
			"src/up":          "../..",
		} {
			if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(link))); err != nil {
				t.Fatal(err)
			}
		}
	}

	rec := getArchive(t, e.host, id, "")
	if rec.Code != 200 || rec.Header().Get("content-type") != "application/zip" {
		t.Fatalf("%d %s %s", rec.Code, rec.Header().Get("content-type"), rec.Body.String())
	}
	if cd := rec.Header().Get("content-disposition"); cd != `attachment; filename="moy-lending.zip"` {
		t.Fatalf("content-disposition %q", cd)
	}
	got := zipEntries(t, rec.Body.Bytes())
	var names []string
	for n := range got {
		names = append(names, n)
		if strings.Contains(got[n], "outside") || strings.Contains(got[n], "SECRET") {
			t.Errorf("%s leaks %q", n, got[n])
		}
		if strings.HasPrefix(n, "/") || strings.Contains(n, "..") || strings.Contains(n, `\`) {
			t.Errorf("unsafe entry name %q", n)
		}
	}
	sort.Strings(names)
	for _, want := range []string{"index.html", "package.json", "AGENTS.md", "src/App.svelte", ".env.example", ".cursor/rules/ui.mdc", ".buildy/brief.json", "packages/web/src/main.ts"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s in %v", want, names)
		}
	}
	for _, n := range names {
		for _, bad := range []string{".git/", "node_modules/", "dist/", ".vite/", ".cache/", ".ui8px/telemetry/", "leak", "inside-link", "src/up"} {
			if strings.HasPrefix(n, bad) || strings.Contains(n, "/"+bad) || strings.HasPrefix(n, "src/"+bad) {
				t.Errorf("%s should be left out", n)
			}
		}
		switch n {
		case ".env", ".env.local", "debug.log", ".DS_Store", ".cursor/hooks.json", ".cursor/mcp.json":
			t.Errorf("%s should be left out", n)
		}
	}
	if got["src/App.svelte"] != "<h1>app</h1>" {
		t.Errorf("content %q", got["src/App.svelte"])
	}

	// ?check=1: JSON with the same name and count.
	rec = getArchive(t, e.host, id, "?check=1")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"moy-lending.zip"`) || !strings.Contains(rec.Body.String(), `"files":`+strconv.Itoa(len(got))) {
		t.Fatalf("check: %d %s (zip has %d)", rec.Code, rec.Body.String(), len(got))
	}
	// Unknown project: 404 { error }.
	if rec := getArchive(t, e.host, "nope", ""); rec.Code != 404 || !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("unknown project: %d %s", rec.Code, rec.Body.String())
	}
}

// TestProjectArchiveLimit: a project over the size limit answers 413 { error } before any ZIP byte.
func TestProjectArchiveLimit(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a"})
	f, err := os.Create(filepath.Join(root, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(archiveMaxBytes + 1); err != nil { // sparse: no real disk use
		t.Fatal(err)
	}
	f.Close()
	_, _, err = archiveFiles(root)
	ae, ok := err.(*archiveError)
	if !ok || ae.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("err %v", err)
	}
}

func TestArchiveName(t *testing.T) {
	for in, want := range map[string]string{"/p/moy-lending": "moy-lending.zip", "/p/My Site": "my-site.zip", "/p/..": "project.zip", "/p/a\"b": "a-b.zip"} {
		if got := archiveName(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
}
