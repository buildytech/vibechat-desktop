package server

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/host"
	"github.com/buildytech/vibechat-desktop/server/internal/preview"
	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// TestTemplImportPreviewE2E: a BuildY Chat templ ZIP imports into an empty project with the real templ template
// (the fastygo/starter layer at its pinned commit), installs (go mod download), verifies (go build), and its
// preview (vibechat-server go-dev through the preview proxy) answers / and /healthz with 200. Network and the
// pinned Go needed: VIBECHAT_E2E_GO=1. VIBECHAT_E2E_TEMPL_ZIPS adds ZIPs (comma-separated paths; the presets).
func TestTemplImportPreviewE2E(t *testing.T) {
	if os.Getenv("VIBECHAT_E2E_GO") != "1" {
		t.Skip("set VIBECHAT_E2E_GO=1 (needs Go and network)")
	}
	tmp := t.TempDir()
	self := filepath.Join(tmp, "vibechat-server")
	build := exec.Command("go", "build", "-o", self, "./cmd/vibechat-server")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}
	templates.Self = self
	if os.Getenv("VIBECHAT_LAYER_CACHE") == "" {
		templates.SetCacheDir(filepath.Join(tmp, "layers"))
	} else {
		templates.SetCacheDir(os.Getenv("VIBECHAT_LAYER_CACHE"))
	}
	var tpl templates.Profile
	all := Templates()
	for _, p := range all {
		if p.ID == "templ" {
			tpl = p
		}
	}
	lookup := func(id string) (templates.Profile, bool) {
		for _, p := range all {
			if p.ID == id {
				return p, true
			}
		}
		return templates.Profile{}, false
	}
	zips := []string{filepath.Join("internal", "host", "testdata", "chat-templ-pack.zip")}
	if extra := os.Getenv("VIBECHAT_E2E_TEMPL_ZIPS"); extra != "" {
		zips = append(zips, strings.Split(extra, ",")...)
	}
	m := preview.New(func(l string) { t.Log(l) })
	defer m.StopAll()
	for i, zip := range zips {
		t.Run(filepath.Base(zip), func(t *testing.T) {
			raw, err := os.ReadFile(zip)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(tmp, "project-"+strings.TrimSuffix(filepath.Base(zip), ".zip"))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			im, err := host.ImportInto(ctx, dir, raw, lookup)
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			if im.Framework != "templ" {
				t.Fatalf("framework %s", im.Framework)
			}
			t.Logf("layers %v", im.Layers)
			if err := host.InstallDeps(ctx, dir, tpl, func(l string) { t.Log(l) }); err != nil {
				t.Fatal(err)
			}
			if out, err := im.Verify(ctx, dir, func(l string) { t.Log(l) }); err != nil {
				t.Fatalf("verify: %v\n%s", err, out)
			}
			id := strings.Repeat("a", 31) + string(rune('0'+i))
			if _, err := m.Ensure(id, dir, tpl); err != nil {
				t.Fatal(err)
			}
			defer m.Stop(id)
			deadline := time.Now().Add(10 * time.Minute)
			var st host.PreviewStatus
			for time.Now().Before(deadline) {
				if st = m.Status(id); st.Status == "running" || st.Error != "" {
					break
				}
				time.Sleep(time.Second)
			}
			if st.Status != "running" {
				t.Fatalf("preview: %+v", st)
			}
			grant, _, err := m.Grant(id)
			if err != nil {
				t.Fatal(err)
			}
			jar, _ := cookiejar.New(nil)
			c := &http.Client{Jar: jar, Timeout: 10 * time.Second}
			// The first answers can be go-dev's building page (503) until the site is up.
			var code int
			var body string
			for time.Now().Before(deadline) {
				u := grant
				if code != 0 {
					u = st.URL + "/"
				}
				res, err := c.Get(u)
				if err == nil {
					b, _ := io.ReadAll(res.Body)
					res.Body.Close()
					code, body = res.StatusCode, string(b)
					if code == http.StatusOK {
						break
					}
				}
				time.Sleep(time.Second)
			}
			if code != http.StatusOK || !strings.Contains(strings.ToLower(body), "<html") {
				t.Fatalf("preview /: %d %.300s", code, body)
			}
			res, err := c.Get(st.URL + "/healthz")
			if err != nil || res.StatusCode != http.StatusOK {
				t.Fatalf("/healthz: %v %v", res, err)
			}
			res.Body.Close()
		})
	}
}
