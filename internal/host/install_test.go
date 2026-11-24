package host

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		less bool
	}{{"1.3.3", "1.3.3", false}, {"1.2.9", "1.3.3", true}, {"1.4.4", "1.3.3", false}, {"1.3.10", "1.3.9", false}, {"1.3.3-canary.1+abc", "1.3.3", false}} {
		if got := versionLess(c.a, c.b); got != c.less {
			t.Errorf("versionLess(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestMinBunFromEngines(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"engines":{"bun":">=1.3.3"}}`), 0o644)
	if got := minBun(dir); got != "1.3.3" {
		t.Fatal(got)
	}
	if got := minBun(t.TempDir()); got != "" {
		t.Fatal(got)
	}
}
