package server

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func TestTemplatesEmbedded(t *testing.T) {
	ts := Templates()
	if len(ts) < 3 || ts[0].ID != "svelte" {
		t.Fatalf("the Svelte starter must be the first (default) template: %+v", ts)
	}
	ids := map[string]bool{}
	for _, p := range ts {
		ids[p.ID] = true
		if p.ID == "empty" {
			if !p.Enabled || len(p.Install) != 0 || len(p.Dev) != 0 || len(p.Build) != 0 {
				t.Errorf("empty: want an enabled blank profile, got %+v", p)
			}
			for _, f := range []string{"package.json", "index.html", "vite.config.ts", ".gitkeep"} {
				if _, err := fs.Stat(p.Files, f); err == nil && f != ".gitkeep" {
					t.Errorf("empty: %s should not be copied into a project (present in the embed: %v)", f, err)
				}
			}
			continue
		}
		if p.Layer != nil {
			// The Go starter (templates ADR 0004): a pinned layer, fetched on first use; the same commit as BuildY
			// Chat's registry/templ snapshot.
			if p.ID != "templ" || p.Runner != "go-templ" || p.Tool != "go" || !p.Enabled || p.Layer.Source.Repo != "fastygo/starter" ||
				p.Layer.Source.Commit != "cf9d7dbf944c2fc395625e19d7cd3a104d0e667c" || p.Layer.Files["go.sum"] == "" || p.Layer.Files["cmd/site/main.go"] == "" {
				t.Errorf("templ: unexpected layer profile %+v", p)
			}
			continue
		}
		for _, f := range []string{"package.json", "bun.lock", "AGENTS.md", "vite.config.ts", "index.html", "LICENSE"} {
			if _, err := fs.Stat(p.Files, f); err != nil {
				t.Errorf("%s: %s missing", p.ID, f)
			}
		}
		if !p.Enabled || len(p.Install) == 0 || len(p.Dev) == 0 || len(p.Build) == 0 {
			t.Errorf("%s: incomplete profile", p.ID)
		}
		for _, f := range []string{"node_modules", "dist"} {
			if _, err := fs.Stat(p.Files, f); err == nil {
				t.Errorf("%s: %s embedded", p.ID, f)
			}
		}
		for _, f := range []string{".cursor/hooks.json", ".cursor/sandbox.json", ".cursor/mcp.json"} {
			if _, err := fs.Stat(p.Files, f); err == nil {
				t.Errorf("%s ships agent config %s", p.ID, f)
			}
		}
	}
	for _, id := range []string{"svelte", "react", "templ", "empty"} {
		if !ids[id] {
			t.Errorf("template %s missing", id)
		}
	}
	for _, p := range ts[:2] {
		for _, f := range []string{".cursor/rules/00-ui8kit-core.mdc", ".cursor/skills/ui8kit-new-block/SKILL.md", ".ui8px/gate.json", "ui8kit.lock.json",
			"src/fixtures/home.json", "src/i18n/en.json", "src/assets/fonts/istok-web/OFL.txt", "public/favicon.svg", "public/favicon.ico"} {
			if _, err := fs.Stat(p.Files, f); err != nil {
				t.Errorf("%s: %s missing", p.ID, f)
			}
		}
	}
	// Projects made from the former ids keep their profile.
	for alias, id := range map[string]string{"vite-svelte-bun": "svelte", "vite-react-bun": "react"} {
		found := false
		for _, p := range ts {
			if p.Is(alias) {
				found = p.ID == id
			}
		}
		if !found {
			t.Errorf("alias %s does not resolve to %s", alias, id)
		}
	}
}

func TestTemplatesManifest(t *testing.T) {
	files := fstest.MapFS{
		"r/templates/a/package.json": {Data: []byte("{}")},
		"r/templates/b/package.json": {Data: []byte("{}")},
	}
	ok := `{"schemaVersion":1,"templates":[{"id":"a","dir":"templates/a","name":"A","runner":"vite-bun","aliases":["old-a"]},{"id":"b","dir":"templates/b","name":"B","runner":"next-pnpm"},{"id":"empty","dir":"templates/empty","name":"Empty","runner":"none"}]}`
	files["r/templates/empty/.gitkeep"] = &fstest.MapFile{Data: []byte{}}
	files["r/templates/h/layer.json"] = &fstest.MapFile{Data: []byte(`{"schemaVersion":1,"base":"svelte"}`)}
	files["r/templates/g2/layer.json"] = &fstest.MapFile{Data: []byte(`{"schemaVersion":1,"base":"empty","source":{"kind":"github-archive","repo":"o/r","commit":"` + strings.Repeat("a", 40) + `"},"files":{"go.mod":"` + strings.Repeat("b", 64) + `"}}`)}
	g, err := templatesFrom([]byte(`{"schemaVersion":1,"templates":[{"id":"g2","dir":"templates/g2","runner":"go-templ"}]}`), files, "r")
	if err != nil || len(g) != 1 || g[0].Layer == nil || !g[0].Enabled || g[0].Tool != "go" || g[0].Dev[0] != "{self}" {
		t.Fatalf("go-templ layer profile: %+v %v", g, err)
	}
	ts, err := templatesFrom([]byte(ok), files, "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 3 || !ts[0].Enabled || !ts[0].Is("old-a") || ts[1].Enabled || len(ts[1].Dev) != 0 || !ts[2].Enabled || len(ts[2].Install) != 0 || len(ts[2].Dev) != 0 {
		t.Fatalf("profiles: %+v", ts)
	}
	for name, bad := range map[string]string{
		"schema":    `{"schemaVersion":2,"templates":[]}`,
		"duplicate": `{"schemaVersion":1,"templates":[{"id":"a","dir":"templates/a","runner":"vite-bun"},{"id":"b","dir":"templates/b","aliases":["a"]}]}`,
		"missing":   `{"schemaVersion":1,"templates":[{"id":"c","dir":"templates/c","runner":"vite-bun"}]}`,
		"json":      `{`,
		"no layer":  `{"schemaVersion":1,"templates":[{"id":"g","dir":"templates/g","runner":"go-templ"}]}`,
		"bad layer": `{"schemaVersion":1,"templates":[{"id":"h","dir":"templates/h","runner":"go-templ"}]}`,
	} {
		if _, err := templatesFrom([]byte(bad), files, "r"); err == nil || !strings.Contains(err.Error(), "templates.json") {
			t.Errorf("%s: want a templates.json error, got %v", name, err)
		}
	}
}
