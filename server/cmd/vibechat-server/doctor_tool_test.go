package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Bun lives in <data>/.bun/bin (the service's PATH), not on a shell's PATH: doctor finds it there.
func TestLookToolFindsTheServiceBun(t *testing.T) {
	data := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	if _, err := lookTool(paths{data: data}, "bun"); err == nil {
		t.Fatal("found a bun that is not there")
	}
	bin := filepath.Join(data, ".bun", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "bun"), []byte("#!/bin/sh\necho 1.3.9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := lookTool(paths{data: data}, "bun"); err != nil || got != filepath.Join(bin, "bun") {
		t.Fatal(got, err)
	}
}
