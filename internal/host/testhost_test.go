package host

import "testing"

func testHost(t *testing.T, s Seam) *Host {
	t.Helper()
	return New(Options{Seam: s, Version: "test"})
}
