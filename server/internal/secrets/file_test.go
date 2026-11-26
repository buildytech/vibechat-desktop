package secrets

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFileStoreIsPrivateAndRefusesLooseFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	dir := filepath.Join(t.TempDir(), "secrets")
	f := File{Path: filepath.Join(dir, "cursor-key")}
	if k, src := f.Key(); k != "" || src != "" || f.Check() != nil {
		t.Fatalf("empty store: %q %q %v", k, src, f.Check())
	}
	if err := f.Store("  test-key-value\n"); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(f.Path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", st.Mode().Perm())
	}
	dst, _ := os.Stat(dir)
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", dst.Mode().Perm())
	}
	if k, src := f.Key(); k != "test-key-value" || src != "file" {
		t.Fatalf("got %q %q", k, src)
	}
	if err := os.Chmod(f.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if k, _ := f.Key(); k != "" {
		t.Fatal("a world-readable key file was used")
	}
	if f.Check() == nil {
		t.Fatal("Check did not report the loose mode")
	}
	if err := f.Store("rotated"); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(f.Path); st.Mode().Perm() != 0o600 {
		t.Fatalf("rewrite kept mode %v", st.Mode().Perm())
	}
	if err := f.Forget(); err != nil || f.Forget() != nil {
		t.Fatal("forget")
	}
	if k, _ := f.Key(); k != "" {
		t.Fatal("forgotten key still there")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("temp files left: %v", entries)
	}
}
