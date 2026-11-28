package host

import (
	"context"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
)

// fakePi is a second, non-Cursor runtime: it proves the host talks to adapters, not to Cursor (ADR 0002).
type fakePi struct {
	*fakeAgent
}

func newFakePi() *fakePi { return &fakePi{&fakeAgent{key: true, keys: &memKeys{}}} }

func (f *fakePi) Runtime() string { return codingagent.RuntimePi }
func (f *fakePi) Capabilities() codingagent.Capabilities {
	return codingagent.Capabilities{Runtime: codingagent.RuntimePi, Label: "Pi", ModelSource: codingagent.ModelSourceOwnKeys, Streaming: true, Tools: true}
}
func (f *fakePi) DefaultModel() codingagent.Model {
	return codingagent.Model{ID: "pi/gigachat", Provider: "pi", Label: "GigaChat"}
}
func (f *fakePi) Models(context.Context) ([]codingagent.Model, error) {
	return []codingagent.Model{f.DefaultModel()}, nil
}
func (f *fakePi) CheckModel(id string) string {
	if !strings.HasPrefix(id, "pi/") {
		return "Model " + id + " is not a Pi model"
	}
	return ""
}
func (f *fakePi) CheckWorkspace(string) error { return nil }
func (f *fakePi) Credential() *codingagent.CredentialSpec {
	return &codingagent.CredentialSpec{
		Provider: "gigachat", Label: "Pi", Store: f.keys, Missing: "Paste a GigaChat key",
		Validate:     func(string) string { return "" },
		NoCredential: codingagent.ErrNoCredential,
		AuthHint:     func(string, string) string { return "check the key" },
		Describe:     func(string, string) string { return "pi key" },
		Mask:         func(string) string { return "****" },
	}
}

func TestRuntimeIsChosenByTheContourAndEachChatKeepsItsOwn(t *testing.T) {
	pi := newFakePi()
	e := newEnvAgents(t, pinnedSeam(t), true, codingagent.RuntimePi, []codingagent.Adapter{pi})

	_, caps := goldenCall(t, e, "GET", "/host/capabilities", nil)
	if caps.(map[string]any)["agent_sandbox"] != true {
		t.Fatalf("capabilities %v", caps)
	}
	_, p := goldenCall(t, e, "POST", "/projects", map[string]any{"name": "x", "templateId": "mini"})
	pid := p.(map[string]any)["project"].(map[string]any)["id"].(string)
	base := "/projects/" + pid + "/agent"
	_, ac := goldenCall(t, e, "GET", base+"/capabilities", nil)
	m := ac.(map[string]any)
	if m["runtime"] != "pi" || m["runtimeLabel"] != "Pi" {
		t.Fatalf("the contour default must answer: %v", m)
	}
	_, s := goldenCall(t, e, "POST", base+"/sessions", nil)
	piSession := s.(map[string]any)["session"].(map[string]any)
	if piSession["runtime"] != "pi" {
		t.Fatalf("new chat runtime %v", piSession["runtime"])
	}
	// A chat made before runtimes existed has no runtime: it is a Cursor chat, whatever the contour default is now.
	old, err := e.host.opts.Store.Session(pid, piSession["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	old.ID, old.Runtime, old.Worktree = "00000000000000000000000000000001", "", ""
	if err := e.host.opts.Store.PutSession(old); err != nil {
		t.Fatal(err)
	}
	_, ls := goldenCall(t, e, "GET", base+"/sessions", nil)
	seen := map[string]string{}
	for _, x := range ls.(map[string]any)["sessions"].([]any) {
		seen[x.(map[string]any)["id"].(string)] = x.(map[string]any)["runtime"].(string)
	}
	if seen[old.ID] != "cursor-sdk" || seen[piSession["id"].(string)] != "pi" {
		t.Fatalf("runtimes %v", seen)
	}

	send := func(sid, model string) (int, any) {
		return goldenCall(t, e, "POST", base+"/sessions/"+sid+"/messages", map[string]any{"text": "hello", "modelId": model})
	}
	if code, out := send(piSession["id"].(string), "cursor/auto"); code != 400 || !strings.Contains(out.(map[string]any)["error"].(string), "not a Pi model") {
		t.Fatalf("a Pi chat must be checked by Pi: %d %v", code, out)
	}
	if code, out := send(piSession["id"].(string), "pi/gigachat"); code != 200 || out.(map[string]any)["status"] != "idle" {
		t.Fatalf("pi run: %d %v", code, out)
	}
	if len(pi.cwds) != 1 || len(e.agent.cwds) != 0 {
		t.Fatalf("the run must go to the chat's runtime: pi %d cursor %d", len(pi.cwds), len(e.agent.cwds))
	}
	if code, out := send(old.ID, "cursor/auto"); code != 200 || out.(map[string]any)["status"] != "idle" {
		t.Fatalf("cursor run: %d %v", code, out)
	}
	if len(pi.cwds) != 1 || len(e.agent.cwds) != 1 {
		t.Fatalf("pi %d cursor %d", len(pi.cwds), len(e.agent.cwds))
	}

	// Both runtimes keep their own credential slot.
	if code, _ := goldenCall(t, e, "PUT", "/me/credentials/pi/gigachat", map[string]any{"key": "gc-secret-key"}); code != 200 {
		t.Fatal("pi credential not saved")
	}
	if code, _ := goldenCall(t, e, "PUT", "/me/credentials/cursor-sdk/cursor", map[string]any{"key": "grok_good_golden_2a61"}); code != 200 {
		t.Fatal("cursor credential not saved")
	}
	_, cl := goldenCall(t, e, "GET", "/me/credentials", nil)
	var slots []string
	for _, c := range cl.(map[string]any)["credentials"].([]any) {
		slots = append(slots, c.(map[string]any)["runtime"].(string)+"/"+c.(map[string]any)["providerId"].(string))
	}
	if strings.Join(slots, ",") != "cursor-sdk/cursor,pi/gigachat" {
		t.Fatalf("slots %v", slots)
	}
	if _, out := goldenCall(t, e, "PUT", "/me/credentials/pi/other", map[string]any{"key": "x"}); !strings.Contains(out.(map[string]any)["error"].(string), "Only the Cursor, Pi key") {
		t.Fatalf("wrong slot: %v", out)
	}
}

func TestContourWithoutCursorFailsClosedForCursorChats(t *testing.T) {
	e := newEnvAgents(t, pinnedSeam(t), true, codingagent.RuntimePi, []codingagent.Adapter{newFakePi()}, true)
	_, p := goldenCall(t, e, "POST", "/projects", map[string]any{"name": "x", "templateId": "mini"})
	pid := p.(map[string]any)["project"].(map[string]any)["id"].(string)
	_, s := goldenCall(t, e, "POST", "/projects/"+pid+"/agent/sessions", nil)
	old, _ := e.host.opts.Store.Session(pid, s.(map[string]any)["session"].(map[string]any)["id"].(string))
	old.Runtime = ""
	_ = e.host.opts.Store.PutSession(old)
	code, out := goldenCall(t, e, "POST", "/projects/"+pid+"/agent/sessions/"+old.ID+"/messages", map[string]any{"text": "hello", "modelId": "cursor/auto"})
	if code != 503 || !strings.Contains(out.(map[string]any)["error"].(string), "not part of this build") {
		t.Fatalf("a Cursor chat on a server without Cursor must stop, not fall back to another provider: %d %v", code, out)
	}
	if code, _ := goldenCall(t, e, "GET", "/me/cursor", nil); code != 503 {
		t.Fatalf("/me/cursor without Cursor: %d", code)
	}
}
