package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/host"
	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// TestTemplatesInstallE2E creates a project from every template with the app's code (templates.Scaffold +
// host.InstallDeps) and installs its dependencies with the Bun on PATH. Network and Bun needed, so it runs only with
// VIBECHAT_E2E_INSTALL=1 (CI: job e2e-install, Windows, several Bun versions). VIBECHAT_E2E_DIR picks the parent folder
// (default: a temp dir), to test paths like the app's projects folder.
func TestTemplatesInstallE2E(t *testing.T) {
	if os.Getenv("VIBECHAT_E2E_INSTALL") != "1" {
		t.Skip("set VIBECHAT_E2E_INSTALL=1 (needs Bun and network)")
	}
	base := os.Getenv("VIBECHAT_E2E_DIR")
	if base == "" {
		base = t.TempDir()
	}
	for _, tpl := range Templates() {
		t.Run(tpl.ID, func(t *testing.T) {
			dir := filepath.Join(base, "e2e "+tpl.ID)
			_ = os.RemoveAll(dir)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if err := templates.Scaffold(ctx, tpl, dir); err != nil {
				t.Fatal(err)
			}
			if err := host.InstallDeps(ctx, dir, tpl, func(l string) { t.Log(l) }); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{"node_modules/vite/package.json", "node_modules/tailwindcss/package.json"} {
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
					t.Errorf("%s missing after install", f)
				}
			}
		})
	}
}
