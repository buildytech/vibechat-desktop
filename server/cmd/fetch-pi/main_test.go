package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("tarball bytes"))
	}))
	defer srv.Close()
	b, err := download(srv.URL + "/ok.tgz")
	if err != nil || string(b) != "tarball bytes" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := download(srv.URL + "/missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("%v", err)
	}
	if _, err := download("http://127.0.0.1:1/x"); err == nil {
		t.Fatal("unreachable host answered")
	}
}
