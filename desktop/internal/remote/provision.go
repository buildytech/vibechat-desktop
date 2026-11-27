package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/buildytech/vibechat-desktop/desktop/internal/provision"
)

// ProvisionOptions configure the setup wizard (defaults: the release of the app's version, 5 minutes).
type ProvisionOptions struct {
	DebURL, SumsURL string
	RollbackAfter   time.Duration
	Container       string
	// ReadyPause: how long the wizard shows "the server is ready" before the app connects (default 2 s).
	ReadyPause time.Duration
}

// ProvisionInput is DesktopProvisionInputSchema.
type ProvisionInput struct {
	Address       string `json:"address"`
	Password      string `json:"password"`
	RootPublicKey string `json:"rootPublicKey,omitempty"`
}

// ProvisionStep is one entry of DesktopProvision.steps; times are Unix milliseconds of the app's clock.
type ProvisionStep struct {
	ID        string         `json:"id"`
	State     string         `json:"state"`
	Detail    string         `json:"detail,omitempty"`
	StartedAt int64          `json:"startedAt,omitempty"`
	EndedAt   int64          `json:"endedAt,omitempty"`
	Subs      []ProvisionSub `json:"subs,omitempty"`
}

// ProvisionSub is one substep of a long step (provision.Substeps).
type ProvisionSub struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	StartedAt int64  `json:"startedAt,omitempty"`
	EndedAt   int64  `json:"endedAt,omitempty"`
}

// ProvisionLogLine is one line of the run's log (secrets scrubbed).
type ProvisionLogLine struct {
	At   int64  `json:"at"`
	Text string `json:"text"`
}

// provisionLogMax is how many of the last log lines the progress carries.
const provisionLogMax = 200

// Provision is DesktopProvisionSchema.
type Provision struct {
	State              string          `json:"state"` // idle | running | done | failed
	Steps              []ProvisionStep `json:"steps"`
	Address            string          `json:"address,omitempty"`
	HostKeyFingerprint string          `json:"hostKeyFingerprint,omitempty"`
	RollbackSeconds    int             `json:"rollbackSeconds"`
	Problem            *Problem        `json:"problem,omitempty"`
	// StartedAt of the current or last run; Now is the app's clock when it answered (for the elapsed timers).
	StartedAt int64              `json:"startedAt,omitempty"`
	Now       int64              `json:"now"`
	Log       []ProvisionLogLine `json:"log"`
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
	if p.ReadyPause == 0 {
		p.ReadyPause = 2 * time.Second
	}
	return p
}

func idleProvision(rollback time.Duration) Provision {
	p := Provision{State: "idle", RollbackSeconds: int(rollback / time.Second), Log: []ProvisionLogLine{}}
	for _, id := range provision.Steps {
		st := ProvisionStep{ID: id, State: provision.Pending}
		for _, sub := range provision.Substeps[id] {
			st.Subs = append(st.Subs, ProvisionSub{ID: sub, State: provision.Pending})
		}
		p.Steps = append(p.Steps, st)
	}
	return p
}

// copyProvision is a deep copy of p (the answer never shares slices with the running wizard).
func copyProvision(p Provision) Provision {
	out := p
	out.Steps = make([]ProvisionStep, len(p.Steps))
	for i, s := range p.Steps {
		s.Subs = append([]ProvisionSub(nil), s.Subs...)
		out.Steps[i] = s
	}
	out.Log = append([]ProvisionLogLine{}, p.Log...)
	return out
}

// Provision answers the wizard's progress.
func (m *Manager) Provision() Provision {
	m.mu.Lock()
	defer m.mu.Unlock()
	var p Provision
	if m.prov == nil {
		p = idleProvision(m.provisionOptions().RollbackAfter)
	} else {
		p = copyProvision(*m.prov)
	}
	p.Now = time.Now().UnixMilli()
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
		p := copyProvision(*m.prov)
		m.mu.Unlock()
		p.Now = time.Now().UnixMilli()
		return p, false
	}
	p := idleProvision(o.RollbackAfter)
	p.State, p.Address, p.StartedAt = "running", addr, time.Now().UnixMilli()
	m.prov, m.provRunning = &p, true
	m.mu.Unlock()
	pw := []byte(in.Password)
	go m.runProvision(addr, hostport, pw, rootKey, o)
	return m.Provision(), true
}

// stamp sets the times of a step or substep for a new state: running starts the clock, any end state stops it.
func stamp(state string, started, ended *int64) {
	now := time.Now().UnixMilli()
	switch state {
	case provision.Running:
		*started, *ended = now, 0
	case provision.Pending:
		*started, *ended = 0, 0
	default:
		if *started == 0 {
			*started = now
		}
		*ended = now
	}
}

func (m *Manager) setStep(id, state, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.prov.Steps {
		if s := &m.prov.Steps[i]; s.ID == id {
			s.State, s.Detail = state, detail
			stamp(state, &s.StartedAt, &s.EndedAt)
		}
	}
}

func (m *Manager) setSub(step, sub, state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.prov.Steps {
		if m.prov.Steps[i].ID != step {
			continue
		}
		for j := range m.prov.Steps[i].Subs {
			if s := &m.prov.Steps[i].Subs[j]; s.ID == sub {
				s.State = state
				stamp(state, &s.StartedAt, &s.EndedAt)
			}
		}
	}
}

// provLog appends a line to the run's log (the last provisionLogMax) and the app's log.
func (m *Manager) provLog(line string) {
	m.mu.Lock()
	if m.prov != nil {
		m.prov.Log = append(m.prov.Log, ProvisionLogLine{At: time.Now().UnixMilli(), Text: line})
		if n := len(m.prov.Log); n > provisionLogMax {
			m.prov.Log = append([]ProvisionLogLine(nil), m.prov.Log[n-provisionLogMax:]...)
		}
	}
	m.mu.Unlock()
	m.o.Log(line)
}

// scrubber replaces every secret (the root password, the host token) in a text by "[redacted]". A secret is read
// when the text is scrubbed: the password is zeroed after the run, the token appears in the middle of it.
func scrubber(secrets ...func() []byte) func(string) string {
	return func(s string) string {
		for _, get := range secrets {
			if b := get(); len(bytes.Trim(b, "\x00")) > 0 && strings.Contains(s, string(b)) {
				s = strings.ReplaceAll(s, string(b), "[redacted]")
			}
		}
		return s
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
	var tok atomic.Value // the host token, once read or made (for the scrubber)
	tok.Store([]byte(nil))
	scrub := scrubber(func() []byte { return pw }, func() []byte { b, _ := tok.Load().([]byte); return b })
	logf := func(s string) { m.provLog(scrub(s)) }
	end := func(p *Problem) {
		if p != nil {
			p.Message = scrub(p.Message)
		}
		m.endProvision(p)
	}
	logf("remote: setup of " + addr + " started")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	signer, err := ensureSigner(m.o.Creds)
	if err != nil {
		m.setStep(provision.StepConnect, provision.Failed, err.Error())
		end(&Problem{Code: provision.ProblemStepFailed, Message: err.Error()})
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
				logf("remote: setup: pinned the host key of " + addr + ": " + ssh.FingerprintSHA256(seen))
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
			t, err := m.o.Creds.Get(CredHostToken)
			if err != nil {
				return ""
			}
			tok.Store([]byte(strings.TrimSpace(t)))
			return t
		},
		TokenSet: func(t string) error { tok.Store([]byte(strings.TrimSpace(t))); return m.o.Creds.Set(CredHostToken, t) },
		Verify:   tunnel,
		Confirm:  confirm,
		Progress: func(step, state, detail string) { m.setStep(step, state, scrub(detail)) },
		Substep:  m.setSub,
		Log:      logf,
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
		end(&Problem{Code: pe.Code, Message: pe.Message})
		return
	}
	if root != nil {
		_ = root.Close()
		root = nil
	}
	logf("remote: setup of " + addr + " done; the server is ready, connecting")
	if o.ReadyPause > 0 {
		select {
		case <-time.After(o.ReadyPause):
		case <-ctx.Done():
		}
	}
	c := m.Connect(ctx, ConnectInput{Address: addr, User: provision.TunnelUser})
	if c.Problem != nil {
		end(&Problem{Code: provision.ProblemUnreachable, Message: "the server is set up, connecting failed: " + c.Problem.Message})
		return
	}
	end(nil)
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
