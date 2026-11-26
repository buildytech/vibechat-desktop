package server

import (
	"io/fs"
	"testing"
)

func TestTemplatesEmbedded(t *testing.T) {
	ts := Templates()
	if len(ts) == 0 || ts[0].ID != "vite-svelte-bun" {
		t.Fatalf("the Svelte landing must be the first (default) template: %+v", ts)
	}
	for _, p := range ts {
		for _, f := range []string{"package.json", "bun.lock", "AGENTS.md", "vite.config.ts"} {
			if _, err := fs.Stat(p.Files, f); err != nil {
				t.Errorf("%s: %s missing", p.ID, f)
			}
		}
		if len(p.Install) == 0 || len(p.Dev) == 0 || len(p.Build) == 0 {
			t.Errorf("%s: incomplete profile", p.ID)
		}
		if _, err := fs.Stat(p.Files, "node_modules"); err == nil {
			t.Errorf("%s: node_modules embedded", p.ID)
		}
	}
	svelte := ts[0].Files
	for _, f := range []string{"src/content/page.json", "contracts/blocks.json", ".cursor/rules/00-ui8kit-core.mdc", ".ui8px/gate.json", "ui8kit.lock.json"} {
		if _, err := fs.Stat(svelte, f); err != nil {
			t.Errorf("vite-svelte-bun: %s missing", f)
		}
	}
	for _, f := range []string{".cursor/hooks.json", ".cursor/sandbox.json", ".cursor/mcp.json"} {
		if _, err := fs.Stat(svelte, f); err == nil {
			t.Errorf("vite-svelte-bun ships agent config %s", f)
		}
	}
}
