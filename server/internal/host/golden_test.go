package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The golden file pins what the Studio sees from the agent routes: capabilities, sessions, prompt outcomes, the
// events published on the session stream, transcript, export, credentials and the error answers. It is the guard of the
// coding-agent adapter extraction (ADR 0002): the runtime behind the routes may change, these answers may not.
// Refresh it only on purpose: UPDATE_GOLDEN=1 go test ./server/internal/host -run TestAgentRoutesGolden

var (
	goldenID   = regexp.MustCompile(`[0-9a-f]{32}`)
	goldenTime = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z`)
	goldenSum  = regexp.MustCompile(`sha256:[0-9a-f]{64}`)
)

type goldenRec struct {
	ids   map[string]string
	steps []map[string]any
}

func (g *goldenRec) norm(raw string) string {
	raw = goldenSum.ReplaceAllString(raw, "sha256:<sum>")
	raw = goldenTime.ReplaceAllString(raw, "<time>")
	return goldenID.ReplaceAllStringFunc(raw, func(id string) string {
		if n, ok := g.ids[id]; ok {
			return n
		}
		n := fmt.Sprintf("<id%d>", len(g.ids)+1)
		g.ids[id] = n
		return n
	})
}

func (g *goldenRec) add(name string, code int, body any) {
	raw, _ := json.Marshal(body)
	var v any
	_ = json.Unmarshal([]byte(g.norm(string(raw))), &v)
	g.steps = append(g.steps, map[string]any{"step": name, "status": code, "body": v})
}

func goldenCall(t *testing.T, e testEnv, method, path string, body any) (int, any) {
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
	e.host.ServeHTTP(rec, req)
	var out any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestAgentRoutesGolden(t *testing.T) {
	e := newEnv(t, pinnedSeam(t), true)
	g := &goldenRec{ids: map[string]string{}}
	step := func(name, method, path string, body any) any {
		code, out := goldenCall(t, e, method, path, body)
		g.add(name, code, out)
		return out
	}
	get := func(v any, keys ...string) string {
		for _, k := range keys {
			v = v.(map[string]any)[k]
		}
		return v.(string)
	}
	step("host capabilities", "GET", "/host/capabilities", nil)
	step("credentials, none yet", "GET", "/me/credentials", nil)
	e.agent.key = false
	step("me/cursor without a key", "GET", "/me/cursor", nil)
	e.agent.key = true
	e.agent.keys.Store("grok_golden_key_0123456789")
	step("credentials, one key", "GET", "/me/credentials", nil)
	step("me/cursor", "GET", "/me/cursor", nil)
	step("put credential, wrong slot", "PUT", "/me/credentials/pi-sdk/openai", map[string]any{"key": "x"})
	step("put credential, empty", "PUT", "/me/credentials/cursor-sdk/cursor", map[string]any{"key": " "})
	step("put credential, foreign key", "PUT", "/me/credentials/cursor-sdk/cursor", map[string]any{"key": "xai-golden0123456789"})
	step("put credential, rejected", "PUT", "/me/credentials/cursor-sdk/cursor", map[string]any{"key": "grok_bad_golden_2a61"})
	step("put credential, good", "PUT", "/me/credentials/cursor-sdk/cursor", map[string]any{"key": "grok_good_golden_2a61"})

	p := step("create project", "POST", "/projects", map[string]any{"name": "golden", "templateId": "mini"})
	pid := get(p, "project", "id")
	base := "/projects/" + pid
	step("agent capabilities", "GET", base+"/agent/capabilities", nil)
	s := step("create session", "POST", base+"/agent/sessions", nil)
	sid := get(s, "session", "id")
	sbase := base + "/agent/sessions/" + sid
	step("list sessions", "GET", base+"/agent/sessions", nil)

	ch, unsub := e.host.bus.Subscribe(sid)
	prompt := func(name string, body map[string]any) {
		if _, ok := body["modelId"]; !ok {
			body["modelId"] = "cursor/grok-4.7"
		}
		step(name, "POST", sbase+"/messages", body)
	}
	prompt("prompt, ok", map[string]any{"text": "alpha", "modelParams": []map[string]string{{"id": "reasoning_effort", "value": "xhigh"}}, "clientMsgId": "m-1"})
	prompt("prompt, same client id again", map[string]any{"text": "alpha", "clientMsgId": "m-1"})
	prompt("prompt, empty text", map[string]any{"text": "  "})
	prompt("prompt, foreign model", map[string]any{"text": "x", "modelId": "openai/gpt"})
	prompt("prompt, model unknown to the key", map[string]any{"text": "x", "modelId": "cursor/gone"})
	prompt("prompt, bad image", map[string]any{"text": "x", "images": []map[string]string{{"mediaType": "image/bmp", "data": "AA=="}}})
	prompt("prompt, clientMsgId too long", map[string]any{"text": "x", "clientMsgId": strings.Repeat("c", 129)})
	prompt("prompt, agent fails", map[string]any{"text": "fail"})
	unsub()
	var events []any
drain:
	for {
		select {
		case ev := <-ch:
			var v any
			raw, _ := json.Marshal(ev.Wire())
			_ = json.Unmarshal([]byte(g.norm(string(raw))), &v)
			events = append(events, v)
		default:
			break drain
		}
	}
	g.steps = append(g.steps, map[string]any{"step": "events published on the session stream", "body": events})

	tx := step("transcript", "GET", sbase+"/messages", nil)
	var runID string
	for _, m := range tx.(map[string]any)["messages"].([]any) {
		if m.(map[string]any)["role"] == "assistant" {
			runID = m.(map[string]any)["runId"].(string)
			break
		}
	}
	step("feedback", "POST", base+"/agent/runs/"+runID+"/feedback", map[string]any{"rating": "up"})
	step("feedback, bad rating", "POST", base+"/agent/runs/"+runID+"/feedback", map[string]any{"rating": "meh"})
	step("transcript with feedback", "GET", sbase+"/messages", nil)
	step("export", "GET", sbase+"/export", nil)
	step("abort", "POST", sbase+"/abort", nil)

	// fail closed
	e.agent.sandbox = errors.New("sandbox preflight failed")
	step("capabilities, sandbox off", "GET", "/host/capabilities", nil)
	prompt("prompt, sandbox off", map[string]any{"text": "x"})
	e.agent.sandbox = nil
	e.agent.key = false
	prompt("prompt, no key", map[string]any{"text": "x"})
	e.agent.key = true
	e.agent.unsandboxed = true
	step("capabilities, unsandboxed", "GET", "/host/capabilities", nil)
	e.agent.unsandboxed = false
	sess, _ := e.host.opts.Store.Session(pid, sid)
	_ = os.MkdirAll(filepath.Join(sess.Worktree, ".cursor"), 0o755)
	_ = os.WriteFile(filepath.Join(sess.Worktree, ".cursor", "hooks.json"), []byte("{}"), 0o644)
	prompt("prompt, project hooks.json", map[string]any{"text": "x"})
	_ = os.RemoveAll(filepath.Join(sess.Worktree, ".cursor"))
	step("delete session", "DELETE", sbase, nil)
	step("session gone", "GET", sbase+"/messages", nil)
	step("delete credential, wrong slot", "DELETE", "/me/credentials/pi-sdk/openai", nil)
	step("delete credential", "DELETE", "/me/credentials/cursor-sdk/cursor", nil)
	step("credentials, none again", "GET", "/me/credentials", nil)

	got, _ := json.MarshalIndent(g.steps, "", "  ")
	got = append(got, '\n')
	file := filepath.Join("testdata", "golden", "agent-routes.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(file, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (UPDATE_GOLDEN=1 writes it)", err)
	}
	if string(want) != string(got) {
		_ = os.WriteFile(file+".got", got, 0o644)
		t.Fatalf("agent routes differ from %s (the answer is in %s.got)", file, file)
	}
}

var _ = http.StatusOK
