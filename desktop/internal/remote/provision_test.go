package remote

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func provisionManager(t *testing.T) (*Manager, *bytes.Buffer, *sync.Mutex) {
	var log bytes.Buffer
	var mu sync.Mutex
	m, err := New(Options{
		DataDir: t.TempDir(), Creds: &MemCreds{}, AppVersion: "0.1.7", AppProtocol: 1, MinServerProtocol: 1, Prefix: "/runtime/v1",
		Log: func(s string) { mu.Lock(); log.WriteString(s + "\n"); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, &log, &mu
}

func post(t *testing.T, m *Manager, body string) (int, Provision) {
	t.Helper()
	rec := httptest.NewRecorder()
	h := m.Middleware(http.NotFoundHandler())
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/runtime/v1/desktop/provision", strings.NewReader(body)))
	var p Provision
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return rec.Code, p
}

func waitProvision(t *testing.T, m *Manager) Provision {
	t.Helper()
	for range 200 {
		if p := m.Provision(); p.State != "running" {
			return p
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the setup is still running")
	return Provision{}
}

func TestProvisionInputIsChecked(t *testing.T) {
	m, _, _ := provisionManager(t)
	if p := m.Provision(); p.State != "idle" || len(p.Steps) != 10 || p.RollbackSeconds != 300 {
		t.Fatalf("%+v", p)
	}
	for _, body := range []string{`{"address":"","password":"x"}`, `{"address":"bad host!","password":"x"}`, `{"address":"203.0.113.5","password":""}`} {
		code, p := post(t, m, body)
		if code != 200 || p.State != "failed" || p.Problem == nil || p.Problem.Code != "bad_input" {
			t.Errorf("%s: %d %+v", body, code, p)
		}
	}
	if code, _ := post(t, m, `{"address":`); code != 400 {
		t.Errorf("bad JSON: %d", code)
	}
	m.mu.Lock()
	m.provRunning, m.prov = true, &Provision{State: "running"}
	m.mu.Unlock()
	if code, p := post(t, m, `{"address":"203.0.113.5","password":"pw"}`); code != 409 || p.Problem.Code != "busy" {
		t.Errorf("busy: %d %+v", code, p)
	}
}

// Over real SSH (the in-process server): root with the password, the host key pinned, a refusal of the preflight
// reaches the progress; the password is nowhere in the answers or the log.
func TestProvisionOverSSHStopsAtPreflight(t *testing.T) {
	const pw = "correct horse battery staple"
	d := startSSHD(t, "127.0.0.1:0", newHostKey(t), nil)
	var scripts []string
	var smu sync.Mutex
	d.password = pw
	d.exec = func(cmd string, stdin []byte) (string, string, uint32) {
		smu.Lock()
		scripts = append(scripts, cmd)
		smu.Unlock()
		if strings.Contains(cmd, "vibechat-step:preflight:check") {
			return "", "refuse: Debian 13 is required, this server runs Ubuntu 24.04.1 LTS\n", 4
		}
		return "", "", 0
	}
	m, log, lmu := provisionManager(t)
	code, p := post(t, m, `{"address":"`+d.addr()+`","password":"`+pw+`"}`)
	if code != 200 || p.State != "running" {
		t.Fatalf("%d %+v", code, p)
	}
	p = waitProvision(t, m)
	if p.State != "failed" || p.Problem == nil || p.Problem.Code != "unsupported" || !strings.Contains(p.Problem.Message, "Ubuntu") {
		t.Fatalf("%+v", p)
	}
	states := map[string]string{}
	for _, s := range p.Steps {
		states[s.ID] = s.State
	}
	if states["connect"] != "done" || states["preflight"] != "failed" || states["incus"] != "pending" {
		t.Errorf("%v", states)
	}
	if !strings.HasPrefix(p.HostKeyFingerprint, "SHA256:") || m.Connection().Target == nil || m.Connection().Target.HostKeyFingerprint != p.HostKeyFingerprint {
		t.Errorf("pin: %+v %+v", p, m.Connection().Target)
	}
	if m.Connection().Target.User != "vibechat-tunnel" {
		t.Errorf("target %+v", m.Connection().Target)
	}
	raw, _ := json.Marshal(m.Provision())
	lmu.Lock()
	all := string(raw) + log.String()
	lmu.Unlock()
	smu.Lock()
	all += strings.Join(scripts, "\n")
	smu.Unlock()
	if strings.Contains(all, pw) {
		t.Fatal("the root password leaked into the answer, the log or a command")
	}

	// A wrong password: auth, at the connect step.
	code, _ = post(t, m, `{"address":"`+d.addr()+`","password":"wrong"}`)
	if code != 200 {
		t.Fatal(code)
	}
	p = waitProvision(t, m)
	if p.Problem == nil || p.Problem.Code != "auth" || p.Steps[0].State != "failed" {
		t.Fatalf("%+v", p)
	}

	// Another host key at the same address: refused (the pin is strict).
	d.close()
	d2 := startSSHD(t, d.addr(), newHostKey(t), nil)
	d2.password, d2.exec = pw, d.exec
	post(t, m, `{"address":"`+d.addr()+`","password":"`+pw+`"}`)
	p = waitProvision(t, m)
	if p.Problem == nil || p.Problem.Code != "host_key_changed" {
		t.Fatalf("%+v", p)
	}
}
