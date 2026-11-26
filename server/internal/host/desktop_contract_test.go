package host

import (
	"net/http/httptest"
	"testing"
)

// TestDesktopRoutesAreNotTheServers: the desktop app answers its own routes (desktop/internal/remote, checked against
// the contract there); the host router of a vibechat-server answers 404, so the Studio in a browser hides the server
// settings.
func TestDesktopRoutesAreNotTheServers(t *testing.T) {
	s := pinnedSeam(t)
	if len(s.Desktop) != 5 {
		t.Fatalf("desktop routes %v", s.Desktop)
	}
	h := testHost(t, s)
	for _, r := range s.Desktop {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(r.Method, s.Prefix+r.Path, nil))
		if rec.Code != 404 {
			t.Errorf("the host router answers %s: %d", r.Key(), rec.Code)
		}
	}
}
