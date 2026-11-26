package auth

import (
	"bufio"
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
)

const prefix = "/runtime/v1"

// server is the remote host's shape: hello public, ticket by Bearer, /auth/session, everything else guarded; an SSE
// route proves the middleware keeps http.Flusher on a real net/http server.
func server(t *testing.T, a *TokenAuth) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/host/hello", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"seam":"1"}`)) })
	mux.HandleFunc(prefix+"/auth/ticket", func(w http.ResponseWriter, r *http.Request) {
		tk, exp, err := a.IssueTicket(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ticket": tk, "expiresAt": exp.Format(time.RFC3339)})
	})
	mux.Handle(SessionPath, a.SessionHandler())
	mux.HandleFunc(prefix+"/projects", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	mux.HandleFunc(prefix+"/sse", func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: one\n\n")
		f.Flush()
		<-r.Context().Done() // the client must see "one" while the handler is still open
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("studio")) })
	public := func(r *http.Request) bool {
		return r.URL.Path == prefix+"/host/hello" || r.URL.Path == SessionPath
	}
	srv := httptest.NewServer(Middleware(a, prefix, public, mux))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestTokenIsRandomAndOnlyItsHashIsStored(t *testing.T) {
	a, _ := NewToken()
	b, _ := NewToken()
	if a == b || len(a) != 43 {
		t.Fatalf("tokens %d chars, equal=%v", len(a), a == b)
	}
	path := filepath.Join(t.TempDir(), "auth", "token.sha256")
	if err := WriteTokenHash(path, a); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), a) || strings.TrimSpace(string(raw)) != HashToken(a) {
		t.Fatal("the token file must hold the sha256 only")
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
	ta, err := LoadTokenAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+a)
	if p, err := ta.Authenticate(r); err != nil || p.Method != "bearer" {
		t.Fatalf("own token refused: %v", err)
	}
	r.Header.Set("Authorization", "Bearer "+b)
	if _, err := ta.Authenticate(r); err == nil {
		t.Fatal("another token accepted")
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o644)
		if _, err := LoadTokenAuth(path); err == nil {
			t.Fatal("a world-readable token hash was loaded")
		}
	}
}

func TestBearerTicketSessionFlow(t *testing.T) {
	tok, _ := NewToken()
	a := NewTokenAuth(tok)
	srv := server(t, a)
	bearer := map[string]string{"Authorization": "Bearer " + tok}

	if res := do(t, "GET", srv.URL+prefix+"/host/hello", nil); res.StatusCode != 200 {
		t.Fatalf("hello is public: %d", res.StatusCode)
	}
	for _, path := range []string{prefix + "/projects", "/", prefix + "/auth/ticket"} {
		res := do(t, "GET", srv.URL+path, nil)
		if res.StatusCode != 401 {
			t.Fatalf("%s without auth: %d", path, res.StatusCode)
		}
	}
	res := do(t, "GET", srv.URL+prefix+"/projects", map[string]string{"Authorization": "Bearer wrong"})
	var e map[string]string
	_ = json.NewDecoder(res.Body).Decode(&e)
	if res.StatusCode != 401 || e["error"] == "" {
		t.Fatalf("wrong token: %d %v", res.StatusCode, e)
	}
	if res := do(t, "GET", srv.URL+prefix+"/projects", bearer); res.StatusCode != 200 {
		t.Fatalf("bearer: %d", res.StatusCode)
	}

	res = do(t, "POST", srv.URL+prefix+"/auth/ticket", bearer)
	var tk map[string]string
	_ = json.NewDecoder(res.Body).Decode(&tk)
	if res.StatusCode != 200 || len(tk["ticket"]) < 32 {
		t.Fatalf("ticket: %d %v", res.StatusCode, tk)
	}
	res = do(t, "GET", srv.URL+SessionPath+"?ticket="+tk["ticket"], nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("session: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	var c *http.Cookie
	for _, x := range res.Cookies() {
		if x.Name == SessionCookie {
			c = x
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("cookie %+v", c)
	}
	cookie := map[string]string{"Cookie": SessionCookie + "=" + c.Value}
	if res := do(t, "GET", srv.URL+"/", cookie); res.StatusCode != 200 {
		t.Fatalf("studio by cookie: %d", res.StatusCode)
	}
	if res := do(t, "GET", srv.URL+prefix+"/projects", cookie); res.StatusCode != 200 {
		t.Fatalf("api by cookie: %d", res.StatusCode)
	}
	if res := do(t, "POST", srv.URL+prefix+"/auth/ticket", cookie); res.StatusCode != 401 {
		t.Fatalf("a session must not mint tickets: %d", res.StatusCode)
	}
	if res := do(t, "GET", srv.URL+SessionPath+"?ticket="+tk["ticket"], nil); res.StatusCode != 401 {
		t.Fatalf("ticket reused: %d", res.StatusCode)
	}
	if res := do(t, "GET", srv.URL+"/", map[string]string{"Cookie": SessionCookie + "=forged"}); res.StatusCode != 401 {
		t.Fatalf("forged cookie: %d", res.StatusCode)
	}
}

func TestTicketAndSessionExpire(t *testing.T) {
	tok, _ := NewToken()
	a := NewTokenAuth(tok)
	now := time.Unix(1_800_000_000, 0)
	a.now = func() time.Time { return now }
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	tk, exp, err := a.IssueTicket(r)
	if err != nil || !exp.Equal(now.Add(TicketTTL)) {
		t.Fatal(err)
	}
	now = now.Add(TicketTTL + time.Second)
	if _, ok := a.redeem(tk); ok {
		t.Fatal("expired ticket redeemed")
	}
	tk, _, _ = a.IssueTicket(r)
	s, ok := a.redeem(tk)
	if !ok {
		t.Fatal("fresh ticket refused")
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: s})
	now = now.Add(SessionTTL - time.Minute)
	if _, err := a.Authenticate(req); err != nil {
		t.Fatal("live session refused")
	}
	now = now.Add(SessionTTL - time.Minute) // still inside: the last use extended it
	if _, err := a.Authenticate(req); err != nil {
		t.Fatal("sliding session refused")
	}
	now = now.Add(SessionTTL + time.Second)
	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("idle session accepted")
	}
}

func TestOnlyLoopbackHostNames(t *testing.T) {
	tok, _ := NewToken()
	srv := server(t, NewTokenAuth(tok))
	bearer := "Bearer " + tok
	for host, want := range map[string]int{
		"127.0.0.1:18120": 200, "localhost:41234": 200, "[::1]:9": 200,
		"evil.example:18120": http.StatusMisdirectedRequest, "127.0.0.1.nip.io": http.StatusMisdirectedRequest,
		"abc.preview.localhost:18121": http.StatusMisdirectedRequest,
	} {
		res := do(t, "GET", srv.URL+prefix+"/projects", map[string]string{"Host": host, "Authorization": bearer})
		if res.StatusCode != want {
			t.Errorf("Host %s: %d, want %d", host, res.StatusCode, want)
		}
	}
}

func TestSSEFlushesThroughTheMiddleware(t *testing.T) {
	tok, _ := NewToken()
	srv := server(t, NewTokenAuth(tok))
	req, _ := http.NewRequest("GET", srv.URL+prefix+"/sse", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("sse %d", res.StatusCode)
	}
	got := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(res.Body).ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if line != "data: one\n" {
			t.Fatalf("got %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event was buffered: no flush through the middleware")
	}
}
