package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/buildytech/vibechat-desktop/internal/auth"
)

const testToken = "test-host-token-0123456789abcdef"

// fakeServer is vibechat-server's HTTP surface with its real auth (auth.TokenAuth + auth.Middleware): public hello,
// Bearer → ticket → session cookie, the Studio page and an authenticated seam route.
func fakeServer(t *testing.T, minClient, previewPort int, legacy303 bool) *httptest.Server {
	t.Helper()
	ta := auth.NewTokenAuth(testToken)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /runtime/v1/host/hello", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"seam": "1", "serverVersion": "0.1.4", "instanceId": "inst-0123456789abcdef", "protocol": 1, "minClientProtocol": minClient, "previewPort": previewPort, "capabilities": map[string]any{}})
	})
	mux.HandleFunc("POST /runtime/v1/auth/ticket", func(w http.ResponseWriter, r *http.Request) {
		tk, exp, err := ta.IssueTicket(r)
		if err != nil {
			writeJSON(w, 401, map[string]string{"error": "no"})
			return
		}
		writeJSON(w, 200, map[string]string{"ticket": tk, "expiresAt": exp.Format(time.RFC3339)})
	})
	mux.HandleFunc("GET /runtime/v1/projects", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, []map[string]any{{"id": "abc123", "preview_url": "http://abc123.preview.localhost:" + strconv.Itoa(previewPort) + "/"}})
	})
	mux.HandleFunc("GET /runtime/v1/host/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"seam": "1"})
	})
	mux.HandleFunc("GET /runtime/v1/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "data: two\n\n")
	})
	mux.Handle("/auth/session", ta.SessionHandler())
	if legacy303 {
		// A server before 0.1.6: the cookie, then 303 → "/".
		mux = withLegacySession(mux, ta.SessionHandler())
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = io.WriteString(w, "<!doctype html><title>server studio</title>")
	})
	public := func(r *http.Request) bool {
		return (r.URL.Path == "/runtime/v1/host/hello" && r.Method == http.MethodGet) || r.URL.Path == "/auth/session"
	}
	srv := httptest.NewServer(auth.Middleware(ta, "/runtime/v1", public, mux))
	t.Cleanup(srv.Close)
	return srv
}

func withLegacySession(mux *http.ServeMux, session http.Handler) *http.ServeMux {
	out := http.NewServeMux()
	out.Handle("/", mux)
	out.HandleFunc("/auth/session", func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		session.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			w.WriteHeader(rec.Code)
			return
		}
		for _, c := range rec.Result().Cookies() {
			http.SetCookie(w, c)
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	return out
}

// freePort is a TCP port free on 127.0.0.1 right now.
func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type rig struct {
	t        *testing.T
	sshd     *testSSHD
	creds    *MemCreds
	m        *Manager
	nav      chan string
	data     string
	preview  int
	previewS *httptest.Server
}

func newRig(t *testing.T, minClient int) *rig { return newRigWith(t, minClient, false) }

// perProject: the next manager uses the per-project preview ports (the WKWebView fallback).
var perProject bool

func newRigWith(t *testing.T, minClient int, legacy303 bool) *rig {
	t.Helper()
	preview := freePort(t)
	previewS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "preview gateway saw Host %s Origin %s", r.Host, r.Header.Get("Origin"))
	}))
	t.Cleanup(previewS.Close)
	api := fakeServer(t, minClient, preview, legacy303)
	sshd := startSSHD(t, "127.0.0.1:0", newHostKey(t), map[string]string{
		ServerAPI:                            strings.TrimPrefix(api.URL, "http://"),
		"127.0.0.1:" + strconv.Itoa(preview): strings.TrimPrefix(previewS.URL, "http://"),
	})
	r := &rig{t: t, sshd: sshd, creds: &MemCreds{}, nav: make(chan string, 4), data: t.TempDir(), preview: preview, previewS: previewS}
	r.m = r.manager()
	t.Cleanup(r.m.Close)
	// The owner installs the app's public key for the tunnel user.
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(r.m.Connection().SSHPublicKey))
	if err != nil {
		t.Fatal(err)
	}
	sshd.authorize(pub)
	return r
}

func (r *rig) manager() *Manager {
	m, err := New(Options{
		DataDir: r.data, Creds: r.creds, AppVersion: "0.1.5", AppProtocol: 1, MinServerProtocol: 1,
		Prefix: "/runtime/v1", SessionPath: "/auth/session", TicketParam: "ticket",
		Navigate:              func(u string) { r.nav <- u },
		PreviewPortPerProject: perProject,
		Log:                   func(s string) { r.t.Log(s) },
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return m
}

func (r *rig) connect(token string) Connection {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return r.m.Connect(ctx, ConnectInput{Address: r.sshd.addr(), User: "vibechat-tunnel", Token: token})
}

func (r *rig) navigated() string {
	select {
	case u := <-r.nav:
		return u
	case <-time.After(5 * time.Second):
		r.t.Fatal("the window was not navigated")
		return ""
	}
}

// signIn opens the window's sign-in URL like the webview: the page that moves on to "/" by itself (never a 303,
// whose cross-site initiator would drop the SameSite=Strict cookie), then "/" with the cookie.
func signIn(t *testing.T, c *http.Client, u, origin string) {
	t.Helper()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer func() { c.CheckRedirect = nil }()
	res, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(page), auth.SignedInHTML) {
		t.Fatalf("sign-in page: %d %q", res.StatusCode, page)
	}
	res, err = c.Get(origin + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(strings.ToLower(string(page)), "<!doctype html") || strings.Contains(string(page), "refresh") {
		t.Fatalf("Studio after sign-in: %d %q", res.StatusCode, page)
	}
}

func browser(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func getJSON(t *testing.T, c *http.Client, method, u string, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, u, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// TestTicketFlowThroughTheTunnel: Connect over SSH, Bearer → ticket on the Go side, the window's
// /auth/session?ticket=… through the local proxy sets the server's session cookie, then the Studio, the seam, SSE and
// the desktop routes work through the proxy, and the preview port answers on the same port locally.
func TestTicketFlowThroughTheTunnel(t *testing.T) {
	r := newRig(t, 1)
	conn := r.connect(testToken)
	if conn.Problem != nil || conn.Mode != "remote" {
		t.Fatalf("connect: %+v", conn)
	}
	if conn.Server == nil || conn.Server.Version != "0.1.4" || !conn.ServerOutdated {
		t.Fatalf("server view: %+v outdated=%v", conn.Server, conn.ServerOutdated)
	}
	if conn.Target == nil || conn.Target.HostKeyFingerprint != ssh.FingerprintSHA256(r.sshd.hostKey.PublicKey()) || !conn.Target.TokenSaved {
		t.Fatalf("target: %+v", conn.Target)
	}
	u := r.navigated()
	if !strings.HasPrefix(u, "http://127.0.0.1:") || !strings.Contains(u, "/auth/session?ticket=") || strings.Contains(u, testToken) {
		t.Fatalf("window URL %q", u)
	}
	origin := u[:strings.Index(u, "/auth/")]
	c := browser(t)
	signIn(t, c, u, origin)
	// A second use of the ticket fails (one-time).
	if res, err := http.Get(u); err != nil || res.StatusCode != 401 {
		t.Fatalf("ticket reuse: %v %v", res.StatusCode, err)
	}
	if code, _ := getJSON(t, c, "GET", origin+"/runtime/v1/host/capabilities", nil); code != 200 {
		t.Fatalf("capabilities with the session: %d", code)
	}
	if code, _ := getJSON(t, http.DefaultClient, "GET", origin+"/runtime/v1/host/capabilities", nil); code != 401 {
		t.Fatalf("capabilities without the session: %d", code)
	}
	code, body := getJSON(t, c, "GET", origin+"/runtime/v1/desktop/connection", nil)
	if code != 200 || body["mode"] != "remote" {
		t.Fatalf("desktop connection through the proxy: %d %v", code, body)
	}
	if code, _ := getJSON(t, http.DefaultClient, "GET", origin+"/runtime/v1/desktop/connection", nil); code != 401 {
		t.Fatalf("desktop route without the session: %d", code)
	}
	if code, _ := getJSON(t, c, "GET", origin+"/runtime/v1/desktop/connection", map[string]string{"Origin": "http://evil.example"}); code != 403 {
		t.Fatalf("cross-origin desktop route: %d", code)
	}
	// SSE is streamed, not buffered.
	res, err := c.Get(origin + "/runtime/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(res.Body).ReadString('\n')
	res.Body.Close()
	if line != "data: one\n" {
		t.Fatalf("sse first line %q", line)
	}
	// Preview: http://<id>.preview.localhost:<port> on this machine reaches the gateway with its Host.
	pc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(r.preview))
	}}}
	res, err = pc.Get("http://abc.preview.localhost:" + strconv.Itoa(r.preview) + "/")
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if want := "Host abc.preview.localhost:" + strconv.Itoa(r.preview); !strings.Contains(string(pb), want) {
		t.Fatalf("preview: %q, want %q", pb, want)
	}
	// Nothing secret on disk: remote.json has the address and the pin only.
	raw, _ := os.ReadFile(filepath.Join(r.data, "remote.json"))
	if strings.Contains(string(raw), testToken) || strings.Contains(string(raw), "PRIVATE KEY") || !strings.Contains(string(raw), `"active": true`) {
		t.Fatalf("remote.json: %s", raw)
	}
	if st, _ := os.Stat(filepath.Join(r.data, "remote.json")); st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("remote.json mode %v", st.Mode())
	}
	if tok, _ := r.creds.Get(CredHostToken); tok != testToken {
		t.Fatal("host token not in the keychain")
	}

	// Disconnect: the window goes home, the proxy closes, the server stays saved but not active.
	c2 := r.m.Disconnect(false)
	if c2.Mode != "local" || c2.Target == nil {
		t.Fatalf("disconnect: %+v", c2)
	}
	if u := r.navigated(); u != "/" {
		t.Fatalf("navigate home: %q", u)
	}
	time.Sleep(2 * time.Second)
	if _, err := http.Get(origin + "/"); err == nil {
		t.Fatal("proxy still answers after disconnect")
	}
	// Reconnect with the token from the keychain, same pin, same local port.
	conn = r.connect("")
	if conn.Problem != nil || conn.Mode != "remote" {
		t.Fatalf("reconnect: %+v", conn.Problem)
	}
	if u := r.navigated(); !strings.HasPrefix(u, origin+"/") {
		t.Fatalf("reconnect origin %q, want %s", u, origin)
	}
	// Forget: no saved server, no token.
	c3 := r.m.Disconnect(true)
	if c3.Target != nil {
		t.Fatalf("forget: %+v", c3)
	}
	if _, err := r.creds.Get(CredHostToken); !errors.Is(err, ErrNoCred) {
		t.Fatal("token still in the keychain after forget")
	}
}

// TestHostKeyPinning: the first connect pins the host key (TOFU); a server with another key at that address is
// refused (host_key_changed) and the window does not move; Forget lets the owner trust the new key.
func TestHostKeyPinning(t *testing.T) {
	r := newRig(t, 1)
	if c := r.connect(testToken); c.Problem != nil {
		t.Fatal(c.Problem)
	}
	r.navigated()
	addr := r.sshd.addr()
	permit := r.sshd.permit
	pinned := r.m.Connection().Target.HostKeyFingerprint
	r.m.Disconnect(false)
	r.navigated()
	r.sshd.close()

	impostor := startSSHD(t, addr, newHostKey(t), permit)
	pub, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(r.m.Connection().SSHPublicKey))
	impostor.authorize(pub)
	c := r.connect("")
	if c.Problem == nil || c.Problem.Code != ProblemHostKeyChanged || c.Mode != "local" {
		t.Fatalf("impostor accepted: %+v", c)
	}
	if c.Target.HostKeyFingerprint != pinned {
		t.Fatal("the pin changed")
	}
	select {
	case u := <-r.nav:
		t.Fatalf("navigated to %s", u)
	default:
	}
	// A restart reads the pin from remote.json: still strict.
	r.m = r.manager()
	if c := r.connect(testToken); c.Problem == nil || c.Problem.Code != ProblemHostKeyChanged {
		t.Fatalf("after restart: %+v", c.Problem)
	}
	r.m.Disconnect(true)
	if c := r.connect(testToken); c.Problem != nil {
		t.Fatalf("after forget: %+v", c.Problem)
	}
	r.navigated()
	if r.m.Connection().Target.HostKeyFingerprint == pinned {
		t.Fatal("new key not pinned")
	}
}

func TestPinCallback(t *testing.T) {
	a, b := newHostKey(t).PublicKey(), newHostKey(t).PublicKey()
	var seen ssh.PublicKey
	if err := pinCallback(nil, &seen)("h", nil, a); err != nil || seen == nil || string(seen.Marshal()) != string(a.Marshal()) {
		t.Fatalf("tofu: %v", err)
	}
	if err := pinCallback(a, &seen)("h", nil, a); err != nil {
		t.Fatalf("same key: %v", err)
	}
	if err := pinCallback(a, &seen)("h", nil, b); !errors.Is(err, ErrHostKeyChanged) {
		t.Fatalf("other key: %v", err)
	}
	p, err := parsePin(marshalPin(a))
	if err != nil || string(p.Marshal()) != string(a.Marshal()) {
		t.Fatal("pin round trip")
	}
}

// TestKeychain: the SSH key is made once and kept in the keychain (never in the data folder); a keychain failure
// is reported, not papered over with a file.
func TestKeychain(t *testing.T) {
	c := &MemCreds{}
	s1, err := ensureSigner(c)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := ensureSigner(c)
	if err != nil || string(s1.PublicKey().Marshal()) != string(s2.PublicKey().Marshal()) {
		t.Fatal("key not reused")
	}
	if raw, _ := c.Get(CredSSHKey); !strings.Contains(raw, "OPENSSH PRIVATE KEY") {
		t.Fatal("not an OpenSSH key")
	}
	if line := authorizedLine(s1.PublicKey()); !strings.HasPrefix(line, "ssh-ed25519 ") || !strings.HasSuffix(line, " vibechat-desktop") {
		t.Fatalf("line %q", line)
	}
	bad := &MemCreds{Fail: errors.New("locked")}
	if _, err := ensureSigner(bad); err == nil {
		t.Fatal("locked keychain accepted")
	}
	dir := t.TempDir()
	m, _ := New(Options{DataDir: dir, Creds: bad, AppVersion: "0.1.5", AppProtocol: 1, MinServerProtocol: 1, Prefix: "/runtime/v1"})
	if c := m.Connection(); c.SSHPublicKey != "" {
		t.Fatal("key without a keychain")
	}
	if c := m.Connect(context.Background(), ConnectInput{Address: "127.0.0.1:1", User: "u", Token: "t"}); c.Problem == nil || c.Problem.Code != ProblemAuth {
		t.Fatalf("locked keychain connect: %+v", c.Problem)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		raw, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if strings.Contains(string(raw), "PRIVATE KEY") {
			t.Fatalf("private key on disk in %s", e.Name())
		}
	}
}

func TestProblems(t *testing.T) {
	r := newRig(t, 1)
	if c := r.connect("wrong-token"); c.Problem == nil || c.Problem.Code != ProblemAuth {
		t.Fatalf("wrong token: %+v", c.Problem)
	}
	if _, err := r.creds.Get(CredHostToken); !errors.Is(err, ErrNoCred) {
		t.Fatal("a refused token was saved")
	}
	ctx := context.Background()
	if c := r.m.Connect(ctx, ConnectInput{Address: r.sshd.addr(), User: "Bad User", Token: "x"}); c.Problem == nil || c.Problem.Code != ProblemBadInput {
		t.Fatalf("bad user: %+v", c.Problem)
	}
	if c := r.m.Connect(ctx, ConnectInput{Address: "127.0.0.1:" + strconv.Itoa(freePort(t)), User: "vibechat-tunnel", Token: "x"}); c.Problem == nil || c.Problem.Code != ProblemUnreachable {
		t.Fatalf("closed port: %+v", c.Problem)
	}
	// An SSH key the server does not know.
	other := newRig(t, 1)
	r2 := &rig{t: t, sshd: other.sshd, creds: &MemCreds{}, nav: make(chan string, 4), data: t.TempDir()}
	r2.m = r2.manager()
	if c := r2.connect(testToken); c.Problem == nil || c.Problem.Code != ProblemAuth {
		t.Fatalf("unknown ssh key: %+v", c.Problem)
	}
	// The preview port taken on this machine.
	busy := newRig(t, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(busy.preview))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if c := busy.connect(testToken); c.Problem == nil || c.Problem.Code != ProblemPortBusy || c.Mode != "local" {
		t.Fatalf("busy preview port: %+v", c.Problem)
	}
}

// TestProtocolMismatch: a server that needs a newer app is protocol_app_old (the Studio shows the update screen);
// the server stays active so the next start checks again.
func TestProtocolMismatch(t *testing.T) {
	r := newRig(t, 2)
	c := r.connect(testToken)
	if c.Problem == nil || c.Problem.Code != ProblemAppOld || c.Mode != "local" || c.Server == nil || c.Server.MinClientProtocol != 2 {
		t.Fatalf("%+v", c)
	}
	if !r.m.target.Active {
		t.Fatal("not active after a protocol mismatch")
	}
	if c := r.m.Disconnect(false); c.Problem != nil || r.m.target.Active {
		t.Fatalf("use this computer: %+v", c)
	}
}

// TestReconnectAfterDrop: when the SSH connection drops, the next request redials (same pin) transparently.
func TestReconnectAfterDrop(t *testing.T) {
	r := newRig(t, 1)
	if c := r.connect(testToken); c.Problem != nil {
		t.Fatal(c.Problem)
	}
	u := r.navigated()
	origin := u[:strings.Index(u, "/auth/")]
	c := browser(t)
	signIn(t, c, u, origin)
	r.sshd.dropAll()
	time.Sleep(200 * time.Millisecond)
	if code, _ := getJSON(t, c, "GET", origin+"/runtime/v1/host/capabilities", nil); code != 200 {
		t.Fatalf("after drop: %d", code)
	}
}

func TestLocalMiddleware(t *testing.T) {
	m, _ := New(Options{DataDir: t.TempDir(), Creds: &MemCreds{}, AppVersion: "0.1.5", AppProtocol: 1, MinServerProtocol: 1, Prefix: "/runtime/v1"})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(299) })
	h := m.Middleware(next)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/runtime/v1/desktop/connection", nil))
	var c Connection
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil || c.Mode != "local" || !strings.HasPrefix(c.SSHPublicKey, "ssh-ed25519 ") || c.AppProtocol != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/runtime/v1/projects", nil))
	if rec.Code != 299 {
		t.Fatal("other routes not passed on")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("PUT", "/runtime/v1/desktop/connection", strings.NewReader("{")))
	if rec.Code != 400 {
		t.Fatalf("bad json: %d", rec.Code)
	}
}

func TestVersionsAndAddresses(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		less bool
	}{{"0.1.4", "0.1.5", true}, {"0.1.5", "0.1.5", false}, {"v0.2.0", "0.1.9", false}, {"0.1.5-rc1", "0.1.10", true}, {"dev", "0.1.5", false}} {
		if versionLess(tc.a, tc.b) != tc.less {
			t.Errorf("%s < %s", tc.a, tc.b)
		}
	}
	for in, want := range map[string]string{"example.com": "example.com:22", "10.0.0.2:2222": "10.0.0.2:2222", "[::1]:22": "[::1]:22", "::1": "[::1]:22"} {
		if got, err := sshAddr(in); err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "host:0", "host:99999", "a b", "user@host"} {
		if _, err := sshAddr(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestLegacySessionRedirect: a server that still answers the sign-in with 303 → "/" gets the page from the proxy.
func TestLegacySessionRedirect(t *testing.T) {
	r := newRigWith(t, 1, true)
	if c := r.connect(testToken); c.Problem != nil {
		t.Fatal(c.Problem)
	}
	u := r.navigated()
	signIn(t, browser(t), u, u[:strings.Index(u, "/auth/")])
}

// TestPreviewPortPerProject: the WKWebView fallback. The project list's preview URL becomes http://127.0.0.1:<P>;
// that origin reaches the gateway as <id>.preview.localhost:<gw> with Origin mapped; the local gateway port maps a
// request from http://127.0.0.1:<P> (Vite's HMR socket) to the same project.
func TestPreviewPortPerProject(t *testing.T) {
	perProject = true
	r := newRig(t, 1)
	perProject = false
	if c := r.connect(testToken); c.Problem != nil {
		t.Fatal(c.Problem)
	}
	u := r.navigated()
	origin := u[:strings.Index(u, "/auth/")]
	c := browser(t)
	signIn(t, c, u, origin)
	res, err := c.Get(origin + "/runtime/v1/projects")
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]string
	_ = json.NewDecoder(res.Body).Decode(&list)
	res.Body.Close()
	if len(list) != 1 || !strings.HasPrefix(list[0]["preview_url"], "http://127.0.0.1:") || !strings.HasSuffix(list[0]["preview_url"], "/") {
		t.Fatalf("projects: %v", list)
	}
	local := strings.TrimSuffix(list[0]["preview_url"], "/")
	gw := "abc123.preview.localhost:" + strconv.Itoa(r.preview)
	get := func(u, origin string) string {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Origin", origin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	want := "Host " + gw + " Origin http://" + gw
	if got := get(local+"/", local); !strings.Contains(got, want) {
		t.Fatalf("per-project origin: %q, want %q", got, want)
	}
	if got := get("http://127.0.0.1:"+strconv.Itoa(r.preview)+"/hmr", local); !strings.Contains(got, want) {
		t.Fatalf("HMR via the gateway port: %q, want %q", got, want)
	}
	if got := get("http://127.0.0.1:"+strconv.Itoa(r.preview)+"/", "http://127.0.0.1:1"); !strings.Contains(got, "no preview here") {
		t.Fatalf("unknown origin on the gateway port: %q", got)
	}
	// The same list again: the same local origin.
	res, _ = c.Get(origin + "/runtime/v1/projects")
	_ = json.NewDecoder(res.Body).Decode(&list)
	res.Body.Close()
	if strings.TrimSuffix(list[0]["preview_url"], "/") != local {
		t.Fatalf("origin changed: %v", list)
	}
}
