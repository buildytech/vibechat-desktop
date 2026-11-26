package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/buildytech/vibechat-desktop/desktop/internal/provision"
)

// ProvisionOptions configure the setup wizard (defaults: the release of the app's version, 5 minutes).
type ProvisionOptions struct {
	DebURL, SumsURL string
	RollbackAfter   time.Duration
	Container       string
}

// ProvisionInput is DesktopProvisionInputSchema.
type ProvisionInput struct {
	Address       string `json:"address"`
	Password      string `json:"password"`
	RootPublicKey string `json:"rootPublicKey,omitempty"`
}

// ProvisionStep is one entry of DesktopProvision.steps.
type ProvisionStep struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// Provision is DesktopProvisionSchema.
type Provision struct {
	State              string          `json:"state"` // idle | running | done | failed
	Steps              []ProvisionStep `json:"steps"`
	Address            string          `json:"address,omitempty"`
	HostKeyFingerprint string          `json:"hostKeyFingerprint,omitempty"`
	RollbackSeconds    int             `json:"rollbackSeconds"`
	Problem            *Problem        `json:"problem,omitempty"`
}

// ProvisionPath is the seam path of the wizard's routes.
func (m *Manager) ProvisionPath() string { return m.o.Prefix + "/desktop/provision" }

func (m *Manager) provisionOptions() ProvisionOptions {
	p := m.o.Provision
	if p.DebURL == "" || p.SumsURL == "" {
		p.DebURL, p.SumsURL = provision.ReleaseURLs(m.o.AppVersion)
	}
	if p.RollbackAfter <= 0 {
		p.RollbackAfter = 5 * time.Minute
	}
	return p
}

func idleProvision(rollback time.Duration) Provision {
	p := Provision{State: "idle", RollbackSeconds: int(rollback / time.Second)}
	for _, id := range provision.Steps {
		p.Steps = append(p.Steps, ProvisionStep{ID: id, State: provision.Pending})
	}
	return p
}

// Provision answers the wizard's progress.
func (m *Manager) Provision() Provision {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.prov == nil {
		return idleProvision(m.provisionOptions().RollbackAfter)
	}
	p := *m.prov
	p.Steps = append([]ProvisionStep(nil), m.prov.Steps...)
	return p
}

// StartProvision starts a run in the background (busy: one runs already) and answers its first state. The
// password lives in the run's memory only and is dropped when it ends.
func (m *Manager) StartProvision(in ProvisionInput) (Provision, bool) {
	o := m.provisionOptions()
	addr := strings.TrimSpace(in.Address)
	bad := func(msg string) (Provision, bool) {
		p := idleProvision(o.RollbackAfter)
		p.State, p.Address, p.Problem = "failed", addr, &Problem{Code: provision.ProblemBadInput, Message: msg}
		return p, true
	}
	hostport, err := sshAddr(addr)
	if err != nil {
		return bad(err.Error())
	}
	if in.Password == "" || len(in.Password) > 1024 {
		return bad("enter the root password of the server")
	}
	rootKey := strings.TrimSpace(in.RootPublicKey)
	m.mu.Lock()
	if m.provRunning {
		p := *m.prov
		m.mu.Unlock()
		return p, false
	}
	p := idleProvision(o.RollbackAfter)
	p.State, p.Address = "running", addr
	m.prov, m.provRunning = &p, true
	m.mu.Unlock()
	pw := []byte(in.Password)
	go m.runProvision(addr, hostport, pw, rootKey, o)
	return m.Provision(), true
}

func (m *Manager) setStep(id, state, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.prov.Steps {
		if m.prov.Steps[i].ID == id {
			m.prov.Steps[i].State, m.prov.Steps[i].Detail = state, detail
		}
	}
}

func (m *Manager) endProvision(problem *Problem) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.provRunning = false
	if problem != nil {
		m.prov.State, m.prov.Problem = "failed", problem
		return
	}
	m.prov.State = "done"
}

func (m *Manager) runProvision(addr, hostport string, pw []byte, rootKey string, o ProvisionOptions) {
	defer func() {
		for i := range pw {
			pw[i] = 0
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	signer, err := ensureSigner(m.o.Creds)
	if err != nil {
		m.setStep(provision.StepConnect, provision.Failed, err.Error())
		m.endProvision(&Problem{Code: provision.ProblemStepFailed, Message: err.Error()})
		return
	}
	password := func() (string, error) { return string(pw), nil }
	rootAuth := []ssh.AuthMethod{
		ssh.PasswordCallback(password),
		ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			out := make([]string, len(qs))
			for i := range qs {
				out[i] = string(pw)
			}
			return out, nil
		}),
	}
	// The pin: a saved server at this address keeps its key (strict); a new one is trusted on first use.
	m.mu.Lock()
	old := m.target
	m.mu.Unlock()
	var pin ssh.PublicKey
	if old != nil && old.Address == addr {
		pin, _ = parsePin(old.HostKey)
	}
	var root *ssh.Client
	defer func() {
		if root != nil {
			_ = root.Close()
		}
	}()
	tunnel := func(ctx context.Context) error {
		var seen ssh.PublicKey
		c, err := dialSSH(ctx, hostport, provision.TunnelUser, signer, pin, &seen)
		if err != nil {
			return err
		}
		s := newSession(hostport, provision.TunnelUser, signer, pin, c, m.o.Log)
		defer s.close()
		if _, err := s.hello(ctx, m.o.Prefix); err != nil {
			return fmt.Errorf("hello through the tunnel: %v", err)
		}
		tok, err := m.o.Creds.Get(CredHostToken)
		if err != nil {
			return fmt.Errorf("host token: %v", err)
		}
		if _, err := s.ticket(ctx, m.o.Prefix, strings.TrimSpace(tok)); err != nil {
			return err
		}
		return nil
	}
	confirm := func(ctx context.Context) error {
		if err := whileRefused(ctx, confirmRefusedFor, func() error { return tunnel(ctx) }); err != nil {
			return err
		}
		var seen ssh.PublicKey
		var c *ssh.Client
		err := whileRefused(ctx, confirmRefusedFor, func() (err error) {
			c, err = dialSSHAuth(ctx, hostport, "root", rootAuth, pin, &seen)
			return err
		})
		if err == nil {
			_ = c.Close()
			return errors.New("the server still accepts the root password")
		}
		if !strings.Contains(err.Error(), "unable to authenticate") {
			return fmt.Errorf("checking that the root password is refused: %v", err)
		}
		return nil
	}
	if m.provisionConfirm != nil {
		confirm = m.provisionConfirm
	}
	hooks := provision.Hooks{
		Connect: func(ctx context.Context) (provision.Runner, error) {
			var seen ssh.PublicKey
			c, err := dialSSHAuth(ctx, hostport, "root", rootAuth, pin, &seen)
			switch {
			case errors.Is(err, ErrHostKeyChanged):
				return nil, &provision.Error{Code: provision.ProblemHostKeyChanged, Message: err.Error() + " (if the server was reinstalled: Forget server in Settings, then set it up again)"}
			case err != nil && strings.Contains(err.Error(), "unable to authenticate"):
				return nil, &provision.Error{Code: provision.ProblemAuth, Message: "the server did not accept the root password (or root password login is already off)"}
			case err != nil:
				return nil, &provision.Error{Code: provision.ProblemUnreachable, Message: err.Error()}
			}
			root = c
			if pin == nil {
				pin = seen
				m.o.Log("remote: setup: pinned the host key of " + addr + ": " + ssh.FingerprintSHA256(seen))
			}
			t := &Target{Address: addr, User: provision.TunnelUser, HostKey: marshalPin(pin)}
			if old != nil && old.Address == addr {
				t.ProxyPort = old.ProxyPort
			}
			m.saveTarget(t)
			m.mu.Lock()
			m.prov.HostKeyFingerprint = ssh.FingerprintSHA256(pin)
			m.mu.Unlock()
			return provision.SSHRunner{Client: c}, nil
		},
		TokenGet: func() string {
			tok, err := m.o.Creds.Get(CredHostToken)
			if err != nil {
				return ""
			}
			return tok
		},
		TokenSet: func(tok string) error { return m.o.Creds.Set(CredHostToken, tok) },
		Verify:   tunnel,
		Confirm:  confirm,
		Progress: m.setStep,
		Log:      m.o.Log,
	}
	cfg := provision.Config{
		Container: o.Container, AppKey: authorizedLine(signer.PublicKey()), RootKey: rootKey,
		DebURL: o.DebURL, SumsURL: o.SumsURL, RollbackAfter: o.RollbackAfter,
	}
	if err := provision.Run(ctx, cfg, hooks); err != nil {
		var pe *provision.Error
		if !errors.As(err, &pe) {
			pe = &provision.Error{Code: provision.ProblemStepFailed, Message: err.Error()}
		}
		m.endProvision(&Problem{Code: pe.Code, Message: pe.Message})
		return
	}
	if root != nil {
		_ = root.Close()
		root = nil
	}
	m.o.Log("remote: setup of " + addr + " done; connecting")
	c := m.Connect(ctx, ConnectInput{Address: addr, User: provision.TunnelUser})
	if c.Problem != nil {
		m.endProvision(&Problem{Code: provision.ProblemUnreachable, Message: "the server is set up, connecting failed: " + c.Problem.Message})
		return
	}
	m.endProvision(nil)
}

func (m *Manager) serveProvision(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, m.Provision())
	case http.MethodPost:
		var in ProvisionInput
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: expected {address, password, rootPublicKey?}"})
			return
		}
		p, started := m.StartProvision(in)
		in.Password = ""
		if !started {
			p.Problem = &Problem{Code: provision.ProblemBusy, Message: "a setup is running already"}
			writeJSON(w, http.StatusConflict, p)
			return
		}
		writeJSON(w, http.StatusOK, p)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
	}
}

// confirmRefusedFor: sshd on Debian 13 re-executes itself on `systemctl reload ssh`, and for a moment its port refuses
// connections (seen in CI right after the lockdown); the confirmation waits that out instead of failing.
var confirmRefusedFor = 20 * time.Second

// refused: the port is closed for now ("connection refused"; Windows: "… actively refused it").
func refused(err error) bool {
	m := err.Error()
	return strings.Contains(m, "connection refused") || strings.Contains(m, "actively refused")
}

// whileRefused retries f while it fails with "connection refused", for at most d; any other answer ends it.
func whileRefused(ctx context.Context, d time.Duration, f func() error) error {
	deadline := time.Now().Add(d)
	for {
		err := f()
		if err == nil || !refused(err) || !time.Now().Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(500 * time.Millisecond):
		}
	}
}
