package host

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// TestImportQuietRoomsPack: the ZIP produced by BuildY Chat packZipFiles (studio-fill / quiet-rooms) imports
// into an empty project, installs with bun --ignore-scripts, and passes verify (ui8kit check, lint:px, build).
func TestImportQuietRoomsPack(t *testing.T) {
	if os.Getenv("VIBECHAT_SKIP_IMPORT_PACK") == "1" {
		t.Skip("VIBECHAT_SKIP_IMPORT_PACK=1")
	}
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun is required to install and verify the imported pack")
	}
	zipPath := filepath.Join(testdataDir(t), "quiet-rooms-pack.zip")
	raw, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	svelte := starterProfile(t, "svelte")
	e := importEnv(t)
	e.host.opts.Templates = []templates.Profile{emptyTemplate, svelte, starterProfile(t, "react")}
	id, dir := mustEmptyProject(t, e, "quiet-rooms")
	rec := postZip(t, e.host, id, raw, "application/zip")
	if rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["framework"] != "svelte" {
		t.Fatalf("framework: %v", out)
	}
	verify, _ := out["verify"].(map[string]any)
	if verify["ok"] != true {
		t.Fatalf("verify failed: %s\n%s", verify["error"], verify["log"])
	}
	p, _ := e.host.opts.Store.Project(id)
	if p.TemplateID != "svelte" {
		t.Fatalf("template id %s", p.TemplateID)
	}
	if _, err := os.Stat(filepath.Join(dir, "src/App.svelte")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".cursor/rules/00-ui8kit-core.mdc")); err != nil {
		t.Fatal("contract rules not overlaid: ", err)
	}
}

func testdataDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(moduleRoot(t), "server", "internal", "host", "testdata")
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("go.mod not found")
	return ""
}

func starterProfile(t *testing.T, id string) templates.Profile {
	t.Helper()
	root := filepath.Join(moduleRoot(t), "server", "third_party", "templates", "templates", id)
	if _, err := os.Stat(filepath.Join(root, "package.json")); err != nil {
		t.Fatal(err)
	}
	return templates.Profile{
		ID: id, Name: id, Stack: id, Description: "starter", Enabled: true,
		Install: []string{"bun", "install", "--ignore-scripts"},
		Dev:     []string{"bun", "x", "vite", "--host", "127.0.0.1", "--port", "{port}", "--strictPort"},
		Build:   []string{"bun", "run", "build"},
		Files:   os.DirFS(root),
	}
}

// Silence unused in case fs is needed for future assertions.
var _ fs.FS
var _ = strings.TrimSpace
