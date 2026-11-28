// Package remote connects the desktop app to the owner's vibechat-server over SSH built into the app
// (golang.org/x/crypto/ssh): the app's ed25519 key and the host token live in the OS keychain, the server's host key
// is pinned on the first connect (trust on first use) and checked strictly afterwards. The Go side buys a one-time
// ticket with the host token (Bearer, never in the webview) and moves the window to http://127.0.0.1:<L> (a local
// reverse proxy through the tunnel) at /auth/session?ticket=…; the preview gateway's port is forwarded as the same
// port on 127.0.0.1 and [::1], so http://<id>.preview.localhost:<port> reaches it.
//
// The Studio's GET/PUT/DELETE /runtime/v1/desktop/connection (HOST_DESKTOP_ROUTES) are answered here in both modes:
// by Middleware in the app's own origin, by the proxy in the server's. A vibechat-server alone answers 404.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/buildytech/vibechat-desktop/protocol/auth"
)

// Problem codes (DesktopProblemCodeSchema).
const (
	ProblemAppOld         = "protocol_app_old"
	ProblemServerOld      = "protocol_server_old"
	ProblemHostKeyChanged = "host_key_changed"
	ProblemUnreachable    = "unreachable"
	ProblemAuth           = "auth"
	ProblemPortBusy       = "port_busy"
	ProblemBadInput       = "bad_input"
)

// Options configure a Manager.
type Options struct {
	// DataDir holds remote.json (the saved server, no secrets).
	DataDir string
	Creds   Creds
	// AppVersion and AppProtocol are the app's (appid.Version, host-seam.json protocol); MinServerProtocol is the
	// oldest server protocol this app speaks.
	AppVersion        string
	AppProtocol       int
	MinServerProtocol int
	Prefix            string // "/runtime/v1"
	SessionPath       string // "/auth/session"
	TicketParam       string // "ticket"
	// PreviewPortPerProject: the webview does not resolve *.preview.localhost (WKWebView): every preview gets a
	// local port of its own (previewports.go).
	PreviewPortPerProject bool
	// Navigate moves the window ("http://127.0.0.1:L/auth/session?ticket=…", or "/" back to the start screen).
	// It is called on its own goroutine.
	Navigate func(url string)
	Log      func(string)
	// Provision configures the setup wizard of a new server (provision.go).
	Provision ProvisionOptions
	// SaveAs asks where to save a file named name (the system's Save dialog; archive.go): the chosen path, or "" when
	// the user closed the dialog. nil: the app cannot save files (POST /desktop/archive answers 501).
	SaveAs func(name string) (string, error)
}

// Problem is DesktopConnection.problem.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// TargetView is DesktopConnection.target.
type TargetView struct {
	Address            string `json:"address"`
	User               string `json:"user"`
	HostKeyFingerprint string `json:"hostKeyFingerprint"`
	TokenSaved         bool   `json:"tokenSaved"`
}

// ServerView is DesktopConnection.server.
type ServerView struct {
	Version           string `json:"version"`
	Protocol          int    `json:"protocol"`
	MinClientProtocol int    `json:"minClientProtocol"`
	InstanceID        string `json:"instanceId"`
}

// Connection is DesktopConnectionSchema.
type Connection struct {
	Mode           string      `json:"mode"`
	Connecting     bool        `json:"connecting,omitempty"`
	AppVersion     string      `json:"appVersion"`
	AppProtocol    int         `json:"appProtocol"`
	SSHPublicKey   string      `json:"sshPublicKey"`
	Target         *TargetView `json:"target,omitempty"`
	Server         *ServerView `json:"server,omitempty"`
	ServerOutdated bool        `json:"serverOutdated,omitempty"`
	Problem        *Problem    `json:"problem,omitempty"`
}

// ConnectInput is DesktopConnectInputSchema.
type ConnectInput struct {
	Address string `json:"address"`
	User    string `json:"user"`
	Token   string `json:"token,omitempty"`
}

// Manager owns the connection state. Connect and Disconnect are serialised.
type Manager struct {
	o    Options
	file string
	op   sync.Mutex // one Connect / Disconnect at a time

	mu       sync.Mutex
	target   *Target
	sess     *session
	origin   string // http://127.0.0.1:L while remote
	server   *ServerView
	outdated bool
	problem  *Problem
	pub      string
	closing  chan struct{} // closed when the last session's delayed close is done
	// connecting: a Connect or Resume is in progress (the start screen shows "Connecting…").
	connecting bool

	prov        *Provision // the setup wizard's current or last run
	provRunning bool
	// provisionConfirm replaces the lockdown confirmation (tests: a missing confirmation).
	provisionConfirm func(context.Context) error
}

// New loads the saved server.
func New(o Options) (*Manager, error) {
	if o.Log == nil {
		o.Log = func(string) {}
	}
	if o.Navigate == nil {
		o.Navigate = func(string) {}
	}
	m := &Manager{o: o, file: filepath.Join(o.DataDir, "remote.json")}
	t, err := loadTarget(m.file)
	if err != nil {
		return nil, err
	}
	m.target = t
	return m, nil
}

// DesktopPath is the seam path of the connection routes.
func (m *Manager) DesktopPath() string { return m.o.Prefix + "/desktop/connection" }

// Connection answers the current state (and makes the app's SSH key on first use, so Settings can show it).
func (m *Manager) Connection() Connection {
	pub := m.publicKey()
	m.mu.Lock()
	defer m.mu.Unlock()
	c := Connection{Mode: "local", AppVersion: m.o.AppVersion, AppProtocol: m.o.AppProtocol, SSHPublicKey: pub, Server: m.server, ServerOutdated: m.outdated, Problem: m.problem, Connecting: m.connecting}
	if m.sess != nil {
		c.Mode = "remote"
	}
	if t := m.target; t != nil {
		_, err := m.o.Creds.Get(CredHostToken)
		c.Target = &TargetView{Address: t.Address, User: t.User, HostKeyFingerprint: fingerprint(t.HostKey), TokenSaved: err == nil}
	}
	return c
}

func (m *Manager) publicKey() string {
	m.mu.Lock()
	pub := m.pub
	m.mu.Unlock()
	if pub != "" {
		return pub
	}
	s, err := ensureSigner(m.o.Creds)
	if err != nil {
		m.o.Log("remote: ssh key: " + err.Error())
		return ""
	}
	pub = authorizedLine(s.PublicKey())
	m.mu.Lock()
	m.pub = pub
	m.mu.Unlock()
	return pub
}

func (m *Manager) setProblem(code, msg string) {
	m.mu.Lock()
	m.problem = &Problem{Code: code, Message: msg}
	m.mu.Unlock()
	m.o.Log(fmt.Sprintf("remote: %s: %s", code, msg))
}

// Connect reaches the server in, pins its host key on the first connect, checks the protocol, buys a ticket and
// moves the window there. On failure the window stays on the start screen and the answer carries the problem.
func (m *Manager) Connect(ctx context.Context, in ConnectInput) Connection {
	m.op.Lock()
	defer m.op.Unlock()
	m.connect(ctx, in, false)
	return m.Connection()
}

// StartResume marks the start-up reconnect at once (the start screen shows "Connecting…" instead of the choice)
// and runs Resume in the background.
func (m *Manager) StartResume(ctx context.Context) {
	m.mu.Lock()
	active := m.target != nil && m.target.Active
	if active {
		m.connecting = true
	}
	m.mu.Unlock()
	if !active {
		return
	}
	go m.Resume(ctx)
}

// Resume connects to the saved server at start when it is active (the token from the keychain).
func (m *Manager) Resume(ctx context.Context) {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	t := m.target
	m.mu.Unlock()
	if t == nil || !t.Active {
		m.mu.Lock()
		m.connecting = false
		m.mu.Unlock()
		return
	}
	m.connect(ctx, ConnectInput{Address: t.Address, User: t.User}, true)
}

func (m *Manager) connect(ctx context.Context, in ConnectInput, resume bool) {
	addr, user, token := strings.TrimSpace(in.Address), strings.TrimSpace(in.User), strings.TrimSpace(in.Token)
	m.mu.Lock()
	old := m.target
	m.server, m.outdated, m.problem = nil, false, nil
	m.connecting = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.connecting = false
		m.mu.Unlock()
	}()
	hostport, err := sshAddr(addr)
	if err == nil {
		err = checkUser(user)
	}
	if err == nil && len(token) > 256 {
		err = errors.New("the host token is too long")
	}
	if err != nil {
		m.setProblem(ProblemBadInput, err.Error())
		return
	}
	next := &Target{Address: addr, User: user}
	same := old != nil && old.Address == addr && old.User == user
	if same {
		next.HostKey, next.ProxyPort = old.HostKey, old.ProxyPort
	}
	if token == "" {
		saved, err := m.o.Creds.Get(CredHostToken)
		if err != nil || old == nil || old.Address != addr {
			m.setProblem(ProblemBadInput, "paste the host token (sudo -u vibechat vibechat-server token new on the server)")
			return
		}
		token = strings.TrimSpace(saved)
	}
	signer, err := ensureSigner(m.o.Creds)
	if err != nil {
		m.setProblem(ProblemAuth, err.Error())
		return
	}
	pin, err := parsePin(next.HostKey)
	if err != nil {
		next.HostKey, pin = "", nil
	}
	m.stop(false) // leaving the current server, if any
	m.settle()

	var seen ssh.PublicKey
	client, err := dialSSH(ctx, hostport, user, signer, pin, &seen)
	if err != nil {
		switch {
		case errors.Is(err, ErrHostKeyChanged):
			m.setProblem(ProblemHostKeyChanged, err.Error())
		case strings.Contains(err.Error(), "unable to authenticate"):
			m.setProblem(ProblemAuth, fmt.Sprintf("the server did not accept this app's SSH key for %s: %v", user, err))
		default:
			m.setProblem(ProblemUnreachable, err.Error())
		}
		m.keepTarget(old, next)
		return
	}
	if pin == nil {
		next.HostKey = marshalPin(seen) // trust on first use; strict from now on
		m.o.Log("remote: pinned the host key of " + addr + ": " + ssh.FingerprintSHA256(seen))
	}
	s := newSession(hostport, user, signer, seen, client, m.o.Log)
	fail := func(code, msg string, active bool) {
		s.close()
		m.setProblem(code, msg)
		next.Active = active || (resume && old != nil && old.Active)
		m.saveTarget(next)
	}
	hello, err := s.hello(ctx, m.o.Prefix)
	if err != nil {
		fail(ProblemUnreachable, "the SSH tunnel works, vibechat-server does not answer on "+ServerAPI+": "+err.Error(), false)
		return
	}
	sv := &ServerView{Version: hello.ServerVersion, Protocol: hello.Protocol, MinClientProtocol: hello.MinClientProtocol, InstanceID: hello.InstanceID}
	m.mu.Lock()
	m.server, m.outdated = sv, versionLess(hello.ServerVersion, m.o.AppVersion)
	m.mu.Unlock()
	if hello.MinClientProtocol > m.o.AppProtocol {
		m.saveToken(in.Token)
		fail(ProblemAppOld, fmt.Sprintf("the server %s needs app protocol %d, this app speaks %d", hello.ServerVersion, hello.MinClientProtocol, m.o.AppProtocol), true)
		return
	}
	if hello.Protocol < m.o.MinServerProtocol {
		m.saveToken(in.Token)
		fail(ProblemServerOld, fmt.Sprintf("the server %s speaks protocol %d, this app needs %d", hello.ServerVersion, hello.Protocol, m.o.MinServerProtocol), true)
		return
	}
	ticket, err := s.ticket(ctx, m.o.Prefix, token)
	if err != nil {
		code := ProblemUnreachable
		if errors.Is(err, errTokenRefused) {
			code = ProblemAuth
		}
		fail(code, err.Error(), false)
		return
	}
	if hello.PreviewPort > 0 {
		if err := s.forward(hello.PreviewPort, m.o.PreviewPortPerProject); err != nil {
			fail(ProblemPortBusy, err.Error(), false)
			return
		}
	}
	origin, port, err := s.serveProxy(next.ProxyPort, m.o.Prefix, m.o.SessionPath, m.serveLocal)
	if err != nil {
		fail(ProblemPortBusy, err.Error(), false)
		return
	}
	m.saveToken(in.Token)
	next.Active, next.ProxyPort = true, port
	m.saveTarget(next)
	m.mu.Lock()
	m.sess, m.origin, m.problem = s, origin, nil
	m.mu.Unlock()
	m.o.Log(fmt.Sprintf("remote: connected to %s (vibechat-server %s, protocol %d) at %s", addr, hello.ServerVersion, hello.Protocol, origin))
	u := origin + m.o.SessionPath + "?" + url.Values{m.o.TicketParam: {ticket}}.Encode()
	go m.o.Navigate(u)
}

// Open moves the window to route of the server's Studio (the native menu's Settings…). Connected: a fresh one-time
// ticket and /auth/session?ticket=…&next=route on the server's origin, so the session cookie is set by that origin
// whatever page the window shows now (a plain SetURL("/settings") resolved against the app's own origin and left the
// window on the start screen). Not connected: the start screen, which is the connection settings.
func (m *Manager) Open(ctx context.Context, route string) {
	m.mu.Lock()
	s, origin := m.sess, m.origin
	m.mu.Unlock()
	if s == nil || origin == "" {
		m.o.Navigate("/")
		return
	}
	route = auth.SafeNext(route)
	token, err := m.o.Creds.Get(CredHostToken)
	if err == nil {
		var ticket string
		if ticket, err = s.ticket(ctx, m.o.Prefix, strings.TrimSpace(token)); err == nil {
			m.o.Navigate(origin + m.o.SessionPath + "?" + url.Values{m.o.TicketParam: {ticket}, auth.NextParam: {route}}.Encode())
			return
		}
	}
	// No ticket (keychain, server): the window's own session cookie may still be good.
	m.o.Log("remote: open " + route + ": " + err.Error())
	m.o.Navigate(origin + route)
}

// keepTarget saves next after a failed SSH step when it is a new server (so its address shows) and keeps the old
// one's pin and active flag otherwise.
func (m *Manager) keepTarget(old, next *Target) {
	if old != nil && old.Address == next.Address && old.User == next.User {
		return
	}
	next.Active = false
	m.saveTarget(next)
}

func (m *Manager) saveTarget(t *Target) {
	if err := saveTarget(m.file, t); err != nil {
		m.o.Log("remote: " + err.Error())
	}
	m.mu.Lock()
	cp := *t
	m.target = &cp
	m.mu.Unlock()
}

func (m *Manager) saveToken(tok string) {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return
	}
	if err := m.o.Creds.Set(CredHostToken, tok); err != nil {
		m.o.Log("remote: host token not saved in the system keychain: " + err.Error())
	}
}

// Disconnect goes back to the start screen (the server stays saved, not active); forget also drops the server, its
// pin and the host token (the app's SSH key stays).
func (m *Manager) Disconnect(forget bool) Connection {
	m.op.Lock()
	defer m.op.Unlock()
	m.stop(true)
	m.mu.Lock()
	t := m.target
	m.server, m.outdated, m.problem = nil, false, nil
	m.mu.Unlock()
	if forget {
		if err := removeTarget(m.file); err != nil {
			m.o.Log("remote: " + err.Error())
		}
		if err := m.o.Creds.Delete(CredHostToken); err != nil {
			m.o.Log("remote: " + err.Error())
		}
		m.mu.Lock()
		m.target = nil
		m.mu.Unlock()
	} else if t != nil && t.Active {
		cp := *t
		cp.Active = false
		m.saveTarget(&cp)
	}
	return m.Connection()
}

// stop ends the live session; with navigate the window goes back to the start screen first (the request that
// asked may be in flight through the proxy, so the proxy closes a moment later).
func (m *Manager) stop(navigate bool) {
	m.mu.Lock()
	s := m.sess
	m.sess, m.origin = nil, ""
	m.mu.Unlock()
	if s == nil {
		return
	}
	if navigate {
		done := make(chan struct{})
		m.mu.Lock()
		m.closing = done
		m.mu.Unlock()
		go m.o.Navigate("/")
		go func() {
			defer close(done)
			time.Sleep(1500 * time.Millisecond)
			s.close()
		}()
		return
	}
	s.close()
}

// settle waits for a session closing after Disconnect (its ports are needed again).
func (m *Manager) settle() {
	m.mu.Lock()
	done := m.closing
	m.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// Close ends the connection (app exit); the saved server stays active.
func (m *Manager) Close() { m.stop(false) }

// Middleware answers the connection routes in the app's own origin and passes everything else on.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == m.DesktopPath() {
			m.serveDesktop(w, r)
			return
		}
		if r.URL.Path == m.ProvisionPath() {
			m.serveProvision(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveLocal is the proxy's local part: the connection and archive routes for a window signed in to the server (its session
// cookie, checked by the server), from the proxy's own origin.
func (m *Manager) serveLocal(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != m.DesktopPath() && r.URL.Path != m.ArchivePath() {
		return false
	}
	m.mu.Lock()
	s, origin := m.sess, m.origin
	m.mu.Unlock()
	if o := r.Header.Get("Origin"); o != "" && o != origin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request"})
		return true
	}
	if s == nil || !m.signedIn(r, s) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in again from the VibeChat app"})
		return true
	}
	if r.URL.Path == m.ArchivePath() {
		m.serveArchive(w, r, s)
		return true
	}
	m.serveDesktop(w, r)
	return true
}

// signedIn asks the server whether the request's cookies are a live session (GET capabilities).
func (m *Manager) signedIn(r *http.Request, s *session) bool {
	ck := r.Header.Get("Cookie")
	if ck == "" {
		return false
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://"+ServerAPI+m.o.Prefix+"/host/capabilities", nil)
	req.Header.Set("Cookie", ck)
	res, err := s.api().Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	return res.StatusCode == http.StatusOK
}

func (m *Manager) serveDesktop(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, m.Connection())
	case http.MethodPut:
		var in ConnectInput
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: " + err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
		defer cancel()
		writeJSON(w, http.StatusOK, m.Connect(ctx, in))
	case http.MethodDelete:
		forget, _ := strconv.ParseBool(r.URL.Query().Get("forget"))
		writeJSON(w, http.StatusOK, m.Disconnect(forget))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// versionLess compares "x.y.z" release versions (a leading v and a -suffix ignored); anything else is not less.
func versionLess(a, b string) bool {
	pa, oka := semver(a)
	pb, okb := semver(b)
	if !oka || !okb {
		return false
	}
	for i := range 3 {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func semver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
