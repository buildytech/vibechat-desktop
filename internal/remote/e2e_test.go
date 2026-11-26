//go:build e2e

package remote

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E runs the desktop connection against a real vibechat-server .deb on Debian 13 behind a real sshd
// (scripts/ci-remote-e2e.sh): the tunnel user from the Studio's setup lines, trust on first use, Bearer → ticket,
// the window's sign-in through the local proxy, the seam, the desktop routes, the preview gateway on its own port,
// a strict pin after reconnect, a changed host key refused, and permitopen holding.
//
//	VIBECHAT_E2E_SSH=<ip>:22 VIBECHAT_E2E_SSH_KEY=<private key file> VIBECHAT_E2E_TOKEN_FILE=<file>
//	VIBECHAT_E2E_HOST_FP=SHA256:… go test -tags e2e -run TestE2E ./internal/remote
func TestE2E(t *testing.T) {
	addr, keyFile, tokFile, wantFP := os.Getenv("VIBECHAT_E2E_SSH"), os.Getenv("VIBECHAT_E2E_SSH_KEY"), os.Getenv("VIBECHAT_E2E_TOKEN_FILE"), os.Getenv("VIBECHAT_E2E_HOST_FP")
	if addr == "" || keyFile == "" || tokFile == "" {
		t.Skip("VIBECHAT_E2E_* not set")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := os.ReadFile(tokFile)
	if err != nil {
		t.Fatal(err)
	}
	user := os.Getenv("VIBECHAT_E2E_USER")
	if user == "" {
		user = "vibechat-tunnel"
	}
	creds := &MemCreds{}
	_ = creds.Set(CredSSHKey, string(key))
	data := t.TempDir()
	nav := make(chan string, 4)
	newM := func() *Manager {
		m, err := New(Options{
			DataDir: data, Creds: creds, AppVersion: "0.1.5", AppProtocol: 1, MinServerProtocol: 1,
			Prefix: "/runtime/v1", SessionPath: "/auth/session", TicketParam: "ticket",
			Navigate: func(u string) { nav <- u }, Log: func(s string) { t.Log(s) },
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := newM()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	wait := func() string {
		select {
		case u := <-nav:
			return u
		case <-time.After(10 * time.Second):
			t.Fatal("no navigation")
			return ""
		}
	}

	c := m.Connect(ctx, ConnectInput{Address: addr, User: user, Token: strings.TrimSpace(string(tok))})
	if c.Problem != nil || c.Mode != "remote" || c.Server == nil {
		t.Fatalf("connect: %+v %+v", c.Problem, c)
	}
	t.Logf("connected: server %s protocol %d, host key %s", c.Server.Version, c.Server.Protocol, c.Target.HostKeyFingerprint)
	if wantFP != "" && c.Target.HostKeyFingerprint != wantFP {
		t.Fatalf("pinned %s, the server's ed25519 key is %s", c.Target.HostKeyFingerprint, wantFP)
	}
	u := wait()
	origin := u[:strings.Index(u, "/auth/")]
	b := browser(t)
	signIn(t, b, u, origin)
	if code, body := getJSON(t, b, "GET", origin+"/runtime/v1/host/capabilities", nil); code != 200 || body["seam"] != "1" {
		t.Fatalf("capabilities: %d %v", code, body)
	}
	if code, _ := getJSON(t, http.DefaultClient, "GET", origin+"/runtime/v1/projects", nil); code != 401 {
		t.Fatalf("projects without the session: %d", code)
	}
	if code, body := getJSON(t, b, "GET", origin+"/runtime/v1/desktop/connection", nil); code != 200 || body["mode"] != "remote" {
		t.Fatalf("desktop connection: %d %v", code, body)
	}
	// The preview gateway through the forward, on the same port: an unknown project is 404. (A local run with the
	// server on this machine cannot forward its own port: VIBECHAT_E2E_NO_PREVIEW=1.)
	if os.Getenv("VIBECHAT_E2E_NO_PREVIEW") == "" {
		checkPreview(t)
	}
	// permitopen: the tunnel user cannot reach other ports on the server.
	m.mu.Lock()
	s := m.sess
	m.mu.Unlock()
	if conn, err := s.dial(ctx, "127.0.0.1:22"); err == nil {
		conn.Close()
		t.Fatal("the tunnel opened 127.0.0.1:22")
	}

	// Back to this computer, then again with the token from the keychain (strict pin).
	m.Disconnect(false)
	wait()
	if c := m.Connect(ctx, ConnectInput{Address: addr, User: user}); c.Problem != nil || c.Mode != "remote" {
		t.Fatalf("reconnect: %+v", c.Problem)
	}
	wait()
	m.Close()

	// A different pinned key (as if the server were replaced): refused.
	raw, _ := os.ReadFile(filepath.Join(data, "remote.json"))
	var tg Target
	_ = json.Unmarshal(raw, &tg)
	tg.HostKey = marshalPin(newHostKey(t).PublicKey())
	if err := saveTarget(filepath.Join(data, "remote.json"), &tg); err != nil {
		t.Fatal(err)
	}
	m2 := newM()
	defer m2.Close()
	if c := m2.Connect(ctx, ConnectInput{Address: addr, User: user}); c.Problem == nil || c.Problem.Code != ProblemHostKeyChanged {
		t.Fatalf("changed host key: %+v", c.Problem)
	}
}

func checkPreview(t *testing.T) {
	pc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, a string) (net.Conn, error) {
		_, port, _ := net.SplitHostPort(a)
		return (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:"+port)
	}}}
	res, err := pc.Get("http://abc.preview.localhost:18121/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("preview gateway: %d", res.StatusCode)
	}
}
