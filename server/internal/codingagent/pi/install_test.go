package pi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tgz(t *testing.T, files map[string]string, extra ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(files[n]))
	}
	for _, h := range extra {
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func testLock(data []byte) Lock {
	return Lock{Package: "@earendil-works/pi-coding-agent", Version: "9.9.9", Integrity: Integrity(data), Entry: "dist/bundle/cli.js", Node: "22.19.0"}
}

func goodFiles() map[string]string {
	return map[string]string{
		"package/package.json":       `{"name":"@earendil-works/pi-coding-agent","version":"9.9.9"}`,
		"package/dist/bundle/cli.js": "// pi",
	}
}

func fakeNode(t *testing.T, version string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho "+version+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPinnedLockIsWellFormed(t *testing.T) {
	l := PinnedLock()
	if l.Package != "@earendil-works/pi-coding-agent" || !strings.HasPrefix(l.Integrity, "sha512-") || l.Version == "" || strings.ContainsAny(l.Version, "^~*><= ") || l.Node != "22.19.0" {
		t.Fatalf("%+v", l)
	}
	if !strings.Contains(l.Tarball, "-"+l.Version+".tgz") {
		t.Fatalf("tarball %s does not match version %s", l.Tarball, l.Version)
	}
}

func TestExtractVerifiesAndLocateRequiresTheMarker(t *testing.T) {
	data := tgz(t, goodFiles())
	l := testLock(data)
	dir := filepath.Join(t.TempDir(), "pi")
	if err := Extract(l, data, dir); err != nil {
		t.Fatal(err)
	}
	in, err := Locate(l, fakeNode(t, "v22.19.0"), dir, nil)
	if err != nil || in.Version != "9.9.9" || in.NodeVersion != "22.19.0" || in.Entry != filepath.Join(dir, "dist", "bundle", "cli.js") {
		t.Fatalf("%+v %v", in, err)
	}
	// a second extract replaces the folder
	writeFile(t, filepath.Join(dir, "stale.txt"), "x")
	if err := Extract(l, data, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.txt")); !os.IsNotExist(err) {
		t.Fatal("old files left")
	}
	// a tarball with another digest is refused and nothing is written
	bad := tgz(t, map[string]string{"package/package.json": "{}"})
	other := filepath.Join(t.TempDir(), "pi2")
	if err := Extract(l, bad, other); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatal("a refused tarball left files")
	}
	// the marker is what makes a folder ours
	if err := os.Remove(filepath.Join(dir, MarkerFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Locate(l, fakeNode(t, "v22.19.0"), dir, nil); err == nil || !strings.Contains(err.Error(), "not installed by fetch-pi") {
		t.Fatalf("%v", err)
	}
}

func TestExtractRefusesUnsafeTarballs(t *testing.T) {
	cases := map[string][]byte{
		"outside package/": tgz(t, map[string]string{"evil.txt": "x"}),
		"dotdot":           tgz(t, map[string]string{"package/../../evil.txt": "x"}),
		"symlink":          tgz(t, goodFiles(), tar.Header{Name: "package/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}),
		"no entry":         tgz(t, map[string]string{"package/package.json": goodFiles()["package/package.json"]}),
		"wrong version":    tgz(t, map[string]string{"package/package.json": `{"name":"@earendil-works/pi-coding-agent","version":"1.0.0"}`, "package/dist/bundle/cli.js": "x"}),
		"wrong package":    tgz(t, map[string]string{"package/package.json": `{"name":"other","version":"9.9.9"}`, "package/dist/bundle/cli.js": "x"}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "pi")
			if err := Extract(testLock(data), data, dir); err == nil {
				t.Fatal("extracted")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("files left behind")
			}
		})
	}
}

func TestLocateFailsClosed(t *testing.T) {
	data := tgz(t, goodFiles())
	l := testLock(data)
	dir := filepath.Join(t.TempDir(), "pi")
	if err := Extract(l, data, dir); err != nil {
		t.Fatal(err)
	}
	node := fakeNode(t, "v22.19.0")
	look := func(string) (string, error) { return "", errors.New("not found") }
	cases := map[string]struct {
		node, dir string
		look      LookPath
		want      string
	}{
		"no dir configured": {node, "", nil, "not set"},
		"dir missing":       {node, filepath.Join(dir, "nope"), nil, "does not exist"},
		"node too old":      {fakeNode(t, "v20.11.1"), dir, nil, "needs Node 22.19.0 or newer"},
		"node 22.18":        {fakeNode(t, "v22.18.9"), dir, nil, "needs Node 22.19.0 or newer"},
		"bun instead":       {fakeNode(t, "1.2.3"), dir, nil, "needs Node"},
		"not a version":     {fakeNode(t, "hello"), dir, nil, "not a version"},
		"node not on PATH":  {"", dir, look, "node 22.19.0 or newer is not on PATH"},
		"node does not run": {filepath.Join(dir, "missing-node"), dir, nil, "--version"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Locate(l, c.node, c.dir, c.look)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
	// another version than the lock's, even with a valid marker
	l2 := l
	l2.Version = "9.9.10"
	if _, err := Locate(l2, node, dir, nil); err == nil || !strings.Contains(err.Error(), "pinned to") {
		t.Fatalf("%v", err)
	}
	// a marker of another integrity
	writeFile(t, filepath.Join(dir, MarkerFile), `{"version":"9.9.9","integrity":"sha512-other"}`)
	if _, err := Locate(l, node, dir, nil); err == nil || !strings.Contains(err.Error(), "not the pinned Pi") {
		t.Fatalf("%v", err)
	}
}

func TestAtLeast(t *testing.T) {
	for _, c := range []struct {
		v, min string
		want   bool
	}{{"22.19.0", "22.19.0", true}, {"22.23.5", "22.19.0", true}, {"22.18.99", "22.19.0", false}, {"23.0.0", "22.19.0", true},
		{"21.99.99", "22.19.0", false}, {"v24.1.0", "22.19.0", true}, {"1.2.3", "22.19.0", false}, {"junk", "22.19.0", false}} {
		if got := AtLeast(c.v, c.min); got != c.want {
			t.Errorf("AtLeast(%s, %s) = %v", c.v, c.min, got)
		}
	}
}
