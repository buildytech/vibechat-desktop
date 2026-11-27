package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"

	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// The pinned buildytech/templates: templates.json lists the templates (picker order, the first is the default) and
// templates/<id> holds each one. A template whose runner this build knows needs no code here, only a pin bump.
//
//go:embed third_party/templates/templates.json
var templatesManifest []byte

//go:embed all:third_party/templates/templates
var templatesFiles embed.FS

// manifest is templates.json (schemaVersion 1).
type manifest struct {
	SchemaVersion int `json:"schemaVersion"`
	Templates     []struct {
		ID          string   `json:"id"`
		Dir         string   `json:"dir"`
		Name        string   `json:"name"`
		Stack       string   `json:"stack"`
		Description string   `json:"description"`
		Runner      string   `json:"runner"`
		Aliases     []string `json:"aliases"`
	} `json:"templates"`
}

// runners are the run profiles this build knows. Dependencies are installed by the host, isolated from the server
// (package isolate) and without lifecycle scripts (--ignore-scripts: package.json is code the agent can write); the
// dev server, isolated the same way, binds 127.0.0.1 on a host-chosen port and its HMR client talks to the preview proxy port.
func runners() map[string]templates.Profile {
	viteDev := []string{"bun", "x", "vite", "--host", "127.0.0.1", "--port", "{port}", "--strictPort"}
	viteEnv := map[string]string{"HMR_CLIENT_PORT": "{hmrPort}", "HMR_PROTOCOL": "ws", "BROWSER": "none"}
	goCache := isolate.GoCache()
	var goEnv, goWritable []string
	if goCache != "" {
		goEnv, goWritable = isolate.GoEnv(goCache), []string{goCache}
	}
	return map[string]templates.Profile{
		"vite-bun": {Runner: "vite-bun", Tool: "bun", Install: []string{"bun", "install", "--ignore-scripts"}, Dev: viteDev, DevEnv: viteEnv, Build: []string{"bun", "run", "build"}},
		// go-templ (ADR 0005): the modules come from the Go proxy into the shared cache; the dev loop is the server's
		// own `go-dev` (templ generate + go build, one build at a time, then the site on {port}; rebuilt on change).
		"go-templ": {
			Runner: "go-templ", Tool: "go", Env: goEnv, Writable: goWritable,
			Install: []string{"go", "mod", "download"},
			Dev:     []string{"{self}", "go-dev", "--port", "{port}"},
			Build:   []string{"go", "build", "-o", ".vibechat/bin/site", "./cmd/site"},
		},
		// none: a blank workspace (no project files). Enabled, but install and the dev server do not run.
		"none": {Runner: "none"},
	}
}

func sub(f fs.FS, dir string) fs.FS {
	s, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return s
}

// Templates are the project templates of this build, in picker order. A template with a runner this build does not
// know is listed but disabled (the picker shows it as coming soon). A broken manifest is a build error (tests).
func Templates() []templates.Profile {
	ts, err := templatesFrom(templatesManifest, templatesFiles, "third_party/templates")
	if err != nil {
		panic(err)
	}
	return ts
}

func templatesFrom(raw []byte, files fs.FS, root string) ([]templates.Profile, error) {
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("templates.json: %w", err)
	}
	if m.SchemaVersion != 1 {
		return nil, fmt.Errorf("templates.json: schemaVersion %d (this build reads 1)", m.SchemaVersion)
	}
	run := runners()
	seen := map[string]bool{}
	out := make([]templates.Profile, 0, len(m.Templates))
	for _, t := range m.Templates {
		for _, id := range append([]string{t.ID}, t.Aliases...) {
			if id == "" || seen[id] {
				return nil, fmt.Errorf("templates.json: id or alias %q empty or used twice", id)
			}
			seen[id] = true
		}
		p := templates.Profile{ID: t.ID, Name: t.Name, Stack: t.Stack, Description: t.Description, Aliases: t.Aliases, Files: sub(files, root+"/"+t.Dir)}
		_, pkgErr := fs.Stat(files, root+"/"+t.Dir+"/package.json")
		switch t.Runner {
		case "none":
			if pkgErr == nil {
				return nil, fmt.Errorf("templates.json: %s: runner none must not ship package.json", t.ID)
			}
		case "go-templ":
			// A layer over the empty workspace (templates ADR 0004): layer.json pins the tree, fetched on first use.
			raw, err := fs.ReadFile(files, root+"/"+t.Dir+"/layer.json")
			if err != nil {
				return nil, fmt.Errorf("templates.json: %s: %s/layer.json missing", t.ID, t.Dir)
			}
			l, err := templates.ParseLayer(raw)
			if err != nil {
				return nil, fmt.Errorf("templates.json: %s: %w", t.ID, err)
			}
			if _, ok := l.Files["go.mod"]; !ok {
				return nil, fmt.Errorf("templates.json: %s: layer.json has no go.mod", t.ID)
			}
			p.Layer, p.Files = l, templates.LayerFS(l)
		default:
			if pkgErr != nil {
				return nil, fmt.Errorf("templates.json: %s: %s/package.json missing", t.ID, t.Dir)
			}
		}
		if r, ok := run[t.Runner]; ok {
			p.Enabled = true
			p.Runner, p.Tool, p.Env, p.Writable = r.Runner, r.Tool, r.Env, r.Writable
			p.Install, p.Dev, p.DevEnv, p.Build = r.Install, r.Dev, r.DevEnv, r.Build
		}
		out = append(out, p)
	}
	return out, nil
}
