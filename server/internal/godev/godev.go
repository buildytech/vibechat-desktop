// Package godev is the live preview loop of a Go templ project (`vibechat-server go-dev --port N`, the go-templ
// runner's dev argv). It runs inside the tools jail in the project folder: build (go generate when the project has
// //go:generate lines, templ generate when a .templ is newer than its _templ.go, go build), run the site on
// 127.0.0.1:PORT with its PocketBase data in .vibechat/pb_data, and rebuild on change. One Go build at a time on the
// server (golock); a failed rebuild keeps the running site, a failed first build serves the build log instead.
package godev

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/golock"
	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

// Options of one loop.
type Options struct {
	Dir   string        // project folder (the working directory)
	Port  int           // the preview port (127.0.0.1)
	Cache string        // the shared Go cache (the build lock)
	Poll  time.Duration // change polling interval (default 1s)
	Out   io.Writer     // build and site output
}

const (
	binDir  = ".vibechat/bin"
	dataDir = ".vibechat/pb_data"
)

// Run builds, serves and rebuilds until ctx ends.
func Run(ctx context.Context, o Options) error {
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	l := &loop{o: o}
	defer l.stop()
	print := func(format string, a ...any) { fmt.Fprintf(o.Out, "go-dev: "+format+"\n", a...) }
	last := fingerprint(o.Dir)
	// The first build of a project can take minutes on a small server: the preview answers at once with a page
	// that reloads itself until the site is up.
	l.serveBuilding()
	if log, err := l.build(ctx); err != nil {
		print("build failed (serving the log until a build succeeds)")
		l.serveError(log)
	} else if err := l.start(); err != nil {
		l.serveError(err.Error())
	} else {
		l.waitHealthy(ctx)
	}
	t := time.NewTicker(o.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		fp := fingerprint(o.Dir)
		if fp == last {
			continue
		}
		// Wait for the writes to settle (an agent edit is several files).
		time.Sleep(300 * time.Millisecond)
		last = fingerprint(o.Dir)
		print("change: rebuilding")
		log, err := l.build(ctx)
		if err != nil {
			print("rebuild failed; the previous build keeps running\n%s", log)
			if l.cmd == nil {
				l.serveError(log)
			}
			continue
		}
		l.stop()
		if err := l.start(); err != nil {
			l.serveError(err.Error())
			continue
		}
		l.waitHealthy(ctx)
	}
}

// waitHealthy waits (up to a minute) for the site's /healthz to answer 200, and reports it.
func (l *loop) waitHealthy(ctx context.Context) {
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://127.0.0.1:" + strconv.Itoa(l.o.Port) + "/healthz"
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if res, err := client.Get(url); err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				fmt.Fprintln(l.o.Out, "go-dev: site ready (/healthz 200)")
				return
			}
		}
		select {
		case <-l.done:
			fmt.Fprintln(l.o.Out, "go-dev: the site exited")
			return
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
	fmt.Fprintln(l.o.Out, "go-dev: the site did not answer /healthz within a minute")
}

type loop struct {
	o    Options
	cmd  *exec.Cmd
	done chan struct{}
	errS *http.Server
	mu   sync.Mutex
}

// build runs the generators and go build under the server's build lock; it returns the combined output.
func (l *loop) build(ctx context.Context) (string, error) {
	release, err := golock.Lock(ctx, l.o.Cache)
	if err != nil {
		return err.Error(), err
	}
	defer release()
	var out bytes.Buffer
	w := io.MultiWriter(&out, l.o.Out)
	run := func(argv ...string) error {
		fmt.Fprintf(w, "$ %s\n", strings.Join(argv, " "))
		c := procgroup.CommandContext(ctx, argv[0], argv[1:]...)
		c.Dir, c.Stdout, c.Stderr = l.o.Dir, w, w
		return c.Run()
	}
	if hasGenerate(l.o.Dir) {
		if err := run("go", "generate", "./..."); err != nil {
			return out.String(), err
		}
	}
	if templStale(l.o.Dir) {
		if err := run("go", "tool", "templ", "generate"); err != nil {
			return out.String(), err
		}
	}
	if err := os.MkdirAll(filepath.Join(l.o.Dir, binDir), 0o755); err != nil {
		return err.Error(), err
	}
	next := filepath.Join(binDir, "site.next")
	if err := run("go", "build", "-p", strconv.Itoa(buildProcs()), "-o", next, "./cmd/site"); err != nil {
		return out.String(), err
	}
	if err := os.Rename(filepath.Join(l.o.Dir, next), filepath.Join(l.o.Dir, binDir, "site")); err != nil {
		return err.Error(), err
	}
	return out.String(), nil
}

// buildProcs: min(2, CPUs), the limit of the server's container.
func buildProcs() int {
	if n := runtime.NumCPU(); n < 2 {
		return n
	}
	return 2
}

var presetRe = regexp.MustCompile(`(?m)^PRESET\s*\?=\s*([a-z]+)\s*$`)

func (l *loop) start() error {
	l.closeError()
	args := []string{"dev", "--http", "127.0.0.1:" + strconv.Itoa(l.o.Port)}
	if raw, err := os.ReadFile(filepath.Join(l.o.Dir, "Makefile")); err == nil {
		if m := presetRe.FindSubmatch(raw); m != nil {
			args = append(args, "--preset", string(m[1]))
		}
	}
	c := procgroup.Command(filepath.Join(l.o.Dir, binDir, "site"), args...)
	c.Dir, c.Stdout, c.Stderr = l.o.Dir, l.o.Out, l.o.Out
	c.Env = append(os.Environ(), "SITE_DATA_DIR="+filepath.Join(l.o.Dir, dataDir))
	if err := c.Start(); err != nil {
		return err
	}
	l.cmd, l.done = c, make(chan struct{})
	go func(c *exec.Cmd, done chan struct{}) { _ = c.Wait(); close(done) }(c, l.done)
	return nil
}

func (l *loop) stop() {
	l.closeError()
	if l.cmd == nil {
		return
	}
	_ = l.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-l.done:
	case <-time.After(5 * time.Second):
		_ = l.cmd.Process.Kill()
		<-l.done
	}
	l.cmd = nil
}

const buildingPage = `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="3">
<title>Building…</title></head><body style="font:16px system-ui;margin:3rem">Building the Go site (templ generate, go build)…
this page reloads by itself.</body></html>`

// serveBuilding answers 503 with a self-reloading page while the first build runs.
func (l *loop) serveBuilding() {
	l.serve(http.StatusServiceUnavailable, "text/html; charset=utf-8", buildingPage)
}

// serveError answers every request with the build log (500) until the next successful build, so the preview shows
// why instead of waiting forever.
func (l *loop) serveError(log string) {
	l.serve(http.StatusInternalServerError, "text/plain; charset=utf-8", "The Go build failed; fix the error and save to rebuild.\n\n"+log)
}

func (l *loop) serve(status int, ctype, body string) {
	l.closeError()
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(l.o.Port))
	if err != nil {
		return
	}
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", ctype)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}), ReadHeaderTimeout: 5 * time.Second}
	l.mu.Lock()
	l.errS = s
	l.mu.Unlock()
	go func() { _ = s.Serve(ln) }()
}

func (l *loop) closeError() {
	l.mu.Lock()
	s := l.errS
	l.errS = nil
	l.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

// skipDir are folders that never change the build.
func skipDir(name string) bool {
	switch name {
	case ".git", ".vibechat", "node_modules", "pb_data", "bin", ".cursor", "skills", "deploy":
		return true
	}
	return false
}

// generated outputs do not count as changes (the generators rewrite them on every build).
func generated(name string) bool {
	return strings.HasSuffix(name, "_templ.go") || strings.HasSuffix(name, "_gen.go") || strings.HasSuffix(name, "_templ.txt")
}

// fingerprint is the size and mtime of every source file (polling: inotify is not available in every jail).
func fingerprint(dir string) string {
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if generated(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		fmt.Fprintf(&b, "%s %d %d\n", p, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return b.String()
}

// hasGenerate reports a //go:generate line in a Go source file of the project.
func hasGenerate(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return fs.SkipAll
		}
		if d.IsDir() {
			if p != dir && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || generated(d.Name()) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "//go:generate ") {
				found = true
				return fs.SkipAll
			}
		}
		return nil
	})
	return found
}

// templStale: a .templ file without its _templ.go or newer than it.
func templStale(dir string) bool {
	stale := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || stale {
			return fs.SkipAll
		}
		if d.IsDir() {
			if p != dir && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".templ") {
			return nil
		}
		src, err := d.Info()
		if err != nil {
			return nil
		}
		out, err := os.Stat(strings.TrimSuffix(p, ".templ") + "_templ.go")
		if err != nil || src.ModTime().After(out.ModTime()) {
			stale = true
			return fs.SkipAll
		}
		return nil
	})
	return stale
}
