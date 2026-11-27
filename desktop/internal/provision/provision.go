// Package provision is the setup wizard of a user's server (level 1: the user enters the VPS address and the root
// password once in the app). Over SSH as root it runs ten idempotent steps on a Debian 13 host; every step checks,
// acts only when the check says so, then checks again, so a second run continues where the first one stopped:
//
//	connect    root + password, the host key pinned (trust on first use; strict on the next runs)
//	preflight  Debian 13, amd64, root, systemd, memory and disk; anything else is refused
//	incus      Incus from Debian (`incus admin init --minimal`: no network listener, the app never gets the Incus API)
//	container  bridge vibechatbr0 (IPv4 only), profile vibechat (2 CPU, 4 GiB, 15 GiB, security.nesting, proxy
//	           devices 127.0.0.1:18120/18121 on the host), an unprivileged images:debian/13 container
//	server     the vibechat-server .deb and the pinned Bun, both sha256 checked on the host, installed in the container
//	egress     Incus network ACL on vibechatbr0: egress only to the resolved IPs of Cursor, npm and Bun (GitHub
//	           releases), refreshed every 10 minutes, the names pinned in the container's /etc/hosts
//	token      `vibechat-server token new` in the container; the token goes to the app's keychain (Hooks), never
//	           to argv, logs or events
//	tunnel     host user vibechat-tunnel (no shell), the app's key with restrict,port-forwarding,permitopen
//	verify     a new SSH connection with the app's key and the pinned host key: hello and a ticket; snapshot
//	lockdown   sshd: no password login, root by key only; a systemd-run rollback timer is armed first and cancelled
//	           only after the app confirms the key login works (Hooks.Confirm)
//
// The root password stays in the caller's memory (Hooks.Connect); this package never sees it.
package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Step ids, in order (DESKTOP_PROVISION_STEPS of the Studio contract).
const (
	StepConnect   = "connect"
	StepPreflight = "preflight"
	StepIncus     = "incus"
	StepContainer = "container"
	StepServer    = "server"
	StepEgress    = "egress"
	StepToken     = "token"
	StepTunnel    = "tunnel"
	StepVerify    = "verify"
	StepLockdown  = "lockdown"
)

// Steps is the order of the wizard.
var Steps = []string{StepConnect, StepPreflight, StepIncus, StepContainer, StepServer, StepEgress, StepToken, StepTunnel, StepVerify, StepLockdown}

// Step states.
const (
	Pending = "pending"
	Running = "running"
	Done    = "done"
	Skipped = "skipped" // the check passed: nothing to do
	Failed  = "failed"
)

// Problem codes (DesktopProvisionProblemCodeSchema).
const (
	ProblemUnreachable    = "unreachable"
	ProblemAuth           = "auth"
	ProblemHostKeyChanged = "host_key_changed"
	ProblemUnsupported    = "unsupported"
	ProblemStepFailed     = "step_failed"
	ProblemNotConfirmed   = "not_confirmed"
	ProblemBadInput       = "bad_input"
	ProblemBusy           = "busy"
)

// Error is a failed run: the problem code and a message for the user (never a secret).
type Error struct {
	Step, Code, Message string
}

func (e *Error) Error() string { return e.Step + ": " + e.Message }

// Result is the exit of one command on the host. Stdout may hold a secret (the token step): it is never logged.
type Result struct {
	Stdout, Stderr []byte
	Code           int
}

// Runner runs a bash script as root on the host, stdin fed to it. A non-zero exit is a Result, not an error;
// err is the transport.
type Runner interface {
	Run(ctx context.Context, script string, stdin []byte) (Result, error)
}

// Hooks are the app's side of the wizard.
type Hooks struct {
	// Connect opens the root connection (password, host key pinned) and answers its runner.
	Connect func(ctx context.Context) (Runner, error)
	// TokenGet answers the host token in the keychain ("" if none); TokenSet stores a new one.
	TokenGet func() string
	TokenSet func(token string) error
	// Verify connects as the tunnel user with the app's key and the pinned host key: hello and a ticket.
	Verify func(ctx context.Context) error
	// Confirm, after sshd took the lockdown: the app's key login works and the root password is refused.
	Confirm func(ctx context.Context) error
	// Progress reports every state change of a step (detail never carries a secret).
	Progress func(step, state, detail string)
	// Substep reports the state changes of the substeps of a step (Substeps), optional.
	Substep func(step, sub, state string)
	Log     func(string)
}

// Config of one run.
type Config struct {
	Container string // incus instance name, default "vibechat-owner"
	// AppKey is the app's public key line for the tunnel user; RootKey an optional public key line of the user for
	// /root/.ssh/authorized_keys (installed before the lockdown).
	AppKey, RootKey string
	// DebURL and SumsURL: the vibechat-server .deb and the SHA256SUMS listing it (checked on the host).
	DebURL, SumsURL string
	// RollbackAfter: the lockdown safety net (default 5 minutes).
	RollbackAfter time.Duration
	// Domains of the egress allowlist (default DefaultDomains).
	Domains []string
}

// Release assets of an app version (the server's protocol matches the app's).
func ReleaseURLs(version string) (deb, sums string) {
	base := "https://github.com/buildytech/vibechat-desktop/releases/download/v" + strings.TrimPrefix(version, "v") + "/"
	return base + "vibechat-server-linux-amd64.deb", base + "SHA256SUMS"
}

func (c *Config) defaults() error {
	if c.Container == "" {
		c.Container = "vibechat-owner"
	}
	if c.RollbackAfter <= 0 {
		c.RollbackAfter = 5 * time.Minute
	}
	if len(c.Domains) == 0 {
		c.Domains = DefaultDomains
	}
	switch {
	case !nameRe.MatchString(c.Container):
		return fmt.Errorf("%q is not an instance name", c.Container)
	case len(c.AppKey) > 8192 || len(c.RootKey) > 8192:
		return errors.New("an SSH public key line is too long")
	case !keyLineRe.MatchString(c.AppKey):
		return errors.New("the app's SSH public key is missing")
	case c.RootKey != "" && !keyLineRe.MatchString(c.RootKey):
		return errors.New("the root public key is not one OpenSSH public key line (ssh-ed25519 AAAA… or ssh-rsa AAAA…)")
	case !urlRe.MatchString(c.DebURL) || !urlRe.MatchString(c.SumsURL):
		return errors.New("the server package URLs are not http(s) URLs")
	}
	for _, d := range c.Domains {
		if !domainRe.MatchString(d) {
			return fmt.Errorf("%q is not a domain name", d)
		}
	}
	return nil
}

// Run runs the steps in order and stops at the first failure (an *Error).
func Run(ctx context.Context, cfg Config, h Hooks) error {
	if h.Progress == nil {
		h.Progress = func(string, string, string) {}
	}
	if h.Log == nil {
		h.Log = func(string) {}
	}
	if err := cfg.defaults(); err != nil {
		return &Error{Step: StepConnect, Code: ProblemBadInput, Message: err.Error()}
	}
	h.Progress(StepConnect, Running, "")
	r, err := h.Connect(ctx)
	if err != nil {
		var pe *Error
		if errors.As(err, &pe) {
			h.Progress(StepConnect, Failed, pe.Message)
			return pe
		}
		h.Progress(StepConnect, Failed, err.Error())
		return &Error{Step: StepConnect, Code: ProblemUnreachable, Message: err.Error()}
	}
	h.Progress(StepConnect, Done, "")
	w := &wizard{cfg: cfg, h: h, r: r}
	for _, s := range w.steps() {
		if err := w.run(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// step: check (exit 0 done, exit todoExit to do, anything else an error), then apply, then check again. A step
// without apply is a check only (preflight, verify): it is done when the check passes.
type step struct {
	id    string
	check func(ctx context.Context) (bool, error)
	apply func(ctx context.Context) error
}

type wizard struct {
	cfg  Config
	h    Hooks
	r    Runner
	done map[string]string // step → time, for state.json
}

func (w *wizard) finished(ctx context.Context, id string) {
	if w.done == nil {
		w.done = map[string]string{}
	}
	w.done[id] = time.Now().UTC().Format(time.RFC3339)
	w.record(ctx, w.done)
}

func (w *wizard) run(ctx context.Context, s step) error {
	w.h.Progress(s.id, Running, "")
	fail := func(err error) error {
		var pe *Error
		if !errors.As(err, &pe) {
			pe = &Error{Step: s.id, Code: ProblemStepFailed, Message: err.Error()}
		}
		pe.Step = s.id
		w.h.Progress(s.id, Failed, pe.Message)
		w.h.Log(fmt.Sprintf("provision: %s failed: %s", s.id, pe.Message))
		return pe
	}
	ok, err := s.check(ctx)
	if err != nil {
		return fail(err)
	}
	if ok && s.apply != nil {
		w.skipSubs(s.id)
		w.h.Progress(s.id, Skipped, "")
		w.h.Log("provision: " + s.id + ": already done")
		w.finished(ctx, s.id)
		return nil
	}
	if !ok {
		if s.apply == nil {
			return fail(errors.New("the check did not pass"))
		}
		if err := s.apply(ctx); err != nil {
			return fail(err)
		}
		ok, err = s.check(ctx)
		if err != nil {
			return fail(err)
		}
		if !ok {
			return fail(errors.New("done, but the check after it does not pass"))
		}
	}
	w.h.Progress(s.id, Done, "")
	w.h.Log("provision: " + s.id + ": done")
	w.finished(ctx, s.id)
	return nil
}

// todoExit is the check scripts' "not done yet"; refuseExit preflight's "not supported".
const (
	todoExit   = 3
	refuseExit = 4
)

// sh runs a script and answers an error carrying the last stderr lines on a non-zero exit.
func (w *wizard) sh(ctx context.Context, script string, stdin []byte) (Result, error) {
	res, err := w.r.Run(ctx, script, stdin)
	if err != nil {
		return res, err
	}
	if res.Code != 0 {
		return res, fmt.Errorf("exit %d: %s", res.Code, tail(res.Stderr))
	}
	return res, nil
}

// checkScript: exit 0 → done, todoExit → to do, else an error.
func (w *wizard) checkScript(ctx context.Context, script string, stdin []byte) (bool, error) {
	res, err := w.r.Run(ctx, script, stdin)
	if err != nil {
		return false, err
	}
	switch res.Code {
	case 0:
		return true, nil
	case todoExit:
		return false, nil
	}
	return false, fmt.Errorf("check exit %d: %s", res.Code, tail(res.Stderr))
}

// tail is the last lines of a command's stderr for a message (at most 600 bytes).
func tail(b []byte) string {
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(l, SubMarker) {
			lines = append(lines, l)
		}
	}
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	s := strings.TrimSpace(strings.Join(lines, "\n"))
	if len(s) > 600 {
		s = s[len(s)-600:]
	}
	if s == "" {
		return "no output"
	}
	return s
}
