package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The app has no local host: every seam path it does not own is 404 { error } (never the start screen's page),
// everything else goes on to the start screen (a stub here: the built shell is not needed for this test).
func TestSeamPathsAreNotFoundWithoutAServer(t *testing.T) {
	h := seamNotFound("/runtime/v1", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	for _, p := range []string{"/runtime/v1", "/runtime/v1/projects", "/runtime/v1/agent/sessions/x/events", "/runtime/v1/host/capabilities"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Body.String())
		}
	}
	for _, p := range []string{"/", "/settings", "/runtime"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

func TestDataDirOverride(t *testing.T) {
	t.Setenv("VIBECHAT_DATA_DIR", "/tmp/vibechat-test-data")
	if d := dataDir(); d != "/tmp/vibechat-test-data" {
		t.Fatal(d)
	}
}
