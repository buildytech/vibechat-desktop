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
//
// Gateway mode (vibechat-server, NewGateway): no port per project; one listener on the gateway port routes by Host,
// `<projectId>.preview.localhost:<port>` (project ids are hex, valid DNS labels; *.localhost resolves to loopback
// without DNS), so one tunnel port carries every preview. Grants, cookies and the same-origin rule are unchanged; the
// HMR client port is the gateway port.
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
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/host"
	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
	"github.com/buildytech/vibechat-desktop/server/internal/templates"
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

	// gatewayPort > 0: gateway mode (one Host-routed listener, see Gateway).
	gatewayPort int

	mu       sync.Mutex
	projects map[string]*server
}

// GatewayDomain is the host suffix of gateway previews.
const GatewayDomain = ".preview.localhost"

// NewGateway is a Manager in gateway mode: previews are http://<projectId>.preview.localhost:<port>/ and Gateway
// serves them; the caller listens on 127.0.0.1:<port>.
func NewGateway(log func(string), port int) *Manager {
	m := New(log)
	m.gatewayPort = port
	return m
}

// Gateway routes a request to the preview its Host names (gateway mode); unknown or stopped projects get 404.
func (m *Manager) Gateway() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := m.gatewayID(r.Host)
		var s *server
		if ok {
			m.mu.Lock()
			s = m.projects[id]
			m.mu.Unlock()
		}
		if s == nil {
			w.Header().Set("content-type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(notRunningPage))
			return
		}
		s.handle.ServeHTTP(w, r)
	})
}

// gatewayID is the project id of "<id>.preview.localhost:<gatewayPort>".
func (m *Manager) gatewayID(hostport string) (string, bool) {
	h, port, err := net.SplitHostPort(hostport)
	if err != nil || port != strconv.Itoa(m.gatewayPort) {
		return "", false
	}
	id, ok := strings.CutSuffix(strings.ToLower(h), GatewayDomain)
	if !ok || id == "" || len(id) > 63 {
		return "", false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return id, true
}

// New returns a Manager. log may be nil.
func New(log func(string)) *Manager {
	if log == nil {
		log = func(string) {}
	}
	return &Manager{log: log, projects: map[string]*server{}}
}

type server struct {
	id       string
	cmd      *exec.Cmd
	done     chan struct{}
	devPort  int
	ln       net.Listener // nil in gateway mode
	http     *http.Server // nil in gateway mode
	handle   http.Handler
	origin   string // http://127.0.0.1:<proxy port>, or http://<id>.preview.localhost:<gateway port>
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
	var ln net.Listener
	proxyPort := m.gatewayPort
	origin := "http://" + id + GatewayDomain + ":" + strconv.Itoa(proxyPort)
	if m.gatewayPort == 0 {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return host.PreviewStatus{}, err
		}
		proxyPort = ln.Addr().(*net.TCPAddr).Port
		origin = "http://127.0.0.1:" + strconv.Itoa(proxyPort)
	}
	closeLn := func() {
		if ln != nil {
			ln.Close()
		}
	}
	argv := templates.Expand(p.Dev, devPort, proxyPort)
	var extra []string
	for k, v := range p.DevEnv {
		extra = append(extra, k+"="+templates.Expand([]string{v}, devPort, proxyPort)[0])
	}
	extra = append(extra, "PORT="+strconv.Itoa(devPort))
	// The dev server runs the project's vite.config (code the agent can write): isolated from the server (package
	// isolate), like the install.
	full, env, mode, err := isolate.Command(argv, isolate.Spec{Dir: dir, Env: append(append([]string{}, p.Env...), extra...), Writable: p.Writable})
	if err != nil {
		closeLn()
		return host.PreviewStatus{}, err
	}
	cmd := procgroup.Command(full[0], full[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	out := &tailBuffer{max: 16 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	procgroup.Prepare(cmd)
	if err := cmd.Start(); err != nil {
		closeLn()
		return host.PreviewStatus{}, fmt.Errorf("start %s: %v", argv[0], err)
	}
	procgroup.Attach(cmd)
	s := &server{
		id: id, cmd: cmd, done: make(chan struct{}), devPort: devPort, ln: ln, out: out,
		origin: origin, session: randomHex(24), status: "starting",
		grants: map[string]time.Time{},
	}
	s.handle = s.handler()
	if ln != nil {
		s.http = &http.Server{Handler: s.handle, ReadHeaderTimeout: 30 * time.Second}
		go func() { _ = s.http.Serve(ln) }()
	}
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
	m.log(fmt.Sprintf("preview %s: %s (%s, dev 127.0.0.1:%d, proxy %s)", id, strings.Join(argv, " "), mode, devPort, s.origin))
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
	if s.http == nil {
		return
	}
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
	if strings.HasSuffix(u.Hostname(), GatewayDomain) {
		return strings.EqualFold(h, u.Host)
	}
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

const notRunningPage = `<!doctype html><meta charset="utf-8"><title>No preview</title>
<body style="font:16px system-ui;padding:32px;background:#111;color:#eee">
<h1 style="font-size:20px">This preview is not running</h1>
<p>Start it from VibeChat (Preview → Play).</p></body>`

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
