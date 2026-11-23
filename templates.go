package vibechat

import (
	"embed"
	"io/fs"

	"github.com/buildytech/vibechat-desktop/internal/templates"
)

//go:embed all:third_party/vibecode/templates/vite-svelte-bun
var viteSvelteBun embed.FS

//go:embed all:third_party/vibecode/templates/vite-react-bun
var viteReactBun embed.FS

func sub(f embed.FS, dir string) fs.FS {
	s, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return s
}

// Templates are the project templates of the desktop app, in picker order. Dependencies are installed by the host
// (outside the agent sandbox), the dev server binds 127.0.0.1 on a host-chosen port and its HMR client talks to the
// preview proxy port.
func Templates() []templates.Profile {
	viteDev := []string{"bun", "x", "vite", "--host", "127.0.0.1", "--port", "{port}", "--strictPort"}
	viteEnv := map[string]string{"HMR_CLIENT_PORT": "{hmrPort}", "HMR_PROTOCOL": "ws", "BROWSER": "none"}
	return []templates.Profile{
		{
			ID: "vite-svelte-bun", Name: "Landing: Svelte 5 + UI8Kit (Bun)", Stack: "Vite 8 · Svelte 5 · Tailwind 4 · UI8Kit · Bun",
			Description: "Landing page from UI8Kit blocks: page contract in JSON, ui8px 8+4 gate, dark mode.", Enabled: true,
			Files:   sub(viteSvelteBun, "third_party/vibecode/templates/vite-svelte-bun"),
			Install: []string{"bun", "install"}, Dev: viteDev, DevEnv: viteEnv, Build: []string{"bun", "run", "build"},
		},
		{
			ID: "vite-react-bun", Name: "Vite + React (Bun)", Stack: "Vite · React 19 · Tailwind 4 · Bun",
			Description: "React single-page app on Vite with Tailwind 4 and UI8Kit rules.", Enabled: true,
			Files:   sub(viteReactBun, "third_party/vibecode/templates/vite-react-bun"),
			Install: []string{"bun", "install"}, Dev: viteDev, DevEnv: viteEnv, Build: []string{"bun", "run", "build"},
		},
	}
}
