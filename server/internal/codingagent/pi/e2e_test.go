package pi

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/providergw/stub"
)

// The end-to-end tests run the real, pinned Pi inside the real jail against a stub OpenAI-compatible provider through the
// real gateway. They need: bubblewrap, Node >= 22.19 (VIBECHAT_PI_NODE or node on PATH) and the pinned Pi installed by
// fetch-pi (VIBECHAT_PI_DIR). Without them they skip: the always-running tests use a fake Pi process instead.
//
//	go run ./cmd/fetch-pi -out /tmp/pi && VIBECHAT_PI_DIR=/tmp/pi go test ./internal/codingagent/pi -run E2E -v
func realInstall(t *testing.T) Install {
	t.Helper()
	needJail(t)
	dir := os.Getenv("VIBECHAT_PI_DIR")
	if dir == "" {
		t.Skip("VIBECHAT_PI_DIR is not set (the pinned Pi installed by fetch-pi)")
	}
	in, err := Locate(PinnedLock(), os.Getenv("VIBECHAT_PI_NODE"), dir, exec.LookPath)
	if err != nil {
		t.Skipf("no usable Pi/Node: %v", err)
	}
	return in
}

func TestE2EWriteFileThroughTheGateway(t *testing.T) {
	in := realInstall(t)
	h := newHarness(t, in, func(n int, req stub.Request) stub.Reply {
		if n == 0 {
			return stub.Reply{Text: "I will create the file. ", Calls: []stub.Call{{Name: "write", Args: map[string]any{"path": "hello.txt", "content": "hello from pi\n"}}}}
		}
		return stub.Reply{Text: "Created hello.txt."}
	})
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, res, err, handle := h.run(t, ctx, ws, "", "create hello.txt")
	if err != nil {
		t.Fatalf("run: %v\nlog: %s", err, h.logs.all())
	}
	raw, _ := json.MarshalIndent(c.wires(), "", " ")
	t.Logf("events: %s\nresult: %+v", raw, res)
	if got, _ := os.ReadFile(filepath.Join(ws, "hello.txt")); string(got) != "hello from pi\n" {
		t.Fatalf("file: %q", got)
	}
	if res.ToolCalls != 1 || res.Error != "" || !strings.Contains(res.Text, "Created hello.txt.") || len(res.Files) != 1 || res.Files[0] != "hello.txt" || res.Usage == nil {
		t.Fatalf("result: %+v", res)
	}
	if len(handle) != 32 {
		t.Fatalf("handle %q", handle)
	}
	// What the provider saw: only the gateway's credential, the swapped model, tools; never the gateway token.
	reqs := h.up.Requests()
	if len(reqs) != 2 {
		t.Fatalf("provider requests: %d", len(reqs))
	}
	for _, r := range reqs {
		if r.Authorization != "Api-Key "+testKey || r.Body["model"] != "gpt://folder/yandexgpt/rc" || r.Body["tools"] == nil {
			t.Fatalf("provider request: auth %q body %s", r.Authorization, r.Raw)
		}
	}
	if strings.Contains(h.logs.all(), testKey) {
		t.Fatal("the provider key is in the log")
	}
	// The run's agent folder is gone, the chat's session folder stays.
	if _, err := os.Stat(filepath.Join(h.data, "agents", "pi", "run", "r1")); !os.IsNotExist(err) {
		t.Fatalf("run folder left: %v", err)
	}
	if st, err := os.Stat(filepath.Join(h.data, "agents", "pi", "sessions", handle)); err != nil || !st.IsDir() {
		t.Fatalf("session folder: %v", err)
	}
}

// TestE2ECaptureRecords writes the raw records of a real Pi run (a tool call with a shell command, a file write and a
// final answer) to the file named by VIBECHAT_PI_CAPTURE; testdata/real-run.jsonl was made this way and sanitized.
func TestE2ECaptureRecords(t *testing.T) {
	out := os.Getenv("VIBECHAT_PI_CAPTURE")
	if out == "" {
		t.Skip("VIBECHAT_PI_CAPTURE is not set")
	}
	in := realInstall(t)
	var lines []string
	var mu sync.Mutex
	h := newHarness(t, in, func(n int, req stub.Request) stub.Reply {
		switch n {
		case 0:
			return stub.Reply{Text: "Listing first. ", Calls: []stub.Call{{Name: "bash", Args: map[string]any{"command": "echo one; sleep 0.3; echo two"}}}}
		case 1:
			return stub.Reply{Calls: []stub.Call{{Name: "write", Args: map[string]any{"path": "notes/a.txt", "content": "alpha\n"}}}}
		case 2:
			return stub.Reply{Calls: []stub.Call{{Name: "edit", Args: map[string]any{"path": "notes/a.txt", "edits": []any{map[string]any{"oldText": "alpha", "newText": "beta"}}}}}}
		case 3:
			return stub.Reply{Calls: []stub.Call{{Name: "read", Args: map[string]any{"path": "missing.txt"}}}}
		}
		return stub.Reply{Text: "All done."}
	}, func(raw []byte) { mu.Lock(); lines = append(lines, string(raw)); mu.Unlock() })
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, _, err, _ := h.run(t, ctx, ws, "", "do things"); err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(strings.Join(lines, "\n")+"\n", ws, "/W/ws")
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestE2EResumeKeepsTheConversation(t *testing.T) {
	in := realInstall(t)
	h := newHarness(t, in, func(n int, req stub.Request) stub.Reply { return stub.Reply{Text: "ack " + string(rune('A'+n))} })
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, _, err, handle := h.run(t, ctx, ws, "", "remember the word pineapple")
	if err != nil {
		t.Fatal(err)
	}
	_, res, err, handle2 := h.run(t, ctx, ws, handle, "what was the word?")
	if err != nil || handle2 != handle || res.Error != "" {
		t.Fatalf("%v %q %+v", err, handle2, res)
	}
	reqs := h.up.Requests()
	if len(reqs) != 2 || !strings.Contains(string(reqs[1].Raw), "pineapple") || !strings.Contains(string(reqs[1].Raw), "ack A") {
		t.Fatalf("the second prompt did not carry the first turn: %s", reqs[len(reqs)-1].Raw)
	}
}

func TestE2EAgentShellSeesNeitherTheKeyNorTheDataFolder(t *testing.T) {
	in := realInstall(t)
	var data string
	h := newHarness(t, in, func(n int, req stub.Request) stub.Reply {
		if n == 0 {
			return stub.Reply{Calls: []stub.Call{{Name: "bash", Args: map[string]any{"command": "cat " + data + "/secret.txt 2>&1; echo ---; env; echo ---; ls " + data + " 2>&1"}}}}
		}
		return stub.Reply{Text: "checked"}
	})
	data = h.data
	t.Setenv("VIBECHAT_YANDEX_API_KEY", testKey)
	writeFile(t, filepath.Join(data, "secret.txt"), "TOP-SECRET-CONTENT")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, _, err, _ := h.run(t, ctx, t.TempDir(), "", "inspect")
	if err != nil {
		t.Fatal(err)
	}
	var detail string
	for _, w := range c.wires() {
		if w["type"] == "tool.finished" {
			detail, _ = w["detail"].(string)
		}
	}
	if detail == "" {
		t.Fatalf("no shell output: %v", c.wires())
	}
	for _, bad := range []string{"TOP-SECRET-CONTENT", testKey, "VIBECHAT_YANDEX", "secret.txt\n"} {
		if strings.Contains(detail, bad) {
			t.Fatalf("the agent's shell saw %q:\n%s", bad, detail)
		}
	}
	if !strings.Contains(detail, "No such file") {
		t.Fatalf("the data folder should be empty inside the jail:\n%s", detail)
	}
}

func TestE2ECancelStopsALongToolCall(t *testing.T) {
	in := realInstall(t)
	h := newHarness(t, in, func(n int, req stub.Request) stub.Reply {
		return stub.Reply{Calls: []stub.Call{{Name: "bash", Args: map[string]any{"command": "sleep 60"}}}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sess, err := h.ad.Open(ctx, codingagent.OpenRequest{Cwd: t.TempDir(), ModelID: "yandex/yagpt"})
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	run := sess.Start(ctx, "s1", "r9", codingagent.Prompt{Text: "sleep a minute"}, c.emit)
	deadline := time.Now().Add(30 * time.Second)
	started := func() bool {
		for _, w := range c.wires() {
			if w["type"] == "tool.started" {
				return true
			}
		}
		return false
	}
	for !started() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	t0 := time.Now()
	if err := run.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := run.Wait()
	if err != nil || res.Error != "cancelled" || time.Since(t0) > 15*time.Second {
		t.Fatalf("%v %+v after %v", err, res, time.Since(t0))
	}
}

func TestE2EProviderErrorBecomesTheRunError(t *testing.T) {
	in := realInstall(t)
	h := newHarness(t, in, func(n int, req stub.Request) stub.Reply {
		return stub.Reply{Status: 403, Text: "folder has no access to the model"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	_, res, err, _ := h.run(t, ctx, t.TempDir(), "", "hi")
	if err != nil || !strings.Contains(res.Error, "403") {
		t.Fatalf("%v %+v", err, res)
	}
}

// The GigaChat route end to end: real Pi speaks OpenAI tools to the gateway, the gateway speaks functions and OAuth to
// the (stub) GigaChat.
func TestE2EGigaChatThroughTheTranslator(t *testing.T) {
	in := realInstall(t)
	h, up := newGigaHarness(t, in, func(n int, req stub.Request) stub.Reply {
		if n == 0 {
			return stub.Reply{Calls: []stub.Call{{Name: "write", Args: map[string]any{"path": "giga.txt", "content": "from gigachat\n"}}}}
		}
		return stub.Reply{Text: "Saved giga.txt."}
	})
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sess, err := h.ad.Open(ctx, codingagent.OpenRequest{Cwd: ws, ModelID: "gigachat/giga"})
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	res, err := sess.Start(ctx, "s1", "r1", codingagent.Prompt{Text: "write giga.txt"}, c.emit).Wait()
	if err != nil {
		t.Fatalf("%v\n%s", err, h.logs.all())
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "giga.txt")); string(got) != "from gigachat\n" {
		t.Fatalf("file %q result %+v log %s", got, res, h.logs.all())
	}
	if res.ToolCalls != 1 || res.Error != "" || !strings.Contains(res.Text, "Saved giga.txt.") {
		t.Fatalf("%+v", res)
	}
	reqs := up.Requests()
	if len(reqs) != 2 || up.TokenCalls != 1 {
		t.Fatalf("requests %d, OAuth %d", len(reqs), up.TokenCalls)
	}
	for _, r := range reqs {
		if r.Body["tools"] != nil || r.Body["functions"] == nil || r.Body["model"] != "GigaChat-2-Pro" || r.Authorization != "Bearer giga-token-1" {
			t.Fatalf("GigaChat request: %s (auth %s)", r.Raw, r.Authorization)
		}
	}
	// second turn carries the function result back in GigaChat's format
	msgs := reqs[1].Body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	prev := msgs[len(msgs)-2].(map[string]any)
	if last["role"] != "function" || last["name"] != "write" || prev["function_call"] == nil || prev["functions_state_id"] == nil {
		t.Fatalf("second request tail: %v / %v", prev, last)
	}
	if strings.Contains(h.logs.all(), testKey) {
		t.Fatal("key in log")
	}
}

// Yandex AI Studio gives parallel calls of one tool the same id (the function name). Through the gateway the real Pi
// must see two distinct calls, run both, and the provider must get both results back under its own ids.
func TestE2EParallelCallsOfOneToolWithYandexIDs(t *testing.T) {
	in := realInstall(t)
	h := newHarnessWith(t, in, func(n int, req stub.Request) stub.Reply {
		if n == 0 {
			return stub.Reply{Calls: []stub.Call{
				{Name: "write", Args: map[string]any{"path": "a.txt", "content": "A\n"}},
				{Name: "write", Args: map[string]any{"path": "b.txt", "content": "B\n"}}}}
		}
		return stub.Reply{Text: "both written"}
	}, []func(*stub.Server){stub.NameAsID})
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, res, err, _ := h.run(t, ctx, ws, "", "write two files")
	if err != nil || res.Error != "" || res.ToolCalls != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	for f, want := range map[string]string{"a.txt": "A\n", "b.txt": "B\n"} {
		if got, _ := os.ReadFile(filepath.Join(ws, f)); string(got) != want {
			t.Fatalf("%s: %q", f, got)
		}
	}
	ids := map[string]bool{}
	for _, w := range c.wires() {
		if w["type"] == "tool.started" {
			ids[w["callId"].(string)] = true
		}
	}
	if len(ids) != 2 {
		t.Fatalf("the two calls must have distinct ids: %v", ids)
	}
	sent := h.up.Requests()[1].Body["messages"].([]any)
	results := 0
	for _, m := range sent {
		msg := m.(map[string]any)
		if msg["role"] == "tool" {
			results++
			if msg["tool_call_id"] != "write" {
				t.Fatalf("the provider must get its own id back: %v", msg["tool_call_id"])
			}
		}
		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, tc := range calls {
				if tc.(map[string]any)["id"] != "write" {
					t.Fatalf("assistant call id: %v", tc)
				}
			}
		}
	}
	if results != 2 {
		t.Fatalf("tool results sent back: %d", results)
	}
}

// The agent's shell is a nested jail: no network of any kind (not the provider gateway, not the host's loopback, not
// the internet), no gateway socket, no sight of Pi's process or environment, and no token or key anywhere.
func TestE2EAgentShellHasNoNetworkAndNoGateway(t *testing.T) {
	in := realInstall(t)
	var hostPort string
	h := newHarness(t, in, func(n int, req stub.Request) stub.Reply {
		if n > 0 {
			return stub.Reply{Text: "probed"}
		}
		script := `
echo "IFACES=$(tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d ' ' | tr '\n' ,)"
(exec 3<>/dev/tcp/127.0.0.1/` + hostPort + `) 2>&1 | sed 's/^/HOSTLO: /'
(exec 3<>/dev/tcp/127.0.0.1/18199) 2>&1 | sed 's/^/RELAY: /'
(exec 3<>/dev/tcp/1.1.1.1/443) 2>&1 | sed 's/^/INTERNET: /'
echo "SOCKFILES=$(find /tmp /run /var/run -name 'gw.sock' 2>/dev/null | wc -l)"
echo "SOCKDIR=$(ls -A /tmp/vc-gw 2>&1 | wc -l)"
echo "PIDS=$(ls /proc | grep -c '^[0-9][0-9]*$')"
echo "ENVLEAK=$(cat /proc/[0-9]*/environ 2>/dev/null | tr '\0' '\n' | grep -c -e vcgw_ -e '^VIBECHAT_GW' -e '^PI_CODING_AGENT_DIR=' -e ` + testKey + `)"

echo "END"`
		return stub.Reply{Calls: []stub.Call{{Name: "bash", Args: map[string]any{"command": script}}}}
	})
	hostPort = portOf(h.up.URL)
	t.Setenv("VIBECHAT_YANDEX_API_KEY", testKey)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, _, err, _ := h.run(t, ctx, t.TempDir(), "", "probe the sandbox")
	if err != nil {
		t.Fatalf("%v\n%s", err, h.logs.all())
	}
	var out string
	for _, w := range c.wires() {
		if w["type"] == "tool.finished" {
			out, _ = w["detail"].(string)
		}
	}
	if !strings.Contains(out, "END") {
		t.Fatalf("the probe did not finish: %q\n%v", out, c.wires())
	}
	must := func(want string) {
		t.Helper()
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	must("IFACES=lo,")
	must("HOSTLO: ")
	must("RELAY: ")
	must("INTERNET: ")
	for _, bad := range []string{"HOSTLO: \n", "INTERNET: \n"} {
		if strings.Contains(out, bad) {
			t.Errorf("a connection was made (%q):\n%s", bad, out)
		}
	}
	must("SOCKFILES=0")
	must("SOCKDIR=0")
	must("ENVLEAK=0")
	if pids := regexp.MustCompile(`PIDS=(\d+)`).FindStringSubmatch(out); pids == nil || len(pids[1]) > 1 {
		t.Errorf("the shell sees Pi's processes: %v\n%s", pids, out)
	}
	if strings.Contains(out, "vcgw_") || strings.Contains(out, testKey) {
		t.Errorf("a credential in the shell's output:\n%s", out)
	}
}
