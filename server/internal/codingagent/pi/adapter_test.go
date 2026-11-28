package pi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/providergw/stub"
)

func okReply(int, stub.Request) stub.Reply { return stub.Reply{Text: "pong"} }

func TestFakePiObservedFromInsideTheJail(t *testing.T) {
	needJail(t)
	t.Setenv("VIBECHAT_YANDEX_API_KEY", testKey) // the server's environment holds the real key
	h := newHarness(t, fakeInstall(t), okReply)
	writeFile(t, filepath.Join(h.data, "secret.txt"), "server secret")
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, res, err, handle := h.run(t, ctx, ws, "", "observe "+portOf(h.up.URL))
	if err != nil {
		t.Fatalf("%v\n%s", err, h.logs.all())
	}
	var obs map[string]any
	if err := json.Unmarshal([]byte(res.Text), &obs); err != nil {
		t.Fatalf("%v: %q", err, res.Text)
	}
	flags := obs["flags"].(map[string]any)
	for k, want := range map[string]string{"--mode": "rpc", "--provider": "vibechat", "--model": "yagpt", "--session-id": handle, "--thinking": "off"} {
		if flags[k] != want {
			t.Errorf("flag %s = %v, want %s", k, flags[k], want)
		}
	}
	for _, k := range []string{"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-mcp", "--no-approve", "--offline"} {
		if flags[k] == nil {
			t.Errorf("flag %s missing: project-local code would be loaded", k)
		}
	}
	// no token, no secret of any kind in the jail: the gateway is a socket, the agent's key is a placeholder
	if obs["gatewayTokenEnv"] != "" {
		t.Fatalf("a gateway token is in the agent's environment: %v", obs["gatewayTokenEnv"])
	}
	if ifs, _ := obs["ifaces"].([]any); len(ifs) != 1 || ifs[0] != "lo" {
		t.Fatalf("the jail's network namespace must hold only its own loopback: %v", obs["ifaces"])
	}
	if obs["hostLoopbackReachable"] != false {
		t.Fatal("the jail reaches a port on the host's loopback")
	}
	if leaked, _ := obs["leakedEnv"].([]any); len(leaked) != 0 {
		t.Fatalf("the provider key reached the jail: %v", leaked)
	}
	if obs["piOffline"] != "1" || obs["agentDirEnv"] != flags["--agent-dir"] {
		t.Fatalf("env: %v %v", obs["piOffline"], obs["agentDirEnv"])
	}
	if obs["cwd"] != ws {
		t.Fatalf("cwd %v", obs["cwd"])
	}
	models, _ := obs["modelsJSON"].(string)
	if strings.Contains(models, testKey) || strings.Contains(models, "vcgw_") || !strings.Contains(models, `"vibechat-no-key"`) || !strings.Contains(models, `"http://127.0.0.1:18199/v1"`) {
		t.Fatalf("models.json: %s", models)
	}
	if strings.Contains(models, "gpt://") {
		t.Fatalf("the provider's model name must stay on the host side: %s", models)
	}
	if obs["secretVisible"] != false {
		t.Fatal("the server's data folder is visible inside the jail")
	}
	if obs["wsWrite"] != true || obs["sessWrite"] != true || obs["exeDirWritable"] != false {
		t.Fatalf("mounts: %v %v %v", obs["wsWrite"], obs["sessWrite"], obs["exeDirWritable"])
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "made-by-pi.txt")); string(got) != "hi" {
		t.Fatal("the workspace write did not reach the host")
	}
	// the gateway answered the call from inside the jail, with the real key towards the provider only
	if obs["gatewayStatus"] != float64(200) || !strings.Contains(obs["gatewayBody"].(string), "pong") {
		t.Fatalf("gateway from the jail: %v %v %v", obs["gatewayStatus"], obs["gatewayBody"], obs["gatewayErr"])
	}
	if reqs := h.up.Requests(); len(reqs) != 1 || reqs[0].Authorization != "Api-Key "+testKey || reqs[0].Body["model"] != "gpt://folder/yandexgpt/rc" {
		t.Fatalf("provider saw %+v", reqs)
	}
	// after the run: the socket is gone, the agent folder is gone, the chat's session folder stays
	if socks, _ := filepath.Glob(filepath.Join(os.TempDir(), "vcgw-*", "gw.sock")); len(socks) != 0 {
		t.Fatalf("a run's gateway socket is left: %v", socks)
	}
	if _, err := os.Stat(flags["--agent-dir"].(string)); !os.IsNotExist(err) {
		t.Fatalf("agent folder left: %v", err)
	}
	if _, err := os.Stat(flags["--session-dir"].(string)); err != nil {
		t.Fatalf("session folder: %v", err)
	}
	if strings.Contains(h.logs.all(), testKey) {
		t.Fatalf("log shows a credential:\n%s", h.logs.all())
	}
	_ = c
}

func TestFakePiEventsAndResult(t *testing.T) {
	needJail(t)
	h := newHarness(t, fakeInstall(t), okReply)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, res, err, _ := h.run(t, ctx, t.TempDir(), "", "echo hello there")
	if err != nil {
		t.Fatal(err)
	}
	w := c.wires()
	if len(w) != 2 || w[0]["type"] != "message.delta" || w[0]["text"] != "echo: echo hello there" || w[1]["type"] != "usage" || w[1]["tokensIn"] != int64(3) {
		t.Fatalf("events %v", w)
	}
	if res.Text != "echo: echo hello there" || res.Summary != "echo: echo hello there" || res.Error != "" || res.Usage.TotalTokens != 5 {
		t.Fatalf("result %+v", res)
	}
	// Resume: the same handle comes back; a Cursor agent id is refused
	sess, err := h.ad.Open(ctx, codingagent.OpenRequest{Cwd: t.TempDir(), ModelID: "yandex/yagpt", Resume: "agent-1234abcd"})
	var oe *codingagent.OpenError
	if err == nil || !errors.As(err, &oe) || oe.Phase != codingagent.PhaseSession {
		t.Fatalf("a foreign handle: %v %v", sess, err)
	}
}

func TestFakePiFailures(t *testing.T) {
	needJail(t)
	h := newHarness(t, fakeInstall(t), okReply)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	t.Run("rejected prompt", func(t *testing.T) {
		_, _, err, _ := h.run(t, ctx, t.TempDir(), "", "reject")
		if err == nil || !strings.Contains(err.Error(), "pi refused the prompt: Model not found") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("crash before settling", func(t *testing.T) {
		_, _, err, _ := h.run(t, ctx, t.TempDir(), "", "crash")
		if err == nil || !strings.Contains(err.Error(), "exited before the run finished") || !strings.Contains(err.Error(), "boom: it broke") {
			t.Fatalf("%v", err)
		}
		if strings.Contains(h.logs.all(), "vcgw_") {
			t.Fatalf("token in the log: %s", h.logs.all())
		}
	})
	t.Run("handled prompt starts no run", func(t *testing.T) {
		c, res, err, _ := h.run(t, ctx, t.TempDir(), "", "handled")
		if err != nil || len(c.wires()) != 0 || res.Error != "" {
			t.Fatalf("%v %v %+v", err, c.wires(), res)
		}
	})
	t.Run("a dialog is cancelled, not waited for", func(t *testing.T) {
		_, res, err, _ := h.run(t, ctx, t.TempDir(), "", "ui")
		if err != nil || !strings.Contains(res.Text, `"cancelled":true`) || !strings.Contains(res.Text, `"id":"ui-1"`) || strings.Contains(res.Text, "ui-2") {
			t.Fatalf("%v %q", err, res.Text)
		}
	})
	t.Run("a record over the limit ends the run with an error", func(t *testing.T) {
		_, _, err, _ := h.run(t, ctx, t.TempDir(), "", "huge")
		if err == nil || !strings.Contains(err.Error(), "pi output: pi record over") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a 2.5 MB record and U+2028 inside it", func(t *testing.T) {
		_, res, err, _ := h.run(t, ctx, t.TempDir(), "", "bigline")
		if err != nil || len(res.Text) != 700_000*5 || !strings.Contains(res.Text, "x\u2028y") {
			t.Fatalf("%v %d", err, len(res.Text))
		}
	})
}

func TestFakePiCancel(t *testing.T) {
	needJail(t)
	h := newHarness(t, fakeInstall(t), okReply)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sess, err := h.ad.Open(ctx, codingagent.OpenRequest{Cwd: t.TempDir(), ModelID: "yandex/yagpt"})
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	run := sess.Start(ctx, "s1", "r2", codingagent.Prompt{Text: "hang"}, c.emit)
	deadline := time.Now().Add(20 * time.Second)
	for len(c.wires()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := run.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := run.Wait()
	if err != nil || res.Error != "cancelled" {
		t.Fatalf("%v %+v", err, res)
	}
	if err := run.Cancel(ctx); err != nil {
		t.Fatalf("cancel after the end: %v", err)
	}
}

func TestFakePiHardStopWhenItIgnoresAbort(t *testing.T) {
	needJail(t)
	h := newHarness(t, fakeInstall(t), okReply)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err, _ := h.run(t, ctx, t.TempDir(), "", "deaf")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("the run took %v to stop", d)
	}
}

func TestFailClosed(t *testing.T) {
	good := fakeInstall(t)
	cases := map[string]func(o *Options){
		"no jail":              func(o *Options) { o.Jail = func() error { return errors.New("bwrap not found") } },
		"no nested shell jail": func(o *Options) { o.ToolJail = func() error { return errors.New("user namespaces are limited") } },
		"no Pi":                func(o *Options) { o.Install = Install{}; o.LocateErr = errors.New("the Pi package folder is not set") },
		"no models":            func(o *Options) { o.Models = nil },
		"bad model":            func(o *Options) { o.Models = []ModelEntry{{ID: "Bad Id"}} },
		"no gateway":           func(o *Options) { o.Gateway = nil },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, good, okReply)
			o := h.ad.o
			o.Jail = func() error { return nil }
			o.ToolJail = func() error { return nil }
			mut(&o)
			ad := New(o)
			if ad.Ready() == nil {
				t.Fatal("ready")
			}
			_, err := ad.Open(context.Background(), codingagent.OpenRequest{Cwd: t.TempDir(), ModelID: "yandex/yagpt"})
			if err == nil {
				t.Fatal("opened")
			}
			if name == "no jail" && !strings.Contains(ad.Ready().Error(), "bubblewrap jail") {
				t.Fatalf("%v", ad.Ready())
			}
			if name == "no nested shell jail" && !strings.Contains(ad.Ready().Error(), "nested jail") {
				t.Fatalf("%v", ad.Ready())
			}
		})
	}
	t.Run("no provider key", func(t *testing.T) {
		h := newHarness(t, good, okReply)
		o := h.ad.o
		o.Gateway = providergwNoKey(t)
		o.Jail = func() error { return nil } // the jail is not what this case is about (CI machines have no bwrap)
		o.ToolJail = func() error { return nil }
		ad := New(o)
		if err := ad.Ready(); err == nil || !strings.Contains(err.Error(), "API key is not set") {
			t.Fatalf("%v", err)
		}
	})
}

func TestMenuAndModelChecks(t *testing.T) {
	h := newHarness(t, fakeInstall(t), okReply)
	ad := h.ad
	ms, _ := ad.Models(context.Background())
	if len(ms) != 1 || ms[0].ID != "yandex/yagpt" || ms[0].Label != "YandexGPT" || ms[0].Provider != "yandex" || ad.DefaultModel().ID != "yandex/yagpt" {
		t.Fatalf("%+v", ms)
	}
	if ad.CheckModel("yandex/yagpt") != "" || ad.CheckModel("cursor/auto") == "" || ad.CheckModel("yandex/other") == "" || ad.CheckModel("gpt://folder/yandexgpt/rc") == "" {
		t.Fatal("CheckModel")
	}
	c := ad.Capabilities()
	if c.Runtime != "pi" || c.ModelSource != codingagent.ModelSourceOwnKeys || !c.Streaming || !c.Tools || ad.Unsandboxed() || ad.Credential() != nil || !ad.HasCredential() {
		t.Fatalf("%+v", c)
	}
	if _, err := ad.CheckCredential(context.Background(), "x"); err == nil {
		t.Fatal("a key pasted into Settings must be refused")
	}
	if ad.DropModel("yandex/yagpt", errors.New("403")) != "" {
		t.Fatal("DropModel")
	}
	if got := ad.EgressHosts(); len(got) != 1 || got[0] != "127.0.0.1" {
		t.Fatalf("egress %v", got)
	}
	if _, err := ad.Open(context.Background(), codingagent.OpenRequest{Cwd: t.TempDir(), ModelID: "cursor/auto"}); !errors.Is(err, codingagent.ErrUnknownModel) {
		t.Fatalf("%v", err)
	}
	if err := ad.CheckWorkspace("/nonexistent"); err == nil {
		t.Fatal("CheckWorkspace")
	}
}

func portOf(u string) string { return u[strings.LastIndex(u, ":")+1:] }
