package godev

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const siteMain = `package main

import (
	"flag"
	"net/http"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "dev" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
	addr := flag.String("http", "127.0.0.1:8090", "")
	preset := flag.String("preset", "", "")
	flag.Parse()
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(MSG + " " + *preset + " " + os.Getenv("SITE_DATA_DIR"))) })
	_ = http.ListenAndServe(*addr, nil)
}
`

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func get(port int) (int, string) {
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitFor(t *testing.T, port int, code int, body string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if c, b := get(port); c == code && strings.Contains(b, body) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	c, b := get(port)
	t.Fatalf("want %d %q, got %d %q", code, body, c, b)
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// mtime granularity: make every write a visible change
	later := time.Now().Add(time.Duration(time.Now().UnixNano()%1000+1) * time.Millisecond)
	_ = os.Chtimes(p, later, later)
}

// TestRunBuildsServesAndRebuilds: first build fails → the log is served (500); a fix → the site (preset from the
// Makefile, data in .vibechat/pb_data); an edit → rebuilt; a broken edit → the previous site keeps serving.
func TestRunBuildsServesAndRebuilds(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is required")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Go projects run on the Linux server")
	}
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/site\n\ngo 1.22\n")
	write(t, dir, "Makefile", "PRESET ?= shop\n")
	write(t, dir, "cmd/site/main.go", siteMain)
	write(t, dir, "cmd/site/msg.go", "package main\n\nconst MSG = \n") // broken
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Dir: dir, Port: port, Cache: t.TempDir(), Poll: 200 * time.Millisecond, Out: io.Discard})
	}()
	defer func() {
		cancel()
		<-done
	}()
	waitFor(t, port, http.StatusInternalServerError, "The Go build failed")
	write(t, dir, "cmd/site/msg.go", "package main\n\nconst MSG = \"v1\"\n")
	waitFor(t, port, http.StatusOK, "v1 shop "+filepath.Join(dir, ".vibechat/pb_data"))
	write(t, dir, "cmd/site/msg.go", "package main\n\nconst MSG = \"v2\"\n")
	waitFor(t, port, http.StatusOK, "v2")
	write(t, dir, "cmd/site/msg.go", "package main\n\nconst MSG = v3 broken\n")
	time.Sleep(3 * time.Second)
	waitFor(t, port, http.StatusOK, "v2")
}

func TestFingerprintIgnoresGeneratedAndOutput(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", "package a")
	before := fingerprint(dir)
	write(t, dir, "a_templ.go", "package a")
	write(t, dir, "sections_gen.go", "package a")
	write(t, dir, ".vibechat/bin/site", "x")
	write(t, dir, ".git/HEAD", "x")
	if fingerprint(dir) != before {
		t.Fatal("generated files or output changed the fingerprint")
	}
	write(t, dir, "b.templ", "x")
	if fingerprint(dir) == before {
		t.Fatal("a source change did not change the fingerprint")
	}
	if !templStale(dir) {
		t.Fatal("b.templ has no b_templ.go: stale")
	}
}

func TestBuildingPageWaitUi(t *testing.T) {
	for _, want := range []string{
		"Building the Go site",
		"templ generate, go build",
		"prefers-reduced-motion",
		`role="status"`,
		"aria-busy",
		"elapsed",
		"This page reloads by itself",
	} {
		if !strings.Contains(buildingPage, want) {
			t.Fatalf("building page: missing %q", want)
		}
	}
	if strings.Contains(buildingPage, "percent complete") || strings.Contains(buildingPage, "% complete") {
		t.Fatal("building page must not invent a percentage")
	}
}
