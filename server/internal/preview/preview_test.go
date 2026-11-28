package preview

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/templates"
)

// TestHelperDevServer is the fake dev server (re-executed test binary): it answers pages and echoes one WebSocket-ish
// upgraded line, and reports the Host and HMR env it saw.
func TestHelperDevServer(t *testing.T) {
	if os.Getenv("VIBECHAT_FAKE_DEV") != "1" {
		t.Skip("helper")
	}
	port := os.Args[len(os.Args)-1]
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			hj, _ := w.(http.Hijacker)
			c, rw, _ := hj.Hijack()
			defer c.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = rw.Flush()
			line, _ := rw.ReadString('\n')
			_, _ = rw.WriteString("echo " + line)
			_ = rw.Flush()
			return
		}
		fmt.Fprintf(w, "page %s host=%s hmr=%s q=%s cookie=%q", r.URL.Path, r.Host, os.Getenv("HMR_CLIENT_PORT"), r.URL.RawQuery, r.Header.Get("Cookie"))
	})
	_ = http.ListenAndServe("127.0.0.1:"+port, nil)
}

func fakeProfile() templates.Profile {
	return templates.Profile{ID: "fake", Dev: []string{os.Args[0], "-test.run=^TestHelperDevServer$", "--", "{port}"},
		DevEnv: map[string]string{"VIBECHAT_FAKE_DEV": "1", "HMR_CLIENT_PORT": "{hmrPort}"}}
}

func startFake(t *testing.T) (*Manager, string) {
	t.Helper()
	// TempDir first: cleanups run last-in first-out, so the dev server stops before its folder is removed
	// (Windows cannot remove a folder a live process works in).
	dir := t.TempDir()
	m := New(func(s string) { t.Log(s) })
	t.Cleanup(m.StopAll)
	st, err := m.Ensure("p1", dir, fakeProfile())
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "starting" && st.Status != "running" {
		t.Fatal(st)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !m.Status("p1").Ready {
		if time.Now().After(deadline) {
			t.Fatalf("never ready: %+v", m.Status("p1"))
		}
		time.Sleep(100 * time.Millisecond)
	}
	return m, strings.TrimSuffix(m.Status("p1").URL, "/")
}

func get(t *testing.T, c *http.Client, u string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestProxyGrantCookieAndSameOrigin(t *testing.T) {
	m, origin := startFake(t)
	bare := &http.Client{}
	if code, _ := get(t, bare, origin+"/", nil); code != 403 {
		t.Fatalf("no grant: %d", code)
	}
	if code, _ := get(t, bare, origin+"/", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}); code != 403 {
		t.Fatal("cross-site allowed")
	}
	grant, _, err := m.Grant("p1")
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	code, body := get(t, c, grant, nil)
	if code != 200 || !strings.Contains(body, "page /") || strings.Contains(body, GrantParam) || strings.Contains(body, "vibechat_preview") {
		t.Fatalf("grant: %d %s", code, body)
	}
	port := strings.TrimPrefix(origin, "http://127.0.0.1:")
	if !strings.Contains(body, "hmr="+port) || !strings.Contains(body, "host=127.0.0.1:") {
		t.Fatalf("env/host: %s", body)
	}
	if code, _ := get(t, bare, grant, nil); code != 403 {
		t.Fatal("grant reused")
	}
	if code, _ := get(t, c, origin+"/src/main.ts", nil); code != 200 {
		t.Fatal("cookie not accepted")
	}
	if code, _ := get(t, bare, origin+"/src/main.ts", map[string]string{"Sec-Fetch-Site": "same-origin"}); code != 200 {
		t.Fatal("same-origin denied")
	}
	if code, _ := get(t, bare, origin+"/x", map[string]string{"Referer": origin + "/"}); code != 200 {
		t.Fatal("referer denied")
	}
	req, _ := http.NewRequest("GET", origin+"/", nil)
	req.Host = "evil.example:" + port
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if res, err := bare.Do(req); err != nil || res.StatusCode != 403 {
		t.Fatal("foreign Host allowed")
	}
}

func TestProxyWebSocket(t *testing.T) {
	_, origin := startFake(t)
	conn, err := net.Dial("tcp", strings.TrimPrefix(origin, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /?token=x HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nOrigin: %s\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", strings.TrimPrefix(origin, "http://"), origin)
	rd := bufio.NewReader(conn)
	status, _ := rd.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("upgrade: %q", status)
	}
	for {
		l, _ := rd.ReadString('\n')
		if l == "\r\n" || l == "" {
			break
		}
	}
	fmt.Fprint(conn, "ping\n")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if l, _ := rd.ReadString('\n'); l != "echo ping\n" {
		t.Fatalf("echo %q", l)
	}
}

func TestStopKillsDevServer(t *testing.T) {
	m, origin := startFake(t)
	m.Stop("p1")
	if st := m.Status("p1"); st.Status != "stopped" {
		t.Fatal(st)
	}
	if _, err := http.Get(origin + "/"); err == nil {
		t.Fatal("proxy still up")
	}
	if _, _, err := m.Grant("p1"); err == nil {
		t.Fatal("grant after stop")
	}
}

func TestAllowFramingDropsFrameProtections(t *testing.T) {
	h := http.Header{}
	h.Set("X-Frame-Options", "DENY")
	h.Add("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'")
	h.Add("Content-Security-Policy", "frame-ancestors 'self'")
	if err := allowFraming(&http.Response{Header: h}); err != nil {
		t.Fatal(err)
	}
	if h.Get("X-Frame-Options") != "" {
		t.Fatal("X-Frame-Options kept")
	}
	got := h.Values("Content-Security-Policy")
	if len(got) != 1 || got[0] != "default-src 'self'; form-action 'self'" {
		t.Fatalf("csp: %q", got)
	}
}
