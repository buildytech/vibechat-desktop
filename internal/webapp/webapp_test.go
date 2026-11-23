package webapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestFilesThenFallback(t *testing.T) {
	site := fstest.MapFS{
		"index.html":        {Data: []byte("<!doctype html><p>studio</p>")},
		"assets/index-a.js": {Data: []byte("export const a = 1;")},
		"assets/x.css":      {Data: []byte("body{}")},
	}
	h := Handler(site)
	cases := []struct{ path, want, typ string }{
		{"/", "studio", "text/html"},
		{"/p/abc", "studio", "text/html"},
		{"/settings", "studio", "text/html"},
		{"/assets/index-a.js", "export const a", "javascript"},
		{"/assets/x.css", "body", "text/css"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://wails.localhost"+c.path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), c.want) || !strings.Contains(rec.Header().Get("content-type"), c.typ) {
			t.Errorf("%s: %d %q %q", c.path, rec.Code, rec.Header().Get("content-type"), rec.Body.String())
		}
	}
}

func TestNotBuilt(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(fstest.MapFS{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://wails.localhost/", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "build-frontend.sh") {
		t.Errorf("got %d %q", rec.Code, rec.Body.String())
	}
}
