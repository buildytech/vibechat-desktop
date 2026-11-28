package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/buildytech/vibechat-desktop/protocol/auth"
)

// ServerAPI is vibechat-server's API + Studio address on the server (the tunnel user may open only it and the
// preview gateway: permitopen in authorized_keys).
const ServerAPI = "127.0.0.1:18120"

const (
	dialTimeout = 15 * time.Second
	keepalive   = 30 * time.Second
)

// session is one live connection: the SSH client (redialled with the same pin when it drops), the window's local
// reverse proxy and the preview port forward.
type session struct {
	addr, user string
	signer     ssh.Signer
	pin        ssh.PublicKey
	logf       func(string)

	// pp is the per-project preview fallback (nil: the gateway port is forwarded as raw TCP).
	pp *previewPorts

	mu      sync.Mutex
	client  *ssh.Client
	closed  bool
	tr      *http.Transport
	servers []*http.Server
	lns     []net.Listener
	wg      sync.WaitGroup
}

// dialSSH opens the SSH connection; seen gets the host key the server presented.
func dialSSH(ctx context.Context, addr, user string, signer ssh.Signer, pin ssh.PublicKey, seen *ssh.PublicKey) (*ssh.Client, error) {
	return dialSSHAuth(ctx, addr, user, []ssh.AuthMethod{ssh.PublicKeys(signer)}, pin, seen)
}

// dialSSHAuth is dialSSH with any auth methods (the setup wizard's root password).
func dialSSHAuth(ctx context.Context, addr, user string, auth []ssh.AuthMethod, pin ssh.PublicKey, seen *ssh.PublicKey) (*ssh.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:              user,
		Auth:              auth,
		HostKeyCallback:   pinCallback(pin, seen),
		HostKeyAlgorithms: hostKeyAlgorithms(pin),
		Timeout:           dialTimeout,
	})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(cc, chans, reqs), nil
}

// hostKeyAlgorithms: with a pin only its type (the server must not switch to another of its keys); before the first
// connect ed25519 first, so the pin is the key `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` shows.
func hostKeyAlgorithms(pin ssh.PublicKey) []string {
	if pin == nil {
		return []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	if pin.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{pin.Type()}
}

func newSession(addr, user string, signer ssh.Signer, pin ssh.PublicKey, client *ssh.Client, logf func(string)) *session {
	s := &session{addr: addr, user: user, signer: signer, pin: pin, logf: logf}
	s.adopt(client)
	s.tr = &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return s.dial(ctx, ServerAPI) },
		MaxIdleConns:        16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: dialTimeout,
	}
	return s
}

// adopt makes c the live client: keepalives, and it is dropped when it dies (the next dial redials).
func (s *session) adopt(c *ssh.Client) {
	s.mu.Lock()
	s.client = c
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		_ = c.Wait()
		close(done)
		s.mu.Lock()
		if s.client == c {
			s.client = nil
		}
		s.mu.Unlock()
	}()
	go func() {
		t := time.NewTicker(keepalive)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					_ = c.Close()
					return
				}
			}
		}
	}()
}

// sshClient answers the live client, redialling (strict pin) when the connection dropped.
func (s *session) sshClient(ctx context.Context) (*ssh.Client, error) {
	s.mu.Lock()
	c, closed := s.client, s.closed
	s.mu.Unlock()
	if closed {
		return nil, errors.New("disconnected from the server")
	}
	if c != nil {
		return c, nil
	}
	var seen ssh.PublicKey
	c, err := dialSSH(ctx, s.addr, s.user, s.signer, s.pin, &seen)
	if err != nil {
		return nil, err
	}
	s.logf("remote: reconnected to " + s.addr)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = c.Close()
		return nil, errors.New("disconnected from the server")
	}
	s.mu.Unlock()
	s.adopt(c)
	return c, nil
}

// dial opens a TCP stream to addr on the server through the tunnel (one retry on a fresh connection).
func (s *session) dial(ctx context.Context, addr string) (net.Conn, error) {
	for attempt := 0; ; attempt++ {
		c, err := s.sshClient(ctx)
		if err != nil {
			return nil, err
		}
		conn, err := c.Dial("tcp", addr)
		if err == nil || attempt > 0 {
			return conn, err
		}
		var oc *ssh.OpenChannelError
		if errors.As(err, &oc) {
			return nil, err // the server refused this port (permitopen): a fresh connection will not help
		}
		_ = c.Close()
		s.mu.Lock()
		if s.client == c {
			s.client = nil
		}
		s.mu.Unlock()
	}
}

// api is an HTTP client to vibechat-server through the tunnel.
func (s *session) api() *http.Client { return &http.Client{Transport: s.tr, Timeout: 20 * time.Second} }

// Hello is GET /host/hello.
type Hello struct {
	Seam              string `json:"seam"`
	ServerVersion     string `json:"serverVersion"`
	InstanceID        string `json:"instanceId"`
	Protocol          int    `json:"protocol"`
	MinClientProtocol int    `json:"minClientProtocol"`
	PreviewPort       int    `json:"previewPort"`
}

func (s *session) hello(ctx context.Context, prefix string) (Hello, error) {
	var h Hello
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ServerAPI+prefix+"/host/hello", nil)
	res, err := s.api().Do(req)
	if err != nil {
		return h, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return h, fmt.Errorf("hello: %s", res.Status)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&h); err != nil {
		return h, fmt.Errorf("hello: %w", err)
	}
	if h.Protocol < 1 || h.ServerVersion == "" {
		return h, errors.New("hello: not a vibechat-server")
	}
	return h, nil
}

// errTokenRefused: POST /auth/ticket answered 401.
var errTokenRefused = errors.New("the server refused the host token")

// ticket buys a one-time window sign-in ticket with the host token (Bearer; the token never reaches the webview).
func (s *session) ticket(ctx context.Context, prefix, token string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+ServerAPI+prefix+"/auth/ticket", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := s.api().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		return "", errTokenRefused
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ticket: %s", res.Status)
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&body); err != nil || body.Ticket == "" {
		return "", fmt.Errorf("ticket: bad answer")
	}
	return body.Ticket, nil
}

// errPortBusy: a local port the connection needs is taken.
var errPortBusy = errors.New("local port busy")

// serveProxy starts the window's origin http://127.0.0.1:<port> (port 0 = any): the server's Studio and seam through
// the tunnel, with local answers for the paths in local (the desktop connection routes).
func (s *session) serveProxy(port int, prefix, sessionPath string, local func(w http.ResponseWriter, r *http.Request) bool) (string, int, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil && port != 0 {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return "", 0, err
	}
	target := &url.URL{Scheme: "http", Host: ServerAPI}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = ServerAPI
			if s.pp != nil {
				pr.Out.Header.Del("Accept-Encoding") // the preview URLs are rewritten in plain JSON
			}
		},
		Transport:     s.tr,
		FlushInterval: -1, // SSE
		// A server before 0.1.6 answers the sign-in with 303 → "/": that hop would carry the app's own origin as
		// the initiator and drop the SameSite=Strict cookie (auth.SignedInHTML), so the proxy answers the page.
		ModifyResponse: func(res *http.Response) error {
			if s.pp != nil {
				if err := s.pp.rewriteJSON(prefix, res); err != nil {
					return err
				}
			}
			if res.Request.URL.Path != sessionPath {
				return nil
			}
			next := res.Request.URL.Query().Get(auth.NextParam)
			legacy := res.StatusCode == http.StatusSeeOther && res.Header.Get("Location") == "/"
			// A server before 0.2.11 ignores next (File → Settings): a signed-in page with a next is answered here too.
			if !legacy && (res.StatusCode != http.StatusOK || auth.SafeNext(next) == "/") {
				return nil
			}
			page := auth.SignedInPage(next)
			_ = res.Body.Close()
			res.StatusCode, res.Status = http.StatusOK, "200 OK"
			res.Header.Del("Location")
			res.Header.Del("Content-Encoding")
			res.Header.Set("content-type", "text/html; charset=utf-8")
			res.Header.Set("cache-control", "no-store")
			res.Header.Set("content-length", strconv.Itoa(len(page)))
			res.ContentLength = int64(len(page))
			res.Body = io.NopCloser(strings.NewReader(page))
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "The server is unreachable over SSH: " + err.Error()})
		},
	}
	srv := &http.Server{
		ReadHeaderTimeout: 30 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !auth.LoopbackHost(r.Host) {
				http.Error(w, "this address answers only on 127.0.0.1", http.StatusMisdirectedRequest)
				return
			}
			if local(w, r) {
				return
			}
			rp.ServeHTTP(w, r)
		}),
	}
	s.mu.Lock()
	s.servers = append(s.servers, srv)
	s.lns = append(s.lns, ln)
	s.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	p := ln.Addr().(*net.TCPAddr).Port
	return "http://127.0.0.1:" + strconv.Itoa(p), p, nil
}

// forward listens on 127.0.0.1:<port> and [::1]:<port> (the same port as on the server: the preview gateway routes
// by Host "<id>.preview.localhost:<port>") and pipes each connection to 127.0.0.1:<port> on the server.
func (s *session) forward(port int, perProject bool) error {
	remote := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ln4, err := net.Listen("tcp", remote)
	if err != nil {
		return fmt.Errorf("%w: 127.0.0.1:%d (%v)", errPortBusy, port, err)
	}
	lns := []net.Listener{ln4}
	if ln6, err := net.Listen("tcp", net.JoinHostPort("::1", strconv.Itoa(port))); err == nil {
		lns = append(lns, ln6)
	} else {
		s.logf(fmt.Sprintf("remote: no IPv6 loopback for the preview forward: %v", err))
	}
	s.mu.Lock()
	s.lns = append(s.lns, lns...)
	s.mu.Unlock()
	if perProject {
		s.pp = newPreviewPorts(s, port)
		for _, ln := range lns {
			srv := &http.Server{ReadHeaderTimeout: 30 * time.Second, Handler: s.pp.gatewayHandler()}
			s.mu.Lock()
			s.servers = append(s.servers, srv)
			s.mu.Unlock()
			go func() { _ = srv.Serve(ln) }()
		}
		return nil
	}
	for _, ln := range lns {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go s.pipe(c, remote)
			}
		}()
	}
	return nil
}

func (s *session) pipe(local net.Conn, remote string) {
	defer local.Close()
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	up, err := s.dial(ctx, remote)
	cancel()
	if err != nil {
		s.logf("remote: preview forward: " + err.Error())
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, up); done <- struct{}{} }()
	<-done
}

// close stops the listeners, the proxy and the SSH connection.
func (s *session) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	c, servers, lns := s.client, s.servers, s.lns
	s.client = nil
	s.mu.Unlock()
	for _, srv := range servers {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
		_ = srv.Close()
	}
	for _, ln := range lns {
		_ = ln.Close()
	}
	s.tr.CloseIdleConnections()
	if s.pp != nil {
		s.pp.tr.CloseIdleConnections()
	}
	if c != nil {
		_ = c.Close()
	}
	s.wg.Wait()
}
