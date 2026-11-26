package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	vibechat "github.com/buildytech/vibechat-desktop"
	"github.com/buildytech/vibechat-desktop/internal/auth"
	"github.com/buildytech/vibechat-desktop/internal/host"
)

func pinned(t *testing.T) host.Seam {
	t.Helper()
	s, err := host.LoadSeam(vibechat.HostSeam())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSeamAuthMatchesInternalAuth(t *testing.T) {
	if err := checkSeamAuth(pinned(t)); err != nil {
		t.Fatal(err)
	}
}

func TestLoopbackOnly(t *testing.T) {
	for addr, good := range map[string]bool{"127.0.0.1:18120": true, "[::1]:1": true, "0.0.0.0:18120": false, ":18120": false, "localhost:18120": false, "10.0.0.5:80": false, "127.0.0.1:0": false} {
		if _, err := loopbackAddr(addr); (err == nil) != good {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

// frontDoor is the server's real handler stack (auth + session exchange + seam/Studio split) over the real host
// seam router, with a fake Studio and an SSE route of the fake seam.
func frontDoor(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	s := pinned(t)
	tok, _ := auth.NewToken()
	ta := auth.NewTokenAuth(tok)
	h := host.New(host.Options{Seam: s, Version: "0.2.0", InstanceID: "0123456789abcdef0123456789abcdef", PreviewPort: 18121, Tickets: ta})
	seam := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == s.Prefix+"/test-sse" {
			w.Header().Set("content-type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: one\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		h.ServeHTTP(w, r)
	})
	studio := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("studio")) })
	srv := httptest.NewServer(apiHandler(s, seam, ta, studio))
	t.Cleanup(srv.Close)
	return srv, tok
}

func call(t *testing.T, method, url string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(res.Body)
	return res, b.String()
}

func TestFrontDoor(t *testing.T) {
	srv, tok := frontDoor(t)
	bearer := map[string]string{"Authorization": "Bearer " + tok}

	res, body := call(t, "GET", srv.URL+"/runtime/v1/host/hello", nil)
	var hello host.Hello
	if res.StatusCode != 200 || json.Unmarshal([]byte(body), &hello) != nil || hello.Protocol != 1 || hello.InstanceID == "" || hello.PreviewPort != 18121 || hello.ServerVersion != "0.2.0" {
		t.Fatalf("hello: %d %s", res.StatusCode, body)
	}
	for _, path := range []string{"/", "/p/x", "/runtime/v1/host/capabilities", "/runtime/v1/projects"} {
		if res, _ := call(t, "GET", srv.URL+path, nil); res.StatusCode != 401 {
			t.Errorf("%s without auth: %d", path, res.StatusCode)
		}
	}
	if res, _ := call(t, "POST", srv.URL+"/runtime/v1/host/hello", nil); res.StatusCode != 401 {
		t.Errorf("only GET hello is public: %d", res.StatusCode)
	}
	if res, _ := call(t, "GET", srv.URL+"/runtime/v1/host/hello", map[string]string{"Host": "attacker.example:18120"}); res.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("rebinding Host: %d", res.StatusCode)
	}

	res, body = call(t, "POST", srv.URL+"/runtime/v1/auth/ticket", bearer)
	var tk struct{ Ticket, ExpiresAt string }
	if res.StatusCode != 200 || json.Unmarshal([]byte(body), &tk) != nil || tk.Ticket == "" {
		t.Fatalf("ticket: %d %s", res.StatusCode, body)
	}
	if exp, err := time.Parse(time.RFC3339, tk.ExpiresAt); err != nil || time.Until(exp) > auth.TicketTTL+time.Second {
		t.Fatalf("expiresAt %q", tk.ExpiresAt)
	}
	res, _ = call(t, "GET", srv.URL+"/auth/session?ticket="+tk.Ticket, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("session: %d", res.StatusCode)
	}
	var cookie string
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookie && c.HttpOnly && c.SameSite == http.SameSiteStrictMode {
			cookie = c.Name + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatal("no HttpOnly SameSite=Strict session cookie")
	}
	sess := map[string]string{"Cookie": cookie}
	if res, body := call(t, "GET", srv.URL+"/", sess); res.StatusCode != 200 || body != "studio" {
		t.Fatalf("studio: %d %s", res.StatusCode, body)
	}
	if res, body := call(t, "GET", srv.URL+"/runtime/v1/host/capabilities", sess); res.StatusCode != 200 || !strings.Contains(body, `"seam"`) {
		t.Fatalf("capabilities: %d %s", res.StatusCode, body)
	}
	if res, _ := call(t, "POST", srv.URL+"/runtime/v1/auth/ticket", sess); res.StatusCode != 401 {
		t.Fatalf("the window session minted a ticket: %d", res.StatusCode)
	}
	if res, _ := call(t, "GET", srv.URL+"/auth/session?ticket="+tk.Ticket, nil); res.StatusCode != 401 {
		t.Fatalf("ticket reused: %d", res.StatusCode)
	}
}

// TestSSEThroughTheServer: the event stream reaches the client while the handler is still open (net/http flushes
// through the auth middleware and the seam split; the Wails Windows asset server did not).
func TestSSEThroughTheServer(t *testing.T) {
	srv, tok := frontDoor(t)
	req, _ := http.NewRequest("GET", srv.URL+"/runtime/v1/test-sse", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := srv.Client().Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	line := make(chan string, 1)
	go func() { l, _ := bufio.NewReader(res.Body).ReadString('\n'); line <- l }()
	select {
	case l := <-line:
		if l != "data: one\n" {
			t.Fatalf("got %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not flushed")
	}
}

func TestTokenNewAndSetKey(t *testing.T) {
	data := filepath.Join(t.TempDir(), "vibechat")
	t.Setenv("VIBECHAT_DATA_DIR", data)
	var out, errb bytes.Buffer
	if code := run([]string{"token", "new"}, nil, &out, &errb); code != 0 {
		t.Fatalf("token new: %d %s", code, errb.String())
	}
	tok := strings.TrimSpace(out.String())
	p := serverPaths(os.Getenv)
	raw, _ := os.ReadFile(p.tokenHash())
	if len(tok) != 43 || strings.Contains(string(raw), tok) || strings.TrimSpace(string(raw)) != auth.HashToken(tok) {
		t.Fatal("token new must print the token and store only its sha256")
	}
	if code := run([]string{"set-key"}, strings.NewReader("test-cursor-key\n"), &out, &errb); code != 0 {
		t.Fatalf("set-key: %s", errb.String())
	}
	if k, src := p.keys().Key(); k != "test-cursor-key" || src != "file" {
		t.Fatalf("key %q %q", k, src)
	}
	if code := run([]string{"set-key"}, strings.NewReader("bad key\n"), &out, &errb); code == 0 {
		t.Fatal("a key with a space was stored")
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{p.tokenHash(), p.cursorKey()} {
			if st, _ := os.Stat(f); st.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %v", f, st.Mode().Perm())
			}
		}
	}
	a, _ := instanceID(p)
	b, _ := instanceID(p)
	if len(a) != 32 || a != b {
		t.Fatalf("instance id %q %q", a, b)
	}
}

func fakeGitHub(t *testing.T, tag string, deb []byte, sum string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tag})
	})
	mux.HandleFunc("/o/r/releases/download/"+tag+"/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  %s\n%s  other.zip\n", sum, debAsset(), strings.Repeat("0", 64))
	})
	mux.HandleFunc("/o/r/releases/download/"+tag+"/"+debAsset(), func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(deb) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestUpdaterVerifiesTheSum(t *testing.T) {
	deb := []byte("!<arch>\nfake deb")
	h := sha256.Sum256(deb)
	good := hex.EncodeToString(h[:])
	newU := func(srv *httptest.Server, current string, installed *[]byte) *updater {
		return &updater{api: srv.URL, download: srv.URL, repo: "o/r", client: srv.Client(), current: current, out: &bytes.Buffer{},
			install: func(f string) error { b, err := os.ReadFile(f); *installed = b; return err }}
	}
	var got []byte
	if err := newU(fakeGitHub(t, "v0.2.0", deb, good), "0.2.0", &got).run(context.Background(), false); err != errUpToDate || got != nil {
		t.Fatalf("same version: %v", err)
	}
	if err := newU(fakeGitHub(t, "v0.1.9", deb, good), "0.2.0", &got).run(context.Background(), false); err != errUpToDate || got != nil {
		t.Fatalf("older release: %v", err)
	}
	if err := newU(fakeGitHub(t, "v0.3.0", deb, good), "0.2.0", &got).run(context.Background(), true); err != nil || got != nil {
		t.Fatalf("--check installed: %v", err)
	}
	if err := newU(fakeGitHub(t, "v0.3.0", deb, strings.Repeat("a", 64)), "0.2.0", &got).run(context.Background(), false); err == nil || got != nil {
		t.Fatalf("bad sum installed: %v", err)
	}
	if err := newU(fakeGitHub(t, "v0.3.0", deb, good), "dev", &got).run(context.Background(), false); err == nil || got != nil {
		t.Fatal("a dev build updated itself")
	}
	if err := newU(fakeGitHub(t, "v0.10.0", deb, good), "0.9.1", &got).run(context.Background(), false); err != nil || !bytes.Equal(got, deb) {
		t.Fatalf("update: %v", err)
	}
}

func TestOSCheckWantsDebian13(t *testing.T) {
	debian := "PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nNAME=\"Debian GNU/Linux\"\nVERSION_ID=\"13\"\nID=debian\n"
	if c := osCheck(debian); c.level != "ok" || !strings.Contains(c.detail, "trixie") {
		t.Fatalf("debian 13: %+v", c)
	}
	for _, other := range []string{
		"PRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n",
		"PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nID=debian\nVERSION_ID=\"12\"\n",
		"",
	} {
		if c := osCheck(other); c.level != "warn" || !strings.Contains(c.detail, "NOT SUPPORTED") || !strings.Contains(c.detail, "Debian 13") {
			t.Errorf("%q: %+v", other, c)
		}
	}
}
