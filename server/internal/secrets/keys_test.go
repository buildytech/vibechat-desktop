package secrets

import (
	"strings"
	"testing"
)

func TestMaskShowsOnlyTheTail(t *testing.T) {
	m := Mask("cursor_secretvalue1234")
	if !strings.HasSuffix(m, "1234") || strings.Contains(m, "secret") {
		t.Fatalf("mask %q", m)
	}
}
