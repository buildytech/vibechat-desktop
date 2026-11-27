package host

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// The Vite adapters (svelte, react): a UI8Kit project as `ui8kit init` writes it, or a BuildY Chat Svelte pack.
// Core of the framework = the official starter (tooling, tsconfig, the contract rules and gates, the Vite config with
// the preview HMR settings), filled in where the ZIP has none; data = src/styles, src/assets, public, page.json and
// the pack's metadata; the rest of src/** is page code.

type viteAdapter struct{ framework string }

func (a viteAdapter) ID() string     { return a.framework }
func (a viteAdapter) Runner() string { return "vite-bun" }

// Detect (a ZIP without a buildy.pack.v1 manifest, as Chat wrote before 0.2.8 and `ui8kit init` writes):
// ui8kit.config.json at the root names the framework.
func (a viteAdapter) Detect(in *importInput) bool {
	return configFramework(in.by) == a.framework
}

func (a viteAdapter) Validate(_ context.Context, in *importInput, _ templates.Profile) error {
	fw, err := validateVite(in.by)
	if err != nil {
		return err
	}
	if fw != a.framework {
		return importFail(http.StatusBadRequest, "manifest.json framework "+a.framework+" does not match ui8kit.config.json framework "+fw)
	}
	return nil
}

func (a viteAdapter) Fill(staging string, _ *importInput, tpl templates.Profile) error {
	if err := overlayTooling(staging, a.framework, tpl); err != nil {
		return err
	}
	if err := mergePackageJSON(staging, a.framework, tpl); err != nil {
		return err
	}
	return ensureViteHMR(staging, a.framework)
}

func (a viteAdapter) Verify(ctx context.Context, dir string, _ templates.Profile, log func(string)) (string, error) {
	return verifyImported(ctx, dir, log)
}

func (a viteAdapter) DataPaths() []string {
	return []string{"src/styles/", "src/assets/", "public/", "page.json", "brand.contract.json", "manifest.json", "GENERATOR.json"}
}

// configFramework is the framework of a root ui8kit.config.json ("" when there is none or it is not JSON).
func configFramework(by map[string][]byte) string {
	raw, ok := by["ui8kit.config.json"]
	if !ok {
		return ""
	}
	var cfg struct {
		Framework string `json:"framework"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	return strings.TrimSpace(cfg.Framework)
}

var (
	importForbiddenScripts = map[string]bool{
		"preinstall": true, "install": true, "postinstall": true, "prepare": true,
		"prepublish": true, "prepublishOnly": true, "prepack": true, "postpack": true,
	}
	importCommonPkgs = map[string]bool{
		"clsx": true, "lucide": true, "tailwind-merge": true, "vite": true,
		"@tailwindcss/vite": true, "tailwindcss": true, "typescript": true,
		"@types/bun": true, "ui8kit": true, "ui8px": true,
	}
	importSveltePkgs = map[string]bool{
		"svelte": true, "@sveltejs/vite-plugin-svelte": true, "svelte-check": true, "@tsconfig/svelte": true,
	}
	importReactPkgs = map[string]bool{
		"react": true, "react-dom": true, "@vitejs/plugin-react": true, "@types/react": true, "@types/react-dom": true,
	}
	importOverlayRoots = []string{".cursor", ".ui8px", "scripts"}
	importOverlayFiles = []string{"AGENTS.md", "tsconfig.json", "svelte.config.js", ".bun-version"}
)

// validateVite is the 0.2.7 check of a UI8Kit Vite project (unchanged): ui8kit.config.json, the entry files of the
// framework, and package.json against the allowlists. It returns the framework of ui8kit.config.json.
func validateVite(by map[string][]byte) (framework string, err error) {
	cfgRaw, ok := by["ui8kit.config.json"]
	if !ok {
		return "", importFail(http.StatusBadRequest, "ui8kit.config.json is required")
	}
	var cfg struct {
		Framework       string `json:"framework"`
		Runtime         string `json:"runtime"`
		ContractVersion string `json:"contractVersion"`
		Dir             string `json:"dir"`
	}
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		return "", importFail(http.StatusBadRequest, "ui8kit.config.json is not valid JSON")
	}
	fw := strings.TrimSpace(cfg.Framework)
	if fw != "svelte" && fw != "react" {
		return "", importFail(http.StatusBadRequest, "The import must be a Svelte or React UI8Kit project (ui8kit.config.json framework)")
	}
	if rt := strings.TrimSpace(cfg.Runtime); rt != "" && rt != fw {
		return "", importFail(http.StatusBadRequest, "ui8kit.config.json runtime must match framework")
	}
	if cfg.ContractVersion != "" && !strings.HasPrefix(cfg.ContractVersion, "2.") {
		return "", importFail(http.StatusBadRequest, "ui8kit.config.json contractVersion must be 2.x")
	}
	if d := strings.TrimSpace(cfg.Dir); d != "" && d != "src" {
		return "", importFail(http.StatusBadRequest, "ui8kit.config.json dir must be src")
	}
	if _, ok := by["ui8kit.lock.json"]; !ok {
		return "", importFail(http.StatusBadRequest, "ui8kit.lock.json is required")
	}
	if _, ok := by["index.html"]; !ok {
		return "", importFail(http.StatusBadRequest, "index.html is required")
	}
	if _, ok := by["vite.config.ts"]; !ok {
		if _, ok := by["vite.config.js"]; !ok {
			return "", importFail(http.StatusBadRequest, "vite.config.ts is required")
		}
	}
	if _, ok := by["src/styles/index.css"]; !ok {
		return "", importFail(http.StatusBadRequest, "src/styles/index.css is required")
	}
	hasSvelte := hasKey(by, "src/App.svelte") && hasKey(by, "src/main.ts")
	hasReact := hasKey(by, "src/App.tsx") && hasKey(by, "src/main.tsx")
	if hasSvelte && hasReact {
		return "", importFail(http.StatusBadRequest, "The ZIP mixes Svelte and React entry files")
	}
	if fw == "svelte" && !hasSvelte {
		return "", importFail(http.StatusBadRequest, "A Svelte import needs src/App.svelte and src/main.ts")
	}
	if fw == "react" && !hasReact {
		return "", importFail(http.StatusBadRequest, "A React import needs src/App.tsx and src/main.tsx")
	}
	if err := validatePackageJSON(by["package.json"], fw); err != nil {
		return "", err
	}
	if raw, ok := by["GENERATOR.json"]; ok {
		var g struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &g) == nil && g.Name != "" && fw == "svelte" && !strings.Contains(g.Name, "svelte") {
			return "", importFail(http.StatusBadRequest, "GENERATOR.json does not match a Svelte pack")
		}
	}
	return fw, nil
}

func validatePackageJSON(raw []byte, framework string) error {
	if len(raw) == 0 {
		return importFail(http.StatusBadRequest, "package.json is required")
	}
	var pkg struct {
		Type            string            `json:"type"`
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
		BundleDeps      any               `json:"bundleDependencies"`
		BundledDeps     any               `json:"bundledDependencies"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return importFail(http.StatusBadRequest, "package.json is not valid JSON")
	}
	if pkg.Type != "" && pkg.Type != "module" {
		return importFail(http.StatusBadRequest, "package.json type must be module")
	}
	if pkg.Scripts["dev"] == "" || pkg.Scripts["build"] == "" {
		return importFail(http.StatusBadRequest, "package.json needs dev and build scripts")
	}
	for name := range pkg.Scripts {
		if importForbiddenScripts[name] {
			return importFail(http.StatusBadRequest, "package.json must not include the "+name+" lifecycle script")
		}
	}
	if pkg.BundleDeps != nil || pkg.BundledDeps != nil {
		return importFail(http.StatusBadRequest, "package.json must not include bundledDependencies")
	}
	allow := map[string]bool{}
	for k := range importCommonPkgs {
		allow[k] = true
	}
	if framework == "svelte" {
		for k := range importSveltePkgs {
			allow[k] = true
		}
	} else {
		for k := range importReactPkgs {
			allow[k] = true
		}
	}
	for _, deps := range []map[string]string{pkg.Dependencies, pkg.DevDependencies} {
		for name := range deps {
			if !allow[name] {
				return importFail(http.StatusBadRequest, "package.json has a dependency that is not allowed: "+name)
			}
		}
	}
	return nil
}

func overlayTooling(dst, framework string, tpl templates.Profile) error {
	copyIfMissing := func(name string) error {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(name))); err == nil {
			return nil
		}
		raw, err := fs.ReadFile(tpl.Files, name)
		if err != nil {
			return nil // starter may not have this file
		}
		return writeInside(dst, name, raw, 0o644)
	}
	for _, name := range importOverlayFiles {
		if name == "svelte.config.js" && framework != "svelte" {
			continue
		}
		if err := copyIfMissing(name); err != nil {
			return err
		}
	}
	for _, dir := range importOverlayRoots {
		_ = fs.WalkDir(tpl.Files, dir, func(name string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(name))); err == nil {
				return nil
			}
			raw, err := fs.ReadFile(tpl.Files, name)
			if err != nil {
				return nil
			}
			_ = writeInside(dst, name, raw, 0o644)
			return nil
		})
	}
	if _, err := os.Stat(filepath.Join(dst, ".gitignore")); err != nil {
		_ = writeInside(dst, ".gitignore", []byte(templatesGitignore()), 0o644)
	}
	return nil
}

func templatesGitignore() string {
	return "node_modules/\ndist/\ncoverage/\n.env\n*.log\n.vite/\n"
}

func mergePackageJSON(dst, framework string, tpl templates.Profile) error {
	path := filepath.Join(dst, "package.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var pkg map[string]any
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return err
	}
	if _, ok := pkg["type"]; !ok {
		pkg["type"] = "module"
	}
	if _, ok := pkg["private"]; !ok {
		pkg["private"] = true
	}
	engines, _ := pkg["engines"].(map[string]any)
	if engines == nil {
		engines = map[string]any{}
	}
	if _, ok := engines["bun"]; !ok {
		engines["bun"] = ">=1.3.3"
	}
	pkg["engines"] = engines
	scripts, _ := pkg["scripts"].(map[string]any)
	if scripts == nil {
		scripts = map[string]any{}
	}
	if _, err := os.Stat(filepath.Join(dst, "scripts/rules-check.ts")); err == nil {
		if _, ok := scripts["rules:check"]; !ok {
			scripts["rules:check"] = "bun scripts/rules-check.ts"
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "scripts/ui8px-gate.ts")); err == nil {
		if _, ok := scripts["lint:px"]; !ok {
			scripts["lint:px"] = "bun scripts/ui8px-gate.ts"
		}
	}
	if _, ok := scripts["check:ui8kit"]; !ok {
		scripts["check:ui8kit"] = "ui8kit check"
	}
	if _, ok := scripts["check"]; !ok {
		if framework == "svelte" {
			scripts["check"] = "svelte-check --tsconfig ./tsconfig.json --fail-on-warnings"
		} else {
			scripts["check"] = "tsc --noEmit"
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "scripts/ui8px-gate.ts")); err == nil {
		if _, ok := scripts["verify"]; !ok {
			scripts["verify"] = "bun run check:ui8kit && bun run rules:check && bun run lint:px && bun run check && bun run build"
		}
	}
	pkg["scripts"] = scripts
	dev, _ := pkg["devDependencies"].(map[string]any)
	if dev == nil {
		dev = map[string]any{}
	}
	if tplRaw, err := fs.ReadFile(tpl.Files, "package.json"); err == nil {
		var tplPkg struct {
			DevDependencies map[string]string `json:"devDependencies"`
			UI8Kit          any               `json:"ui8kit"`
		}
		if json.Unmarshal(tplRaw, &tplPkg) == nil {
			for k, v := range tplPkg.DevDependencies {
				if _, ok := dev[k]; !ok {
					// Only add packages the allowlist already accepts.
					if importCommonPkgs[k] || (framework == "svelte" && importSveltePkgs[k]) || (framework == "react" && importReactPkgs[k]) {
						dev[k] = v
					}
				}
			}
			if _, ok := pkg["ui8kit"]; !ok && tplPkg.UI8Kit != nil {
				pkg["ui8kit"] = tplPkg.UI8Kit
			}
			// A BuildY Chat pack composes registry blocks in src/App.svelte and src/sections (not Block-rooted
			// organisms). Point the contract checks at the registry files so verify/lint:px can run.
			if _, err := os.Stat(filepath.Join(dst, "GENERATOR.json")); err == nil {
				pkg["ui8kit"] = map[string]any{
					"runtime":     framework,
					"kit":         []any{"src/kit", "src/compose", "src/icons", "src/utils", "src/presets", "src/components"},
					"composition": []any{"src/blocks"},
					"blockRoot":   []any{"src/blocks"},
				}
				// Chat-generated sections spread JSON props as string; svelte-check then fails, and
				// excluding those files is ineffective because src/main.ts imports them. Keep lint:px and build.
				if s, ok := pkg["scripts"].(map[string]any); ok {
					s["verify"] = "bun run check:ui8kit && bun run rules:check && bun run lint:px && bun run build"
				}
			}
		}
	}
	pkg["devDependencies"] = dev
	out, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

func ensureViteHMR(dst, framework string) error {
	candidates := []string{"vite.config.ts", "vite.config.js"}
	var path string
	var raw []byte
	for _, c := range candidates {
		b, err := os.ReadFile(filepath.Join(dst, c))
		if err == nil {
			path, raw = filepath.Join(dst, c), b
			break
		}
	}
	if path == "" {
		return nil
	}
	if bytes.Contains(raw, []byte("HMR_CLIENT_PORT")) {
		return nil
	}
	plugin := `import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";
`
	plugins := "tailwindcss(), svelte()"
	if framework == "react" {
		plugin = `import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
`
		plugins = "react(), tailwindcss()"
	}
	content := `import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";
` + plugin + `
const hmrPort = process.env.HMR_CLIENT_PORT ? Number(process.env.HMR_CLIENT_PORT) : undefined;
const hmrProtocol = process.env.HMR_PROTOCOL;

export default defineConfig({
  plugins: [` + plugins + `],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  server: {
    host: "127.0.0.1",
    port: 5173,
    allowedHosts: true,
    hmr: {
      ...(hmrPort ? { clientPort: hmrPort } : {}),
      ...(hmrProtocol === "ws" || hmrProtocol === "wss" ? { protocol: hmrProtocol } : {}),
    },
  },
  preview: { host: "127.0.0.1", port: 4173 },
});
`
	return os.WriteFile(path, []byte(content), 0o644)
}

func verifyImported(ctx context.Context, dir string, log func(string)) (string, error) {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", err
	}
	if json.Unmarshal(raw, &pkg) != nil || pkg.Scripts["verify"] == "" {
		return "", nil
	}
	var out bytes.Buffer
	err = runProjectTool(ctx, dir, []string{"bun", "run", "verify"}, isolate.Spec{}, log, &out)
	return strings.TrimSpace(out.String()), err
}
