package remote

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/buildytech/vibechat-desktop/desktop/internal/provision"
)

func TestScrubberRedactsSecrets(t *testing.T) {
	pw := []byte("hunter2-root")
	tok := []byte(nil)
	scrub := scrubber(func() []byte { return pw }, func() []byte { return tok })
	if got := scrub("auth hunter2-root failed"); got != "auth [redacted] failed" {
		t.Errorf("%q", got)
	}
	tok = []byte("vct_abcdef0123456789")
	if got := scrub("Bearer vct_abcdef0123456789 and hunter2-root"); strings.Contains(got, "vct_") || strings.Contains(got, "hunter2") {
		t.Errorf("%q", got)
	}
	for i := range pw {
		pw[i] = 0
	}
	if got := scrub("plain line"); got != "plain line" {
		t.Errorf("a zeroed password changes lines: %q", got)
	}
}

// The progress carries the run's log, the step and substep times and the app's clock; a secret that a host
// command echoes (here the refusal of the preflight) is redacted in the log, the step detail and the problem.
func TestProvisionLogAndTimesWithoutSecrets(t *testing.T) {
	const pw = "s3cret-root-password"
	d := startSSHD(t, "127.0.0.1:0", newHostKey(t), nil)
	d.password = pw
	d.exec = func(cmd string, stdin []byte) (string, string, uint32) {
		if strings.Contains(cmd, "vibechat-step:preflight:check") {
			return "", "refuse: the password " + pw + " is in this line\n", 4
		}
		return "", "", 0
	}
	m, log, lmu := provisionManager(t)
	if p := m.Provision(); p.Now == 0 || p.Log == nil || len(p.Steps[2].Subs) != len(provision.Substeps["incus"]) || len(p.Steps[3].Subs) != len(provision.Substeps["container"]) {
		t.Fatalf("idle %+v", p)
	}
	if code, _ := post(t, m, `{"address":"`+d.addr()+`","password":"`+pw+`"}`); code != 200 {
		t.Fatal(code)
	}
	p := waitProvision(t, m)
	if p.StartedAt == 0 || p.Now < p.StartedAt {
		t.Errorf("times %d %d", p.StartedAt, p.Now)
	}
	c := p.Steps[0]
	if c.ID != "connect" || c.StartedAt == 0 || c.EndedAt < c.StartedAt {
		t.Errorf("connect %+v", c)
	}
	var lines []string
	for _, l := range p.Log {
		if l.At == 0 {
			t.Errorf("line without time %+v", l)
		}
		lines = append(lines, l.Text)
	}
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "started") || !strings.Contains(text, "pinned the host key") || !strings.Contains(text, "[redacted]") {
		t.Errorf("log:\n%s", text)
	}
	raw, _ := json.Marshal(p)
	lmu.Lock()
	all := string(raw) + log.String()
	lmu.Unlock()
	if strings.Contains(all, pw) {
		t.Fatalf("the root password is in the progress or the log:\n%s", all)
	}
}

func TestSubstepTimesAndLogCap(t *testing.T) {
	m, _, _ := provisionManager(t)
	p := idleProvision(m.provisionOptions().RollbackAfter)
	m.mu.Lock()
	m.prov = &p
	m.mu.Unlock()
	m.setSub("incus", "apt-update", provision.Running)
	m.setSub("incus", "apt-update", provision.Done)
	m.setSub("container", "wait-dns", provision.Skipped)
	got := m.Provision()
	a := got.Steps[2].Subs[0]
	if a.State != "done" || a.StartedAt == 0 || a.EndedAt < a.StartedAt {
		t.Errorf("apt-update %+v", a)
	}
	if w := got.Steps[3].Subs[4]; w.ID != "wait-dns" || w.State != "skipped" || w.EndedAt == 0 {
		t.Errorf("wait-dns %+v", w)
	}
	// The answer is a copy: a later change does not reach it.
	m.setSub("incus", "admin-init", provision.Running)
	if got.Steps[2].Subs[2].State != "pending" {
		t.Error("the answer shares the substeps")
	}
	var wg sync.WaitGroup
	for range provisionLogMax + 50 {
		wg.Add(1)
		go func() { defer wg.Done(); m.provLog("line") }()
	}
	wg.Wait()
	if n := len(m.Provision().Log); n != provisionLogMax {
		t.Errorf("log has %d lines", n)
	}
}
