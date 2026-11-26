package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Every platform the installers ship must have both the bridge and the sandbox helper pinned.
func TestLockCoversShippedPlatforms(t *testing.T) {
	raw, err := os.ReadFile("../../bridge.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	var l lock
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"linux-x64", "win32-x64", "darwin-arm64", "darwin-x64"} {
		if len(l.Bridge.SHA256[p]) != 64 {
			t.Errorf("bridge %s: sha256 %q", p, l.Bridge.SHA256[p])
		}
		if len(l.Helper.Integrity[p]) != 95 {
			t.Errorf("helper %s: integrity %q", p, l.Helper.Integrity[p])
		}
	}
}

func TestUntarRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	body := []byte("x")
	_ = tw.WriteHeader(&tar.Header{Name: "package/../../evil", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = zw.Close()
	dst := filepath.Join(t.TempDir(), "out")
	if err := untar(buf.Bytes(), dst, "package/"); err == nil {
		t.Fatal("traversal accepted")
	}
}

// The manifest keeps every upstream field and gains the pinned release.
func TestStampRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(p, []byte(`{"bridgeVersion":"1.0.0","protocol":"sdk.v1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stampRelease(p, "1.2.0"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	var m struct {
		BridgeVersion string                   `json:"bridgeVersion"`
		Protocol      string                   `json:"protocol"`
		Release       struct{ Version string } `json:"release"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.BridgeVersion != "1.0.0" || m.Protocol != "sdk.v1" || m.Release.Version != "1.2.0" {
		t.Fatalf("%v %s", err, raw)
	}
}
