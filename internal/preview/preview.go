// Package preview runs a project's dev server and shows it through a loopback proxy.
//
// Per project: the dev server (template profile Dev argv, e.g. `bun x vite --host 127.0.0.1 --port {port}`) runs in
// its own process group on a host-chosen 127.0.0.1 port; a proxy listens on a second 127.0.0.1 port, which is the
// origin the Studio iframe (and a browser tab) opens. The proxy lets a request through when
//
//   - it carries a valid one-time grant (`?__buildy_grant=…`, minted by the host for the Studio, 60 s, single use);
//     the answer also sets a session cookie, or
//   - it carries that session cookie, or
//   - the browser says it comes from the preview itself (Sec-Fetch-Site: same-origin, or an Origin / Referer equal to
//     the proxy origin): module imports, HMR WebSocket, reloads and in-app links of an already-granted page.
//
// Every other request (another site in the browser, DNS rebinding with a foreign Host) gets 403. The proxy speaks
// WebSocket (Vite HMR: the dev server's HMR client port is the proxy port).
package preview

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildytech/vibechat-desktop/internal/host"
	"github.com/buildytech/vibechat-desktop/internal/procgroup"
	"github.com/buildytech/vibechat-desktop/internal/templates"
)

const (
	// GrantParam is the query parameter of a one-time grant (the Studio strips it after the first load).
	GrantParam = "__buildy_grant"
	grantTTL   = 60 * time.Second
	cookieName = "vibechat_preview"
	readyWait  = 3 * time.Minute
)

// Manager owns the dev servers. Zero value is not usable; call New.
type Manager struct {
	log func(string)
	// environ is the base environment of dev servers (os.Environ without CURSOR_API_KEY).
	environ []string

	mu       sync.Mutex
	projects map[string]*server
}

// New returns a Manager. log may be nil.
func New(log func(string)) *Manager {
	if log == nil {
		log = func(string) {}
	}
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(name, "CURSOR_API_KEY") {
			continue
		}
		env = append(env, kv)
	}
	return &Manager{log: log, environ: env, projects: map[string]*server{}}
}

type server struct {
	id       string
	cmd      *exec.Cmd
	done     chan struct{}
	devPort  int
	ln       net.Listener
	http     *http.Server
	origin   string // http://127.0.0.1:<proxy port>
	session  string // cookie value
	out      *tailBuffer
	mu       sync.Mutex
	status   string // starting | running | exited
	ready    bool
	err      string
	grants   map[string]time.Time
	stopping bool
}

func (s *server) snapshot() host.PreviewStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := host.PreviewStatus{Status: s.status, Ready: s.ready, Error: s.err}
	if s.status != "exited" {
		st.URL = s.origin + "/"
	}
	return st
}

// Status answers the project's preview state ("stopped" when none).
func (m *Manager) Status(id string) host.PreviewStatus {
	m.mu.Lock()
	s := m.projects[id]
	m.mu.Unlock()
	if s == nil {
		return host.PreviewStatus{Status: "stopped"}
	}
	return s.snapshot()
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Ensure starts the project's dev server and proxy unless they run.
func (m *Manager) Ensure(id, dir string, p templates.Profile) (host.PreviewStatus, error) {
	if len(p.Dev) == 0 {
		return host.PreviewStatus{}, errors.New("the template has no dev server")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.projects[id]; s != nil {
		if st := s.snapshot(); st.Status != "exited" {
			return st, nil
		}
		m.stopLocked(id)
	}
	devPort, err := freePort()
	if err != nil {
		return host.PreviewStatus{}, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return host.PreviewStatus{}, err
	}
	proxyPort := ln.Addr().(*net.TCPAddr).Port
	argv := templates.Expand(p.Dev, devPort, proxyPort)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append([]string{}, m.environ...)
	for k, v := range p.DevEnv {
		cmd.Env = append(cmd.Env, k+"="+templates.Expand([]string{v}, devPort, proxyPort)[0])
	}
	cmd.Env = append(cmd.Env, "PORT="+strconv.Itoa(devPort))
	out := &tailBuffer{max: 16 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	procgroup.Prepare(cmd)
	if err := cmd.Start(); err != nil {
		ln.Close()
		return host.PreviewStatus{}, fmt.Errorf("start %s: %v", argv[0], err)
	}
	procgroup.Attach(cmd)
	s := &server{
		id: id, cmd: cmd, done: make(chan struct{}), devPort: devPort, ln: ln, out: out,
		origin: "http://127.0.0.1:" + strconv.Itoa(proxyPort), session: randomHex(24), status: "starting",
		grants: map[string]time.Time{},
	}
	s.http = &http.Server{Handler: s.handler(), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = s.http.Serve(ln) }()
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.status, s.ready = "exited", false
		if !s.stopping {
			s.err = strings.TrimSpace(fmt.Sprintf("dev server exited (%v)\n%s", err, out.String()))
		}
		s.mu.Unlock()
		close(s.done)
	}()
	go s.waitReady()
	m.projects[id] = s
	m.log(fmt.Sprintf("preview %s: %s (dev 127.0.0.1:%d, proxy %s)", id, strings.Join(argv, " "), devPort, s.origin))
	return s.snapshot(), nil
}

func (s *server) waitReady() {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(readyWait)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			return
		case <-time.After(300 * time.Millisecond):
		}
		res, err := client.Get("http://127.0.0.1:" + strconv.Itoa(s.devPort) + "/")
		if err != nil {
			continue
		}
		res.Body.Close()
		s.mu.Lock()
		if s.status == "starting" {
			s.status, s.ready = "running", true
		}
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	if s.status == "starting" {
		s.err = "the dev server did not answer within " + readyWait.String() + "\n" + s.out.String()
	}
	s.mu.Unlock()
}

// Grant mints a one-time URL for the project's preview.
func (m *Manager) Grant(id string) (string, time.Time, error) {
	m.mu.Lock()
	s := m.projects[id]
	m.mu.Unlock()
	if s == nil {
		return "", time.Time{}, errors.New("the preview is not running; press Play first")
	}
	tok := randomHex(24)
	exp := time.Now().Add(grantTTL)
	s.mu.Lock()
	for t, e := range s.grants {
		if time.Now().After(e) {
			delete(s.grants, t)
		}
	}
	s.grants[tok] = exp
	s.mu.Unlock()
	return s.origin + "/?" + GrantParam + "=" + tok, exp, nil
}

// Stop stops the project's dev server and proxy.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked(id)
}

func (m *Manager) stopLocked(id string) {
	s := m.projects[id]
	if s == nil {
		return
	}
	delete(m.projects, id)
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	procgroup.Stop(s.cmd, s.done, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.http.Shutdown(ctx)
	cancel()
	_ = s.http.Close()
}

// StopAll stops every dev server (app exit).
func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.projects))
	for id := range m.projects {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) { defer wg.Done(); m.Stop(id) }(id)
	}
	wg.Wait()
}

// takeGrant consumes a valid grant.
func (s *server) takeGrant(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.grants[tok]
	if !ok {
		return false
	}
	delete(s.grants, tok)
	return time.Now().Before(exp)
}

func (s *server) hostAllowed(h string) bool {
	u, _ := url.Parse(s.origin)
	port := u.Port()
	return h == "127.0.0.1:"+port || h == "localhost:"+port || h == "[::1]:"+port
}

func (s *server) sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "same-origin" {
		return true
	}
	for _, v := range []string{r.Header.Get("Origin"), r.Header.Get("Referer")} {
		if v == "" || v == "null" {
			continue
		}
		u, err := url.Parse(v)
		if err == nil && u.Scheme == "http" && s.hostAllowed(u.Host) {
			return true
		}
	}
	return false
}

func (s *server) hasCookie(r *http.Request) bool {
	for _, name := range []string{cookieName, cookieName + "_lax"} {
		if c, err := r.Cookie(name); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.session)) == 1 {
			return true
		}
	}
	return false
}

const deniedPage = `<!doctype html><meta charset="utf-8"><title>Preview locked</title>
<body style="font:16px system-ui;padding:32px;background:#111;color:#eee">
<h1 style="font-size:20px">This preview link has expired</h1>
<p>Open the preview again from VibeChat (Preview → Play, or “Open in new tab”).</p></body>`

func (s *server) handler() http.Handler {
	target, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(s.devPort))
	rp := httputil.NewSingleHostReverseProxy(target)
	base := rp.Director
	rp.Director = func(r *http.Request) {
		base(r)
		r.Host = target.Host
		r.Header.Del("Cookie")
		if o := r.Header.Get("Origin"); o != "" {
			r.Header.Set("Origin", target.Scheme+"://"+target.Host)
		}
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.Header().Set("retry-after", "1")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="1"><body style="font:16px system-ui;padding:32px;background:#111;color:#eee">Starting the dev server…</body>`))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		tok := q.Get(GrantParam)
		ok := s.takeGrant(tok)
		if ok {
			for _, c := range []*http.Cookie{
				{Name: cookieName, Value: s.session, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteNoneMode, Partitioned: true},
				{Name: cookieName + "_lax", Value: s.session, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode},
			} {
				http.SetCookie(w, c)
			}
		}
		if !ok && !s.hasCookie(r) && !s.sameOrigin(r) {
			w.Header().Set("content-type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(deniedPage))
			return
		}
		if tok != "" {
			q.Del(GrantParam)
			r.URL.RawQuery = q.Encode()
		}
		rp.ServeHTTP(w, r)
	})
}

// tailBuffer keeps the last max bytes written.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.buf.String()
	if i := strings.LastIndexByte(s, '\n'); i > 0 && len(s) > 4000 {
		s = s[len(s)-4000:]
	}
	return s
}
