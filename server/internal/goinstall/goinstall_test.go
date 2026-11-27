package goinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeGo is an archive whose go/bin/go answers `go env GOVERSION` with version.
func fakeGo(t *testing.T, version string, extra ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name string, mode int64, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	add("go/bin/go", 0o755, "#!/bin/sh\necho go"+version+"\n")
	add("go/VERSION", 0o644, "go"+version+"\n")
	for _, e := range extra {
		add(e, 0o644, "x")
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func serve(t *testing.T, body []byte) (*httptest.Server, Release) {
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(srv.Close)
	return srv, Release{Version: "1.27.4", URL: srv.URL + "/go.tgz", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
}

func TestEnsureInstallsChecksAndKeeps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for go")
	}
	root := filepath.Join(t.TempDir(), "opt", "go")
	_, rel := serve(t, fakeGo(t, "1.27.4"))
	msg, err := Ensure(context.Background(), root, rel, nil)
	if err != nil || !strings.Contains(msg, "Installed Go 1.27.4") {
		t.Fatalf("%q %v", msg, err)
	}
	if v := Installed(root); v != "1.27.4" {
		t.Fatalf("installed %q", v)
	}
	msg, err = Ensure(context.Background(), root, Release{Version: "1.27.4", URL: "http://127.0.0.1:1/never"}, nil)
	if err != nil || !strings.Contains(msg, "is installed") {
		t.Fatalf("second run: %q %v", msg, err)
	}
}

func TestEnsureRefusesBadArchives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for go")
	}
	root := filepath.Join(t.TempDir(), "go")
	_, rel := serve(t, fakeGo(t, "1.27.4"))
	bad := rel
	bad.SHA256 = strings.Repeat("0", 64)
	if _, err := Ensure(context.Background(), root, bad, nil); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("sha: %v", err)
	}
	bad = rel
	bad.Size++
	if _, err := Ensure(context.Background(), root, bad, nil); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("size: %v", err)
	}
	_, evil := serve(t, fakeGo(t, "1.27.4", "../escape"))
	if _, err := Ensure(context.Background(), root, evil, nil); err == nil || !strings.Contains(err.Error(), "leaves") {
		t.Fatalf("traversal: %v", err)
	}
	_, other := serve(t, fakeGo(t, "1.26.0"))
	if _, err := Ensure(context.Background(), root, other, nil); err == nil || !strings.Contains(err.Error(), "reports") {
		t.Fatalf("version: %v", err)
	}
	if Installed(root) != "" {
		t.Fatal("a refused archive was installed")
	}
}
