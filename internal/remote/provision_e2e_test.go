//go:build e2e

package remote

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestProvisionE2E runs the setup wizard against a fresh Debian 13 VM with sshd and a root password
// (scripts/ci-provision-e2e.sh), twice:
//
//  1. every step for real, but the lockdown is not confirmed: password login is off at once and comes back by
//     itself when the rollback timer fires;
//  2. the second run skips every step that is done, takes the lockdown with the confirmation, and the app connects
//     (ticket → the window's sign-in through the tunnel); root's password is refused from then on.
//
//	VIBECHAT_E2E_PROVISION_ADDR=<ip>:22 VIBECHAT_E2E_ROOT_PASSWORD_FILE=<file> VIBECHAT_E2E_DEB_URL=… \
//	VIBECHAT_E2E_SUMS_URL=… go test -tags e2e -run TestProvisionE2E ./internal/remote
func TestProvisionE2E(t *testing.T) {
	addr, pwFile := os.Getenv("VIBECHAT_E2E_PROVISION_ADDR"), os.Getenv("VIBECHAT_E2E_ROOT_PASSWORD_FILE")
	if addr == "" || pwFile == "" {
		t.Skip("VIBECHAT_E2E_PROVISION_* not set")
	}
	raw, err := os.ReadFile(pwFile)
	if err != nil {
		t.Fatal(err)
	}
	pw := strings.TrimSpace(string(raw))
	const rollback = 40 * time.Second
	nav := make(chan string, 4)
	m, err := New(Options{
		DataDir: t.TempDir(), Creds: &MemCreds{}, AppVersion: "0.1.7", AppProtocol: 1, MinServerProtocol: 1,
		Prefix: "/runtime/v1", SessionPath: "/auth/session", TicketParam: "ticket",
		Navigate: func(u string) { nav <- u }, Log: func(s string) { t.Log(s) },
		Provision: ProvisionOptions{DebURL: os.Getenv("VIBECHAT_E2E_DEB_URL"), SumsURL: os.Getenv("VIBECHAT_E2E_SUMS_URL"), RollbackAfter: rollback},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	hostport, err := sshAddr(addr)
	if err != nil {
		t.Fatal(err)
	}
	rootPassword := func() error {
		var seen ssh.PublicKey
		c, err := dialSSHAuth(context.Background(), hostport, "root", []ssh.AuthMethod{ssh.Password(pw)}, nil, &seen)
		if err == nil {
			_ = c.Close()
		}
		return err
	}
	if err := rootPassword(); err != nil {
		t.Fatalf("the VM does not take the root password before the setup: %v", err)
	}
	run := func(name string) Provision {
		t.Helper()
		p, ok := m.StartProvision(ProvisionInput{Address: addr, Password: pw})
		if !ok || p.State != "running" {
			t.Fatalf("%s: %+v", name, p)
		}
		last := ""
		deadline := time.Now().Add(30 * time.Minute)
		for time.Now().Before(deadline) {
			p = m.Provision()
			var b strings.Builder
			for _, s := range p.Steps {
				b.WriteString(s.ID + "=" + s.State + " ")
			}
			if b.String() != last {
				last = b.String()
				t.Logf("%s: %s", name, last)
			}
			if p.State != "running" {
				return p
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("%s: still running after 30 minutes", name)
		return p
	}
	states := func(p Provision) map[string]string {
		out := map[string]string{}
		for _, s := range p.Steps {
			out[s.ID] = s.State
		}
		return out
	}

	// 1. No confirmation: the safety net.
	m.provisionConfirm = func(context.Context) error { return errors.New("no confirmation (e2e)") }
	p := run("run 1")
	if p.State != "failed" || p.Problem == nil || p.Problem.Code != "not_confirmed" {
		t.Fatalf("run 1: %+v %v", p.Problem, states(p))
	}
	for id, st := range states(p) {
		if id != "lockdown" && st != "done" {
			t.Errorf("run 1: %s %s", id, st)
		}
	}
	if err := rootPassword(); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("run 1: password login still works after the lockdown: %v", err)
	}
	t.Logf("run 1: password login is off; waiting for the rollback timer (%s)", rollback)
	back := false
	for end := time.Now().Add(rollback + 90*time.Second); time.Now().Before(end); time.Sleep(5 * time.Second) {
		if rootPassword() == nil {
			back = true
			break
		}
	}
	if !back {
		t.Fatal("the rollback timer did not bring password login back")
	}
	t.Log("run 1: the rollback brought password login back")

	// 2. The second run: done steps are skipped, the lockdown is confirmed, the app connects.
	m.provisionConfirm = nil
	p = run("run 2")
	if p.State != "done" || p.Problem != nil {
		t.Fatalf("run 2: %+v %v", p.Problem, states(p))
	}
	want := map[string]string{
		"connect": "done", "preflight": "done", "incus": "skipped", "container": "skipped", "server": "skipped", "egress": "skipped",
		"token": "skipped", "tunnel": "skipped", "verify": "done", "lockdown": "done",
	}
	for id, st := range states(p) {
		if want[id] != st {
			t.Errorf("run 2: %s %s, want %s", id, st, want[id])
		}
	}
	if err := rootPassword(); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("run 2: password login still works: %v", err)
	}
	time.Sleep(rollback + 15*time.Second)
	if err := rootPassword(); err == nil {
		t.Fatal("run 2: password login came back although the lockdown was confirmed")
	}
	c := m.Connection()
	if c.Mode != "remote" || c.Server == nil || c.Target == nil || c.Target.User != "vibechat-tunnel" || !c.Target.TokenSaved {
		t.Fatalf("connection %+v", c)
	}
	var u string
	select {
	case u = <-nav:
	case <-time.After(10 * time.Second):
		t.Fatal("the window was not moved to the server")
	}
	pu, err := url.Parse(u)
	if err != nil || pu.Path != "/auth/session" || pu.Query().Get("ticket") == "" {
		t.Fatalf("navigate %q", u)
	}
	signIn(t, browser(t), u, pu.Scheme+"://"+pu.Host)
	t.Logf("the app is connected to vibechat-server %s at %s", c.Server.Version, pu.Host)

	// A third run cannot even sign in as root with the password: the server is set up.
	p = run("run 3")
	if p.Problem == nil || p.Problem.Code != "auth" {
		t.Fatalf("run 3: %+v", p.Problem)
	}
}
