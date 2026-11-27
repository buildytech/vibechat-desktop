package host

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

func importEnv(t *testing.T) testEnv {
	t.Helper()
	e := newEnv(t, pinnedSeam(t), false)
	e.host.opts.Templates = []templates.Profile{emptyTemplate, svelteImportTemplate, reactImportTemplate}
	return e
}

var emptyTemplate = templates.Profile{
	ID: "empty", Name: "Empty", Stack: "Blank workspace", Description: "no files", Enabled: true,
	Files: fstest.MapFS{".gitkeep": {Data: []byte{}}},
}

var svelteImportTemplate = templates.Profile{
	ID: "svelte", Name: "Svelte starter", Stack: "Svelte", Description: "test", Enabled: true,
	Files: fstest.MapFS{
		"package.json":         {Data: []byte(`{"devDependencies":{"typescript":"~5.9.3","ui8kit":"3.5.0","ui8px":"0.3.0","svelte-check":"^4.7.8","@tsconfig/svelte":"^5.0.9"}}`)},
		"tsconfig.json":        {Data: []byte(`{"compilerOptions":{"strict":true}}`)},
		"AGENTS.md":            {Data: []byte("# test\n")},
		".bun-version":         {Data: []byte("1.3.3\n")},
		".cursor/rules/00.mdc": {Data: []byte("rule\n")},
	},
}

var reactImportTemplate = templates.Profile{
	ID: "react", Name: "React starter", Stack: "React", Description: "test", Enabled: true,
	Files: fstest.MapFS{
		"package.json":  {Data: []byte(`{"devDependencies":{"typescript":"~5.9.3","ui8kit":"3.5.0","ui8px":"0.3.0","@types/react":"^19.0.0"}}`)},
		"tsconfig.json": {Data: []byte(`{"compilerOptions":{"strict":true}}`)},
		"AGENTS.md":     {Data: []byte("# test\n")},
	},
}

func mustEmptyProject(t *testing.T, e testEnv, name string) (id, dir string) {
	t.Helper()
	code, out := call(t, e.host, "POST", "/projects", map[string]any{"name": name, "templateId": "empty"})
	if code != http.StatusCreated {
		t.Fatalf("create empty: %d %v", code, out)
	}
	p := out["project"].(map[string]any)
	id = p["id"].(string)
	sp, err := e.host.opts.Store.Project(id)
	if err != nil {
		t.Fatal(err)
	}
	return id, sp.Path
}

func packZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func validSvelteFiles() map[string]string {
	return map[string]string{
		"ui8kit.config.json": `{
  "contractVersion": "2.0.0",
  "framework": "svelte",
  "runtime": "svelte",
  "dir": "src"
}`,
		"ui8kit.lock.json": `{"lockVersion":2,"cli":"3.5.0","items":{}}`,
		"index.html":       `<!doctype html><html><body><div id="app"></div><script type="module" src="/src/main.ts"></script></body></html>`,
		"vite.config.ts":   `import { defineConfig } from "vite"; export default defineConfig({});`,
		"package.json": `{
  "name": "sample",
  "private": true,
  "type": "module",
  "scripts": { "dev": "vite", "build": "vite build", "preview": "vite preview" },
  "dependencies": { "svelte": "5.57.3", "clsx": "2.1.1", "lucide": "1.72.0", "tailwind-merge": "3.7.0" },
  "devDependencies": { "vite": "7.3.7", "@sveltejs/vite-plugin-svelte": "6.2.4", "@tailwindcss/vite": "4.3.4", "tailwindcss": "4.3.4" }
}`,
		"src/main.ts":          `import { mount } from "svelte"; import App from "./App.svelte"; mount(App, { target: document.getElementById("app")! });`,
		"src/App.svelte":       `<script lang="ts"></script>\n`,
		"src/styles/index.css": `/* tokens */\n`,
	}
}

func validReactFiles() map[string]string {
	m := validSvelteFiles()
	m["ui8kit.config.json"] = `{
  "contractVersion": "2.0.0",
  "framework": "react",
  "runtime": "react",
  "dir": "src"
}`
	m["package.json"] = `{
  "name": "sample-react",
  "private": true,
  "type": "module",
  "scripts": { "dev": "vite", "build": "vite build" },
  "dependencies": { "react": "19.3.1", "react-dom": "19.3.1", "clsx": "2.1.1", "lucide": "1.81.0", "tailwind-merge": "3.7.1" },
  "devDependencies": { "vite": "8.4.0", "@vitejs/plugin-react": "5.2.0", "@tailwindcss/vite": "4.3.5", "tailwindcss": "4.3.5" }
}`
	delete(m, "src/App.svelte")
	delete(m, "src/main.ts")
	m["src/App.tsx"] = `export default function App() { return null }`
	m["src/main.tsx"] = `import { createRoot } from "react-dom/client"; import App from "./App"; createRoot(document.getElementById("app")!).render(<App />);`
	return m
}

func postZip(t *testing.T, h http.Handler, id string, raw []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/runtime/v1/projects/"+id+"/import.zip", bytes.NewReader(raw))
	if contentType != "" {
		req.Header.Set("content-type", contentType)
	} else {
		req.Header.Set("content-type", "application/zip")
	}
	h.ServeHTTP(rec, req)
	return rec
}

func TestImportZipSvelteAndReact(t *testing.T) {
	e := importEnv(t)
	id, dir := mustEmptyProject(t, e, "Импорт svelte")
	rec := postZip(t, e.host, id, packZip(t, validSvelteFiles()), "")
	if rec.Code != 200 {
		t.Fatalf("svelte import: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["framework"] != "svelte" {
		t.Fatalf("framework: %v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "src/App.svelte")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Fatal("tooling overlay missing: ", err)
	}
	p, _ := e.host.opts.Store.Project(id)
	if p.TemplateID != "svelte" {
		t.Fatalf("template id %s", p.TemplateID)
	}

	id2, dir2 := mustEmptyProject(t, e, "Импорт react")
	rec = postZip(t, e.host, id2, packZip(t, validReactFiles()), "")
	if rec.Code != 200 {
		t.Fatalf("react import: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir2, "src/App.tsx")); err != nil {
		t.Fatal(err)
	}
	p, _ = e.host.opts.Store.Project(id2)
	if p.TemplateID != "react" {
		t.Fatalf("template id %s", p.TemplateID)
	}
}

func TestImportZipRejectsNonEmpty(t *testing.T) {
	e := importEnv(t)
	id, dir := mustEmptyProject(t, e, "Не пустой")
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := postZip(t, e.host, id, packZip(t, validSvelteFiles()), "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "empty") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestImportZipRejectsZipSlipAndSymlink(t *testing.T) {
	e := importEnv(t)
	id, _ := mustEmptyProject(t, e, "slip")
	rec := postZip(t, e.host, id, packZip(t, map[string]string{"../escape.txt": "no"}), "")
	if rec.Code != 400 {
		t.Fatalf("slip: %d %s", rec.Code, rec.Body.String())
	}

	id, dir := mustEmptyProject(t, e, "symlink")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "link", Method: zip.Store}
	h.SetMode(os.ModeSymlink | 0o644)
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "/etc/passwd"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	rec = postZip(t, e.host, id, buf.Bytes(), "")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "symbolic") {
		t.Fatalf("symlink: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(dir, "link")); err == nil {
		t.Fatal("symlink was written")
	}
}

func TestImportZipRejectsPostinstallAndUnknownPackage(t *testing.T) {
	e := importEnv(t)
	id, _ := mustEmptyProject(t, e, "postinstall")
	files := validSvelteFiles()
	files["package.json"] = `{
  "name": "bad",
  "type": "module",
  "scripts": { "dev": "vite", "build": "vite build", "postinstall": "curl evil" },
  "dependencies": { "svelte": "5.57.3" },
  "devDependencies": { "vite": "7.3.7" }
}`
	rec := postZip(t, e.host, id, packZip(t, files), "")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "postinstall") {
		t.Fatalf("postinstall: %d %s", rec.Code, rec.Body.String())
	}

	id, _ = mustEmptyProject(t, e, "чужой пакет")
	files = validSvelteFiles()
	files["package.json"] = `{
  "name": "bad",
  "type": "module",
  "scripts": { "dev": "vite", "build": "vite build" },
  "dependencies": { "svelte": "5.57.3", "left-pad": "1.0.0" },
  "devDependencies": { "vite": "7.3.7" }
}`
	rec = postZip(t, e.host, id, packZip(t, files), "")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "left-pad") {
		t.Fatalf("unknown pkg: %d %s", rec.Code, rec.Body.String())
	}
}

func TestImportZipRejectsChatExport(t *testing.T) {
	e := importEnv(t)
	id, _ := mustEmptyProject(t, e, "chat export")
	raw := []byte(`{"format":"buildy.chat.v1","exportedAt":"2026-11-27T08:20:55Z","messages":[]}`)
	// A lone JSON file in a zip, and also a raw JSON body.
	rec := postZip(t, e.host, id, packZip(t, map[string]string{"chat.json": string(raw)}), "")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "chat export") {
		t.Fatalf("zipped json: %d %s", rec.Code, rec.Body.String())
	}
	rec = postZip(t, e.host, id, raw, "application/json")
	if rec.Code != 400 {
		t.Fatalf("raw json: %d %s", rec.Code, rec.Body.String())
	}
}

func TestImportZipUnwrapsRootFolder(t *testing.T) {
	e := importEnv(t)
	id, dir := mustEmptyProject(t, e, "wrapped")
	wrapped := map[string]string{}
	for k, v := range validSvelteFiles() {
		wrapped["page/"+k] = v
	}
	rec := postZip(t, e.host, id, packZip(t, wrapped), "")
	if rec.Code != 200 {
		t.Fatalf("wrapped: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "src/App.svelte")); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyProjectHasNoFiles(t *testing.T) {
	e := importEnv(t)
	id, dir := mustEmptyProject(t, e, "Пустой")
	code, out := call(t, e.host, "GET", "/projects/"+id+"/files", nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	tree, _ := json.Marshal(out["tree"])
	if strings.Contains(string(tree), `"type":"file"`) {
		t.Fatalf("empty project tree has files: %s", tree)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ".git" {
			t.Fatalf("unexpected entry %s", e.Name())
		}
	}
}
