package host

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/bridge"
)

func call(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = strings.NewReader(string(raw))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, "/runtime/v1"+path, rd)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func mustProject(t *testing.T, e testEnv, name string) map[string]any {
	t.Helper()
	code, out := call(t, e.host, "POST", "/projects", map[string]any{"name": name, "templateId": "mini"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	return out["project"].(map[string]any)
}

func TestProjectsAndFiles(t *testing.T) {
	e := newEnv(t, pinnedSeam(t), true)
	p := mustProject(t, e, "Мой лендинг")
	id := p["id"].(string)
	if _, out := call(t, e.host, "GET", "/projects", nil); len(out["projects"].([]any)) != 1 {
		t.Fatal(out)
	}
	if code, out := call(t, e.host, "GET", "/projects/"+id+"/files", nil); code != 200 || !strings.Contains(mustJSON(out), "index.html") || strings.Contains(mustJSON(out), `"name":".git"`) {
		t.Fatalf("tree %d %v", code, out)
	}
	if code, _ := call(t, e.host, "PUT", "/projects/"+id+"/file", map[string]any{"path": "src/a.txt", "content": "A"}); code != 200 {
		t.Fatal(code)
	}
	if _, out := call(t, e.host, "GET", "/projects/"+id+"/file?path=src/a.txt", nil); out["content"] != "A" {
		t.Fatal(out)
	}
	for _, bad := range []string{"../x", "/etc/passwd", ".git/config"} {
		if code, _ := call(t, e.host, "GET", "/projects/"+id+"/file?path="+bad, nil); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, _ := call(t, e.host, "PUT", "/projects/"+id+"/brief", map[string]any{"format": "buildy.brief.v1"}); code != 200 {
		t.Fatal(code)
	}
	if _, out := call(t, e.host, "GET", "/projects/"+id+"/brief", nil); out["format"] != "buildy.brief.v1" {
		t.Fatal(out)
	}
	if code, _ := call(t, e.host, "POST", "/projects/"+id+"/delete", map[string]any{"confirmName": "nope"}); code != 400 {
		t.Fatal("delete without confirm")
	}
	if code, _ := call(t, e.host, "POST", "/projects/"+id+"/delete", map[string]any{"confirmName": "Мой лендинг"}); code != 200 {
		t.Fatal("delete")
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dir, "projects")); len(entries) != 0 {
		t.Fatal("folder left")
	}
}

func mustJSON(v any) string { raw, _ := json.Marshal(v); return string(raw) }

func TestCredentialsNeverEchoTheKey(t *testing.T) {
	e := newEnv(t, pinnedSeam(t), false)
	code, out := call(t, e.host, "PUT", "/me/credentials/cursor-sdk/cursor", map[string]any{"key": "cursor_supersecret_9876"})
	if code != 200 || strings.Contains(mustJSON(out), "supersecret") || !strings.Contains(mustJSON(out), "9876") {
		t.Fatalf("%d %v", code, out)
	}
	if _, out := call(t, e.host, "GET", "/me/credentials", nil); strings.Contains(mustJSON(out), "supersecret") {
		t.Fatal(out)
	}
	if code, _ := call(t, e.host, "PUT", "/me/credentials/pi-sdk/openai", map[string]any{"key": "x"}); code != 404 {
		t.Fatal("other slots accepted")
	}
}

func prompt(t *testing.T, e testEnv, pid, sid, text string) (int, map[string]any) {
	return call(t, e.host, "POST", "/projects/"+pid+"/agent/sessions/"+sid+"/messages", map[string]any{"text": text, "modelId": "cursor/grok-4.7",
		"modelParams": []map[string]string{{"id": "reasoning_effort", "value": "xhigh"}}})
}

func TestSessionsWorkInWorktreesAndLandOnMain(t *testing.T) {
	e := newEnv(t, pinnedSeam(t), true)
	p := mustProject(t, e, "wt")
	pid := p["id"].(string)
	_, a := call(t, e.host, "POST", "/projects/"+pid+"/agent/sessions", nil)
	_, b := call(t, e.host, "POST", "/projects/"+pid+"/agent/sessions", nil)
	sa := a["session"].(map[string]any)["id"].(string)
	sb := b["session"].(map[string]any)["id"].(string)
	code, out := prompt(t, e, pid, sa, "alpha")
	if code != 200 || out["status"] != "idle" || out["filesChanged"].(float64) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	if len(e.agent.cwds) != 1 || !strings.Contains(e.agent.cwds[0], filepath.Join("worktrees", pid, sa)) {
		t.Fatalf("agent cwd %v", e.agent.cwds)
	}
	proj, _ := e.host.opts.Store.Project(pid)
	if _, err := os.Stat(filepath.Join(proj.Path, "alpha.txt")); err != nil {
		t.Fatal("run not merged into the project folder")
	}
	sessB, _ := e.host.opts.Store.Session(pid, sb)
	if _, err := os.Stat(filepath.Join(sessB.Worktree, "alpha.txt")); err == nil {
		t.Fatal("session B saw A's file before its own run")
	}
	if code, out := prompt(t, e, pid, sb, "beta"); code != 200 || out["status"] != "idle" {
		t.Fatalf("%d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(sessB.Worktree, "alpha.txt")); err != nil {
		t.Fatal("session B did not take in main before its run")
	}
	_, tx := call(t, e.host, "GET", "/projects/"+pid+"/agent/sessions/"+sa+"/messages", nil)
	msgs := tx["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "user" || msgs[1].(map[string]any)["content"] != "wrote alpha" {
		t.Fatalf("%v", msgs)
	}
	runID := msgs[1].(map[string]any)["runId"].(string)
	if code, _ := call(t, e.host, "POST", "/projects/"+pid+"/agent/runs/"+runID+"/feedback", map[string]any{"rating": "up"}); code != 200 {
		t.Fatal("feedback")
	}
	if code, _ := call(t, e.host, "DELETE", "/projects/"+pid+"/agent/sessions/"+sa, nil); code != 204 {
		t.Fatal("delete session")
	}
	sessA := filepath.Join(e.dir, "worktrees", pid, sa)
	if _, err := os.Stat(sessA); err == nil {
		t.Fatal("worktree left after delete")
	}
}

// A message sent twice with the same clientMsgId (the owner's Windows double send) is stored and run once.
func TestSameClientMessageRunsOnce(t *testing.T) {
	e := newEnv(t, pinnedSeam(t), true)
	pid := mustProject(t, e, "once")["id"].(string)
	_, a := call(t, e.host, "POST", "/projects/"+pid+"/agent/sessions", nil)
	sid := a["session"].(map[string]any)["id"].(string)
	send := func() (int, map[string]any) {
		return call(t, e.host, "POST", "/projects/"+pid+"/agent/sessions/"+sid+"/messages", map[string]any{"text": "alpha", "modelId": "cursor/grok-4.7",
			"modelParams": []map[string]string{{"id": "reasoning_effort", "value": "xhigh"}}, "clientMsgId": "u-1"})
	}
	if code, out := send(); code != 200 || out["status"] != "idle" {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := send(); code != 409 {
		t.Fatalf("repeat answered %d", code)
	}
	if len(e.agent.cwds) != 1 {
		t.Fatalf("agent ran %d times", len(e.agent.cwds))
	}
	_, tx := call(t, e.host, "GET", "/projects/"+pid+"/agent/sessions/"+sid+"/messages", nil)
	if msgs := tx["messages"].([]any); len(msgs) != 2 {
		t.Fatalf("%v", msgs)
	}
}

// Windows: the agent runs without the SDK sandbox, and the capabilities say so for the Studio's warning.
func TestCapabilitiesReportAnUnsandboxedAgent(t *testing.T) {
	e := newEnv(t, pinnedSeam(t), false)
	if _, caps := call(t, e.host, "GET", "/host/capabilities", nil); caps["agent_unsandboxed"] != false || caps["agent_sandbox"] != true {
		t.Fatalf("sandboxed host: %v", caps)
	}
	e.agent.unsandboxed = true
	if _, caps := call(t, e.host, "GET", "/host/capabilities", nil); caps["agent_unsandboxed"] != true {
		t.Fatalf("unsandboxed host: %v", caps)
	}
}

func TestPromptFailsClosed(t *testing.T) {
	e := newEnv(t, pinnedSeam(t), false)
	pid := mustProject(t, e, "closed")["id"].(string)
	_, s := call(t, e.host, "POST", "/projects/"+pid+"/agent/sessions", nil)
	sid := s["session"].(map[string]any)["id"].(string)
	e.agent.sandbox = errors.New("sandbox preflight failed")
	if code, out := prompt(t, e, pid, sid, "x"); code != 503 || !strings.Contains(out["error"].(string), "sandbox") {
		t.Fatalf("%d %v", code, out)
	}
	if _, caps := call(t, e.host, "GET", "/host/capabilities", nil); caps["agent_sandbox"] != false {
		t.Fatal("capability not off")
	}
	e.agent.sandbox = nil
	e.agent.key = false
	if code, _ := prompt(t, e, pid, sid, "x"); code != 503 {
		t.Fatal("ran without a key")
	}
	e.agent.key = true
	proj, _ := e.host.opts.Store.Project(pid)
	_ = os.MkdirAll(filepath.Join(proj.Path, ".cursor"), 0o755)
	_ = os.WriteFile(filepath.Join(proj.Path, ".cursor", "hooks.json"), []byte("{}"), 0o644)
	if code, _ := prompt(t, e, pid, sid, "x"); code != 409 {
		t.Fatal("ran with a project hooks.json")
	}
	_ = os.RemoveAll(filepath.Join(proj.Path, ".cursor"))
	if code, _ := call(t, e.host, "POST", "/projects/"+pid+"/agent/sessions/"+sid+"/messages", map[string]any{"text": "x", "modelId": "openai/gpt"}); code != 400 {
		t.Fatal("foreign model accepted")
	}
	if code, out := prompt(t, e, pid, sid, "fail"); code != 200 || out["status"] != "error" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestEventsStream(t *testing.T) {
	e := newEnv(t, pinnedSeam(t), false)
	pid := mustProject(t, e, "sse")["id"].(string)
	_, s := call(t, e.host, "POST", "/projects/"+pid+"/agent/sessions", nil)
	sid := s["session"].(map[string]any)["id"].(string)
	srv := httptest.NewServer(e.host)
	defer srv.Close()
	res, err := http.Get(srv.URL + "/runtime/v1/projects/" + pid + "/agent/sessions/" + sid + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if !strings.HasPrefix(res.Header.Get("content-type"), "text/event-stream") {
		t.Fatal(res.Header)
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				lines <- strings.TrimPrefix(sc.Text(), "data: ")
			}
		}
	}()
	<-lines // initial status
	go prompt(t, e, pid, sid, "gamma")
	want := []string{"session.status", "message.delta", "files.changed", "session.status"}
	for _, w := range want {
		select {
		case l := <-lines:
			var ev map[string]any
			_ = json.Unmarshal([]byte(l), &ev)
			if ev["type"] != w {
				t.Fatalf("got %v, want %s", ev["type"], w)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no %s event", w)
		}
	}
}

// Saving the key: format checks, the xai- hint, and the online check through Me (warning, never a lost key).
func TestCredentialSaveChecksTheKey(t *testing.T) {
	e := newEnv(t, pinnedSeam(t), false)
	put := func(k string) (int, string) {
		code, out := call(t, e.host, "PUT", "/me/credentials/cursor-sdk/cursor", map[string]any{"key": k})
		return code, mustJSON(out)
	}
	if code, out := put("xai-supersecret1234567890"); code != 400 || !strings.Contains(out, "grok.com/dashboard/api-keys") || strings.Contains(out, "supersecret") {
		t.Fatalf("xai key: %d %s", code, out)
	}
	if code, out := put("grok_super secret_1234567890"); code != 400 || !strings.Contains(out, "spaces") {
		t.Fatalf("inner space: %d %s", code, out)
	}
	if code, out := put("grok_bad_supersecret_2a61"); code != 200 || !strings.Contains(out, "Invalid Service Token") ||
		!strings.Contains(out, "grok.com/dashboard/api-keys") || !strings.Contains(out, `prefix \"grok_\"`) || strings.Contains(out, "supersecret") {
		t.Fatalf("rejected key: %d %s", code, out)
	} else if k, _ := e.agent.keys.Key(); k != "grok_bad_supersecret_2a61" {
		t.Fatal("a rejected key must still be kept")
	}
	if code, out := put("grok_offline_supersecret"); code != 200 || !strings.Contains(out, "could not check") {
		t.Fatalf("offline: %d %s", code, out)
	}
	if code, out := put("  grok_good_supersecret_2a61\n"); code != 200 || !strings.Contains(out, "a@b.c") || strings.Contains(out, "warning") {
		t.Fatalf("good key: %d %s", code, out)
	} else if k, _ := e.agent.keys.Key(); k != "grok_good_supersecret_2a61" {
		t.Fatalf("not trimmed: %q", k)
	}
}

// "Could not start the agent: bridge: unauthenticated: …" carries the key diagnostic and where to get a key.
func TestAgentErrorExplainsARejectedKey(t *testing.T) {
	e := newEnv(t, pinnedSeam(t), false)
	_ = e.agent.keys.Store("grok_supersecret_middle_2a61")
	msg := e.host.agentError(e.agent, "Could not start the agent: ", &bridge.Error{Code: "unauthenticated", Message: "Invalid Service Token"})
	for _, want := range []string{"Invalid Service Token", "28 chars", `prefix "grok_"`, "fingerprint sha256:", "from keychain", "grok.com/dashboard/api-keys"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "supersecret") || strings.Contains(msg, "2a61") {
		t.Fatal("key leaked")
	}
	if got := e.host.agentError(e.agent, "x: ", errors.New("boom")); got != "x: boom" {
		t.Errorf("other errors unchanged: %q", got)
	}
}
