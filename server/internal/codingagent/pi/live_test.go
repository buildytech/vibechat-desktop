package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
	"github.com/buildytech/vibechat-desktop/server/internal/providergw"
)

// TestLiveCodingTask is a real Pi through the real gateway to a real OpenAI-compatible provider (OpenRouter for the
// pilot). It needs the pinned Pi (VIBECHAT_PI_DIR) and VIBECHAT_LIVE=1; VIBECHAT_LIVE_BASEURL (default
// https://openrouter.ai/api/v1), VIBECHAT_LIVE_KEYENV (the variable holding the key, default OPENROUTER_API_KEY) and
// VIBECHAT_LIVE_MODEL (default openai/gpt-oss-120b). The key stays in the environment: the gateway reads it per
// request and it is never logged.
//
// One chat, three steps (create a file, edit it, run it in the shell); the test fails if a step did not do its job.
func TestLiveCodingTask(t *testing.T) {
	keyEnv := envOr("VIBECHAT_LIVE_KEYENV", "OPENROUTER_API_KEY")
	if os.Getenv("VIBECHAT_LIVE") != "1" || os.Getenv(keyEnv) == "" {
		t.Skip("the live run is off (VIBECHAT_LIVE=1 and the key variable are needed)")
	}
	in := realInstall(t)
	base := envOr("VIBECHAT_LIVE_BASEURL", "https://openrouter.ai/api/v1")
	model := envOr("VIBECHAT_LIVE_MODEL", "openai/gpt-oss-120b")
	prov := &providergw.OpenAICompat{ID: "live", BaseURL: base, Key: func() string { return os.Getenv(keyEnv) }}
	client, err := providergw.NewClient(prov.Hosts(), "")
	if err != nil {
		t.Fatal(err)
	}
	if dump := os.Getenv("VIBECHAT_LIVE_DUMP"); dump != "" { // request and response bodies only, never headers
		client.Transport = &dumpTransport{next: client.Transport, path: dump}
	}
	prov.Client = client
	ls := &logs{}
	gw := providergw.New(providergw.WithModels(prov, map[string]string{"live": model}), ls.log)
	data := t.TempDir()
	isolate.Configure(isolate.Config{Hide: []string{data}, Require: true})
	t.Cleanup(func() { isolate.Configure(isolate.Config{}) })
	ad := New(Options{Label: "Pi live", Provider: "live", DataDir: data, Install: in, Gateway: gw,
		Models: []ModelEntry{{ID: "live", Label: model, ContextWindow: 32000, MaxTokens: 1024}}, Log: ls.log, Grace: 2 * time.Second})
	t.Cleanup(ad.Close)

	ws := t.TempDir()
	steps := []struct{ prompt, check string }{
		{"Create a file hello.py in the current directory that defines a function greet(name) returning \"Hello, \" + name + \"!\" and, under a __main__ guard, prints greet(\"world\"). Use the write tool.", "hello.py"},
		{"Edit hello.py with the edit tool: make greet return \"Hi, \" + name + \"!\" instead of \"Hello, \".", "Hi, "},
		{"Run `python3 hello.py` with the bash tool and tell me its exact output.", "Hi, world!"},
	}
	handle := ""
	for i, st := range steps {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		sess, err := ad.Open(ctx, codingagent.OpenRequest{Cwd: ws, ModelID: "live/live", Resume: handle})
		if err != nil {
			cancel()
			t.Fatalf("open: %v", err)
		}
		c := &collector{}
		start := time.Now()
		res, err := sess.Start(ctx, "s1", "r"+string(rune('1'+i)), codingagent.Prompt{Text: st.prompt}, c.emit).Wait()
		cancel()
		handle = sess.Handle()
		raw, _ := json.Marshal(c.wires())
		t.Logf("step %d (%s): err=%v result=%+v\nevents: %s", i+1, time.Since(start).Round(time.Millisecond), err, res, trunc(string(raw), 6000))
		if err != nil || res.Error != "" {
			t.Fatalf("step %d failed: %v %q\nlog: %s", i+1, err, res.Error, ls.all())
		}
		if res.ToolCalls < 1 {
			t.Fatalf("step %d: the model made no tool call; answer: %q", i+1, res.Text)
		}
		switch i {
		case 0:
			if b, _ := os.ReadFile(filepath.Join(ws, "hello.py")); !strings.Contains(string(b), "def greet") {
				t.Fatalf("hello.py: %q", b)
			}
		case 1:
			if b, _ := os.ReadFile(filepath.Join(ws, "hello.py")); !strings.Contains(string(b), "Hi, ") || strings.Contains(string(b), "Hello, ") {
				t.Fatalf("hello.py was not edited: %q", b)
			}
		case 2:
			if !strings.Contains(res.Text, st.check) {
				t.Fatalf("the answer does not carry the output: %q", res.Text)
			}
		}
	}
	if out, err := exec.Command("python3", filepath.Join(ws, "hello.py")).Output(); err != nil || strings.TrimSpace(string(out)) != "Hi, world!" {
		t.Fatalf("the file does not run: %q %v", out, err)
	}
	if strings.Contains(ls.all(), os.Getenv(keyEnv)) {
		t.Fatal("the key is in the log")
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

type dumpTransport struct {
	next http.RoundTripper
	path string
}

func (d *dumpTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f, _ := os.OpenFile(d.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		fmt.Fprintf(f, ">>> REQUEST\n%s\n", b)
	}
	resp, err := d.next.RoundTrip(r)
	if err == nil {
		fmt.Fprintf(f, "<<< RESPONSE %d\n", resp.StatusCode)
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(resp.Body, f), resp.Body}
	}
	return resp, err
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
