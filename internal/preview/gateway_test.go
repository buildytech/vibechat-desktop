package preview

import (
	"context"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestGatewayRoutesByHost: one listener, previews by <projectId>.preview.localhost:<port>; grants and cookies stay per
// project; unknown, malformed or foreign Hosts never reach a dev server.
func TestGatewayRoutesByHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	addr := ln.Addr().String()
	dir := t.TempDir()
	m := NewGateway(func(s string) { t.Log(s) }, port)
	srv := &http.Server{Handler: m.Gateway()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Cleanup(m.StopAll)

	const a, b = "aa11aa11aa11aa11aa11aa11aa11aa11", "bb22bb22bb22bb22bb22bb22bb22bb22"
	for _, id := range []string{a, b} {
		if _, err := m.Ensure(id, dir, fakeProfile()); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for !m.Status(a).Ready || !m.Status(b).Ready {
		if time.Now().After(deadline) {
			t.Fatalf("never ready: %+v %+v", m.Status(a), m.Status(b))
		}
		time.Sleep(100 * time.Millisecond)
	}
	ps := strconv.Itoa(port)
	originA := "http://" + a + ".preview.localhost:" + ps
	if got := m.Status(a).URL; got != originA+"/" {
		t.Fatalf("url %s", got)
	}
	// Every name dials the gateway, as *.localhost does in a browser.
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Transport: tr, Jar: jar}
	bare := &http.Client{Transport: tr}

	if code, _ := get(t, bare, originA+"/", nil); code != 403 {
		t.Fatalf("no grant: %d", code)
	}
	grant, _, err := m.Grant(a)
	if err != nil || !strings.HasPrefix(grant, originA+"/?"+GrantParam+"=") {
		t.Fatalf("grant %q %v", grant, err)
	}
	code, body := get(t, c, grant, nil)
	if code != 200 || !strings.Contains(body, "hmr="+ps) || !strings.Contains(body, "host=127.0.0.1:") || strings.Contains(body, GrantParam) {
		t.Fatalf("granted: %d %s", code, body)
	}
	if code, _ := get(t, c, originA+"/src/main.ts", nil); code != 200 {
		t.Fatal("cookie refused")
	}
	originB := "http://" + b + ".preview.localhost:" + ps
	if code, _ := get(t, c, originB+"/", nil); code != 403 {
		t.Fatal("project a's session opened project b")
	}
	if code, _ := get(t, bare, originB+"/", map[string]string{"Referer": originA + "/"}); code != 403 {
		t.Fatal("a page of project a counted as same-origin for b")
	}
	if code, _ := get(t, bare, originB+"/", map[string]string{"Sec-Fetch-Site": "same-origin"}); code != 200 {
		t.Fatal("same-origin denied")
	}
	for _, h := range []string{
		"http://cc33cc33.preview.localhost:" + ps, // not running
		"http://evil.preview.localhost:" + ps,     // not a project id
		"http://" + a + ".preview.localhost.evil.example:" + ps,
		"http://127.0.0.1:" + ps,
		"http://" + a + ".preview.localhost:1", // another port
	} {
		if code, _ := get(t, bare, h+"/", map[string]string{"Sec-Fetch-Site": "same-origin"}); code != 404 {
			t.Errorf("%s: %d, want 404", h, code)
		}
	}
	m.Stop(a)
	if code, _ := get(t, c, originA+"/", nil); code != 404 {
		t.Fatalf("stopped preview: %d", code)
	}
	if code, _ := get(t, bare, originB+"/", map[string]string{"Sec-Fetch-Site": "same-origin"}); code != 200 {
		t.Fatal("stopping a stopped b")
	}
}
