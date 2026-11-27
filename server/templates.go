package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"

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
	return map[string]templates.Profile{
		"vite-bun": {Install: []string{"bun", "install", "--ignore-scripts"}, Dev: viteDev, DevEnv: viteEnv, Build: []string{"bun", "run", "build"}},
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
		if _, err := fs.Stat(files, root+"/"+t.Dir+"/package.json"); err != nil {
			return nil, fmt.Errorf("templates.json: %s: %s/package.json missing", t.ID, t.Dir)
		}
		p := templates.Profile{ID: t.ID, Name: t.Name, Stack: t.Stack, Description: t.Description, Aliases: t.Aliases, Files: sub(files, root+"/"+t.Dir)}
		if r, ok := run[t.Runner]; ok {
			p.Enabled, p.Install, p.Dev, p.DevEnv, p.Build = true, r.Install, r.Dev, r.DevEnv, r.Build
		}
		out = append(out, p)
	}
	return out, nil
}
