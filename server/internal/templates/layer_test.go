package templates

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body string
	link       bool
}

func archive(t *testing.T, top string, es []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "x"}})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top + "/", Mode: 0o755})
	for _, e := range es {
		if e.link {
			_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: top + "/" + e.name, Linkname: "/etc/passwd"})
			continue
		}
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: top + "/" + e.name, Mode: 0o644, Size: int64(len(e.body))})
		_, _ = tw.Write([]byte(e.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func layerOf(t *testing.T, files map[string]string, exec ...string) *Layer {
	t.Helper()
	hashes := map[string]string{}
	for k, v := range files {
		hashes[k] = sum(v)
	}
	raw, _ := json.Marshal(map[string]any{
		"schemaVersion": 1, "base": "empty",
		"source": map[string]any{"kind": "github-archive", "repo": "o/starter", "commit": strings.Repeat("c", 40), "exclude": []string{".github/"}},
		"files":  hashes, "executables": exec,
	})
	l, err := ParseLayer(raw)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func serve(t *testing.T, body []byte) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/o/starter/tar.gz/"+strings.Repeat("c", 40) {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	old := ArchiveBase
	ArchiveBase = srv.URL
	t.Cleanup(func() { ArchiveBase = old })
	SetCacheDir(t.TempDir())
}

func TestLayerFetchChecksEveryFile(t *testing.T) {
	files := map[string]string{"go.mod": "module x\n", "cmd/site/main.go": "package main\n", "scripts/verify.sh": "#!/bin/sh\n"}
	l := layerOf(t, files, "scripts/verify.sh")
	es := []entry{{name: "go.mod", body: files["go.mod"]}, {name: "cmd/site/main.go", body: files["cmd/site/main.go"]}, {name: "scripts/verify.sh", body: files["scripts/verify.sh"]}, {name: ".github/workflows/ci.yml", body: "x"}}
	serve(t, archive(t, "starter-cccc", es))
	f := LayerFS(l)
	raw, err := fs.ReadFile(f, "cmd/site/main.go")
	if err != nil || string(raw) != "package main\n" {
		t.Fatalf("read: %q %v", raw, err)
	}
	if _, err := fs.Stat(f, ".github/workflows/ci.yml"); err == nil {
		t.Fatal("an excluded path was kept")
	}
	dir, err := EnsureLayer(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Join(dir, "scripts/verify.sh"))
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("executable mode %v", st.Mode())
	}
	// Cached: a second call does not download (the server is gone).
	ArchiveBase = "http://127.0.0.1:1"
	if _, err := EnsureLayer(context.Background(), l); err != nil {
		t.Fatal("cached layer refetched: ", err)
	}
}

func TestLayerFetchRefusesADifferentTree(t *testing.T) {
	files := map[string]string{"go.mod": "module x\n", "main.go": "package main\n"}
	for name, es := range map[string][]entry{
		"changed": {{name: "go.mod", body: "module evil\n"}, {name: "main.go", body: files["main.go"]}},
		"extra":   {{name: "go.mod", body: files["go.mod"]}, {name: "main.go", body: files["main.go"]}, {name: "evil.go", body: "package main"}},
		"missing": {{name: "go.mod", body: files["go.mod"]}},
		"link":    {{name: "go.mod", body: files["go.mod"]}, {name: "main.go", link: true}},
	} {
		t.Run(name, func(t *testing.T) {
			serve(t, archive(t, "starter-cccc", es))
			if _, err := EnsureLayer(context.Background(), layerOf(t, files)); err == nil {
				t.Fatal("a tree that does not match layer.json was accepted")
			}
		})
	}
}

func TestParseLayerRefusesBadEntries(t *testing.T) {
	good := `"source":{"kind":"github-archive","repo":"o/r","commit":"` + strings.Repeat("a", 40) + `"}`
	h := strings.Repeat("b", 64)
	for name, raw := range map[string]string{
		"base":     `{"schemaVersion":1,"base":"svelte",` + good + `,"files":{"a":"` + h + `"}}`,
		"commit":   `{"schemaVersion":1,"base":"empty","source":{"kind":"github-archive","repo":"o/r","commit":"main"},"files":{"a":"` + h + `"}}`,
		"repo":     `{"schemaVersion":1,"base":"empty","source":{"kind":"github-archive","repo":"../x","commit":"` + strings.Repeat("a", 40) + `"},"files":{"a":"` + h + `"}}`,
		"dotdot":   `{"schemaVersion":1,"base":"empty",` + good + `,"files":{"../a":"` + h + `"}}`,
		"abs":      `{"schemaVersion":1,"base":"empty",` + good + `,"files":{"/etc/a":"` + h + `"}}`,
		"git":      `{"schemaVersion":1,"base":"empty",` + good + `,"files":{".git/config":"` + h + `"}}`,
		"hash":     `{"schemaVersion":1,"base":"empty",` + good + `,"files":{"a":"x"}}`,
		"exec":     `{"schemaVersion":1,"base":"empty",` + good + `,"files":{"a":"` + h + `"},"executables":["b"]}`,
		"no files": `{"schemaVersion":1,"base":"empty",` + good + `}`,
	} {
		if _, err := ParseLayer([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
