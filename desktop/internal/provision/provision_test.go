package provision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeHost models the host's state per step: a check passes once its apply ran. Every script must parse (bash -n).
type fakeHost struct {
	t        *testing.T
	done     map[string]bool
	refuse   string
	noEffect string // a step whose apply leaves the check failing
	token    string // the server's current token
	n        int
	timer    bool // the rollback timer is armed
	dropIn   bool
	calls    []string
	scripts  []string
}

var markerRe = regexp.MustCompile(`^# vibechat-step:([a-z]+):([a-z]+)\n`)

func newFake(t *testing.T) *fakeHost { return &fakeHost{t: t, done: map[string]bool{}} }

func (f *fakeHost) Run(_ context.Context, script string, stdin []byte) (Result, error) {
	m := markerRe.FindStringSubmatch(script)
	if m == nil {
		f.t.Fatalf("script without marker:\n%s", script)
	}
	if out, err := exec.Command("bash", "-n", "-c", script).CombinedOutput(); err != nil {
		f.t.Fatalf("%s:%s does not parse: %v\n%s\n%s", m[1], m[2], err, out, script)
	}
	id, phase := m[1], m[2]
	f.calls = append(f.calls, id+":"+phase)
	f.scripts = append(f.scripts, script)
	code := func(ok bool) Result {
		if ok {
			return Result{}
		}
		return Result{Code: todoExit}
	}
	switch id + ":" + phase {
	case "preflight:check":
		if f.refuse != "" {
			return Result{Stderr: []byte("refuse: " + f.refuse + "\n"), Code: refuseExit}, nil
		}
		return Result{}, nil
	case "token:check":
		return code(f.token != "" && string(stdin) == "Authorization: Bearer "+f.token+"\n"), nil
	case "token:apply":
		f.n++
		f.token = fmt.Sprintf("tok%dsecretvalue", f.n)
		return Result{Stdout: []byte(f.token + "\n")}, nil
	case "verify:doctor", "verify:snapshot", "state:write":
		return Result{}, nil
	case "lockdown:check":
		return code(f.dropIn && !f.timer), nil
	case "lockdown:arm":
		f.timer = true
		return Result{}, nil
	case "lockdown:apply":
		f.dropIn = true
		return Result{}, nil
	case "lockdown:cancel":
		f.timer = false
		return Result{}, nil
	}
	switch phase {
	case "check":
		return code(f.done[id]), nil
	case "apply":
		if id != f.noEffect {
			f.done[id] = true
		}
		return Result{}, nil
	}
	f.t.Fatalf("unexpected %s:%s", id, phase)
	return Result{}, nil
}

func (f *fakeHost) applied() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasSuffix(c, ":apply") {
			out = append(out, strings.TrimSuffix(c, ":apply"))
		}
	}
	return out
}

type app struct {
	token     string
	confirm   error
	order     []string
	states    map[string]string
	details   []string
	logs      []string
	verifyErr error
}

func (a *app) hooks(r Runner) Hooks {
	a.states = map[string]string{}
	return Hooks{
		Connect:  func(context.Context) (Runner, error) { return r, nil },
		TokenGet: func() string { return a.token },
		TokenSet: func(tok string) error { a.token = tok; return nil },
		Verify:   func(context.Context) error { a.order = append(a.order, "verify"); return a.verifyErr },
		Confirm:  func(context.Context) error { a.order = append(a.order, "confirm"); return a.confirm },
		Progress: func(step, state, detail string) {
			a.states[step] = state
			a.details = append(a.details, detail)
		},
		Log: func(s string) { a.logs = append(a.logs, s) },
	}
}

func cfg() Config {
	return Config{
		AppKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGq3mVt5Hq2wq7o0nQe1bW8E2k9h2f3dC4mUe6rJk1aB vibechat-desktop",
		DebURL: "https://example.test/v0.1.7/vibechat-server-linux-amd64.deb", SumsURL: "https://example.test/v0.1.7/SHA256SUMS",
	}
}

func TestRunTwiceIsIdempotent(t *testing.T) {
	f := newFake(t)
	a := &app{}
	if err := Run(context.Background(), cfg(), a.hooks(f)); err != nil {
		t.Fatal(err)
	}
	want := []string{StepIncus, StepContainer, StepServer, StepEgress, StepToken, StepTunnel, StepLockdown}
	if got := f.applied(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("first run applied %v, want %v", got, want)
	}
	for _, s := range Steps {
		if a.states[s] != Done {
			t.Errorf("first run: %s %s", s, a.states[s])
		}
	}
	first := a.token
	f.calls = nil
	if err := Run(context.Background(), cfg(), a.hooks(f)); err != nil {
		t.Fatal(err)
	}
	if got := f.applied(); len(got) != 0 {
		t.Fatalf("second run applied %v", got)
	}
	for _, s := range Steps {
		want := Skipped
		if s == StepConnect || s == StepPreflight || s == StepVerify {
			want = Done // check-only steps run every time
		}
		if a.states[s] != want {
			t.Errorf("second run: %s %s, want %s", s, a.states[s], want)
		}
	}
	if a.token != first {
		t.Error("the second run made a new token")
	}
}

func TestEachStepContinuesWhereItStopped(t *testing.T) {
	for i, stop := range []string{StepIncus, StepContainer, StepServer, StepEgress, StepTunnel} {
		f := newFake(t)
		f.noEffect = stop
		a := &app{}
		err := Run(context.Background(), cfg(), a.hooks(f))
		var pe *Error
		if !errors.As(err, &pe) || pe.Step != stop || pe.Code != ProblemStepFailed || a.states[stop] != Failed {
			t.Fatalf("%d %s: %v %+v", i, stop, err, a.states)
		}
		f.noEffect, f.calls = "", nil
		if err := Run(context.Background(), cfg(), a.hooks(f)); err != nil {
			t.Fatalf("rerun after %s: %v", stop, err)
		}
		if got := f.applied(); len(got) == 0 || got[0] != stop {
			t.Errorf("rerun after %s applied %v", stop, got)
		}
	}
}

func TestPreflightRefusesOtherSystems(t *testing.T) {
	f := newFake(t)
	f.refuse = "Debian 13 is required, this server runs Ubuntu 24.04.1 LTS"
	a := &app{}
	err := Run(context.Background(), cfg(), a.hooks(f))
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != ProblemUnsupported || pe.Step != StepPreflight || !strings.Contains(pe.Message, "Ubuntu") {
		t.Fatalf("%v", err)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "preflight:") {
			t.Errorf("ran %s after a refusal", c)
		}
	}
	if a.states[StepIncus] != "" {
		t.Error("incus started")
	}
}

func TestTokenGoesToTheKeychainOnly(t *testing.T) {
	f := newFake(t)
	a := &app{}
	if err := Run(context.Background(), cfg(), a.hooks(f)); err != nil {
		t.Fatal(err)
	}
	if a.token == "" || a.token != f.token {
		t.Fatalf("keychain %q, server %q", a.token, f.token)
	}
	all := strings.Join(f.scripts, "\n") + strings.Join(a.details, "\n") + strings.Join(a.logs, "\n")
	if strings.Contains(all, a.token) {
		t.Fatal("the token is in a script, a progress detail or the log")
	}
	// A token the server no longer accepts (reinstalled server): a new one.
	f.token = "changed"
	f.calls = nil
	if err := Run(context.Background(), cfg(), a.hooks(f)); err != nil {
		t.Fatal(err)
	}
	if got := f.applied(); strings.Join(got, ",") != StepToken {
		t.Errorf("applied %v", got)
	}
}

func TestLockdownWaitsForTheAppsConfirmation(t *testing.T) {
	f := newFake(t)
	a := &app{confirm: errors.New("key login refused")}
	err := Run(context.Background(), cfg(), a.hooks(f))
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != ProblemNotConfirmed || pe.Step != StepLockdown {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(pe.Message, "5m0s") {
		t.Errorf("message %q", pe.Message)
	}
	if !f.timer {
		t.Fatal("the rollback timer was cancelled without a confirmation")
	}
	var lock []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "lockdown:") {
			lock = append(lock, c)
		}
	}
	if strings.Join(lock, ",") != "lockdown:check,lockdown:arm,lockdown:apply" {
		t.Errorf("lockdown calls %v", lock)
	}
	// The timer fired (password login back); the next run confirms and cancels it.
	f.timer, f.dropIn, f.calls = false, false, nil
	a.confirm, a.order = nil, nil
	if err := Run(context.Background(), cfg(), a.hooks(f)); err != nil {
		t.Fatal(err)
	}
	lock = nil
	for _, c := range f.calls {
		if strings.HasPrefix(c, "lockdown:") {
			lock = append(lock, c)
		}
	}
	if strings.Join(lock, ",") != "lockdown:check,lockdown:arm,lockdown:apply,lockdown:cancel,lockdown:check" || f.timer {
		t.Errorf("lockdown calls %v, timer %v", lock, f.timer)
	}
	if strings.Join(a.order, ",") != "verify,confirm" {
		t.Errorf("order %v", a.order)
	}
}

func TestVerifyFailureStopsBeforeLockdown(t *testing.T) {
	f := newFake(t)
	a := &app{verifyErr: errors.New("no hello")}
	err := Run(context.Background(), cfg(), a.hooks(f))
	var pe *Error
	if !errors.As(err, &pe) || pe.Step != StepVerify {
		t.Fatalf("%v", err)
	}
	if f.dropIn || f.timer {
		t.Fatal("lockdown ran after a failed verify")
	}
}

func scriptsOf(t *testing.T) map[string]string {
	f := newFake(t)
	c := cfg()
	c.RootKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB1rootkeyrootkeyrootkeyrootkeyroo me@laptop"
	if err := Run(context.Background(), c, (&app{}).hooks(f)); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for i, c := range f.calls {
		out[c] += f.scripts[i]
	}
	return out
}

func TestScriptsSayWhatTheArchitectureSays(t *testing.T) {
	s := scriptsOf(t)
	has := func(key string, subs ...string) {
		t.Helper()
		for _, sub := range subs {
			if !strings.Contains(s[key], sub) {
				t.Errorf("%s lacks %q", key, sub)
			}
		}
	}
	has("preflight:check", `"${ID:-}" = debian`, `"${VERSION_ID:-}" = 13`, "amd64")
	has("incus:apply", "apt-get install -y -qq incus", "incus admin init --minimal")
	has("container:apply", "limits.cpu=2 limits.memory=4GiB security.nesting=true", "size=15GiB", "btrfs size=18GiB",
		"listen=tcp:127.0.0.1:18120 connect=tcp:127.0.0.1:18120 bind=host", "listen=tcp:127.0.0.1:18121 connect=tcp:127.0.0.1:18121 bind=host",
		"images:debian/13", "ipv6.address=none", "security.privileged")
	has("server:apply", "sha256sum -c --strict --quiet want", BunSHA256, "apt-get install -y -qq --reinstall /root/vibechat-server.deb")
	has("egress:apply", "security.acls.default.egress.action=reject", `destination_port: "443"`, "registry.npmjs.org", "api.cursor.com", "api2.cursor.sh", "bun.sh",
		"OnUnitActiveSec=10min", "--mode 0644")
	has("token:apply", "vibechat-server token new 2>/dev/null")
	has("tunnel:apply", `LINE='restrict,port-forwarding,permitopen="127.0.0.1:18120",permitopen="127.0.0.1:18121" ssh-ed25519 `, "--shell /usr/sbin/nologin")
	has("lockdown:arm", "systemd-run", `SECS='300'`, `--on-active="${SECS}s"`)
	has("lockdown:apply", "PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin prohibit-password\n", "/usr/sbin/sshd -t", "me@laptop")
	if strings.Contains(s["container:apply"], "security.privileged=true") {
		t.Error("a privileged container")
	}
	for k, v := range s {
		if strings.Contains(v, "core.https_address") || strings.Contains(v, "incus config trust") {
			t.Errorf("%s opens the Incus API", k)
		}
	}
}

func TestConfigIsChecked(t *testing.T) {
	bad := []func(*Config){
		func(c *Config) { c.Container = "Bad Name" },
		func(c *Config) { c.AppKey = "" },
		func(c *Config) { c.RootKey = "ssh-ed25519 AAAA\nPermitRootLogin yes" },
		func(c *Config) { c.DebURL = "file:///etc/passwd" },
		func(c *Config) { c.SumsURL = "https://x/'; reboot; '" },
		func(c *Config) { c.Domains = []string{"evil.com; rm -rf /"} },
	}
	for i, mod := range bad {
		c := cfg()
		mod(&c)
		f := newFake(t)
		err := Run(context.Background(), c, (&app{}).hooks(f))
		var pe *Error
		if !errors.As(err, &pe) || pe.Code != ProblemBadInput || len(f.calls) != 0 {
			t.Errorf("%d: %v, calls %v", i, err, f.calls)
		}
	}
}

func TestQuote(t *testing.T) {
	for _, s := range []string{"", "a b", `it's`, `"$(reboot)"`, "x\ny"} {
		out, err := exec.Command("bash", "-c", "printf %s "+quote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("%q → %q %v", s, out, err)
		}
	}
}

func TestReleaseURLs(t *testing.T) {
	deb, sums := ReleaseURLs("v0.1.7")
	if deb != "https://github.com/buildytech/vibechat-desktop/releases/download/v0.1.7/vibechat-server-linux-amd64.deb" || !strings.HasSuffix(sums, "/v0.1.7/SHA256SUMS") {
		t.Fatal(deb, sums)
	}
	if !urlRe.MatchString(deb) {
		t.Fatal("release URL refused by the config check")
	}
}

func TestDefaults(t *testing.T) {
	c := cfg()
	if err := c.defaults(); err != nil {
		t.Fatal(err)
	}
	if c.Container != "vibechat-owner" || c.RollbackAfter != 5*time.Minute || len(c.Domains) != len(DefaultDomains) {
		t.Fatalf("%+v", c)
	}
	var b bytes.Buffer
	for _, d := range DefaultDomains {
		b.WriteString(d)
	}
	for _, want := range []string{"cursor.sh", "registry.npmjs.org", "bun.sh", "github.com"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("allowlist lacks %s", want)
		}
	}
}
