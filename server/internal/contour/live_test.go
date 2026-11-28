package contour

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/codingagent/pi"
	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
)

// TestLiveContour runs one real chat of three steps (create a file, edit it, run it in the shell) through a contour
// file, the real Pi in the real jail and the real provider. It is off unless VIBECHAT_LIVE=1:
//
//	VIBECHAT_LIVE=1 VIBECHAT_PI_DIR=/tmp/pi VIBECHAT_LIVE_CONTOUR=contour.json \
//	  VIBECHAT_LIVE_ENVMAP=VIBECHAT_CLOUDRU_KEY=CLOUDRU_API_KEY VIBECHAT_LIVE_MODEL=cloudru/gigachat-4 \
//	  go test ./server/internal/contour -run LiveContour -v
//
// VIBECHAT_LIVE_ENVMAP ("NAME=SOURCE,...") reads the contour's key variables from other variables of this machine, so a
// key that lives under another name is used without being copied anywhere. VIBECHAT_LIVE_DUMP names a file that gets
// caps the answer of each model call (default 1024).
func TestLiveContour(t *testing.T) {
	file := os.Getenv("VIBECHAT_LIVE_CONTOUR")
	if os.Getenv("VIBECHAT_LIVE") != "1" || file == "" {
		t.Skip("the live run is off (VIBECHAT_LIVE=1 and VIBECHAT_LIVE_CONTOUR are needed)")
	}
	if err := isolate.JailAvailable(); err != nil {
		t.Skipf("no bubblewrap: %v", err)
	}
	cfg, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	envmap := map[string]string{}
	for _, kv := range strings.Split(os.Getenv("VIBECHAT_LIVE_ENVMAP"), ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			envmap[k] = v
		}
	}
	getenv := func(n string) string {
		if src, ok := envmap[n]; ok {
			return os.Getenv(src)
		}
		return os.Getenv(n)
	}
	var wrap func(http.RoundTripper) http.RoundTripper
	if dump := os.Getenv("VIBECHAT_LIVE_DUMP"); dump != "" {
		wrap = func(next http.RoundTripper) http.RoundTripper { return &dumper{next: next, path: dump} }
	}
	var mu sync.Mutex
	var lines []string
	logf := func(s string) { mu.Lock(); lines = append(lines, s); mu.Unlock() }
	data := t.TempDir()
	isolate.Configure(isolate.Config{Hide: []string{data}, Require: true})
	t.Cleanup(func() { isolate.Configure(isolate.Config{}) })
	ad, err := Build(cfg, Env{Getenv: getenv, LookPath: exec.LookPath, DataDir: data, Log: logf, WrapTransport: wrap})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ad.Close)
	if err := ad.Ready(); err != nil {
		t.Skipf("not ready: %v", err)
	}
	model := os.Getenv("VIBECHAT_LIVE_MODEL")
	if model == "" {
		model = ad.DefaultModel().ID
	}
	t.Logf("menu: %v; running %s", menuIDs(ad), model)

	ws := t.TempDir()
	steps := []string{
		"Create a file hello.py in the current directory that defines a function greet(name) returning \"Hello, \" + name + \"!\" and, under a __main__ guard, prints greet(\"world\"). Use the write tool.",
		"Edit hello.py with the edit tool: make greet return \"Hi, \" + name + \"!\" instead of \"Hello, \".",
		"Run `python3 hello.py` with the bash tool and tell me its exact output.",
	}
	handle, calls := "", 0
	for i, prompt := range steps {
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		sess, err := ad.Open(ctx, codingagent.OpenRequest{Cwd: ws, ModelID: model, Resume: handle})
		if err != nil {
			cancel()
			t.Fatalf("open: %v", err)
		}
		var types []string
		var tmu sync.Mutex
		start := time.Now()
		res, err := sess.Start(ctx, "s1", "r"+string(rune('1'+i)), codingagent.Prompt{Text: prompt}, func(e codingagent.Event) {
			tmu.Lock()
			types = append(types, e.Wire()["type"].(string))
			tmu.Unlock()
		}).Wait()
		cancel()
		handle = sess.Handle()
		counts := map[string]int{}
		for _, ty := range types {
			counts[ty]++
		}
		t.Logf("step %d (%s): err=%v tools=%d files=%v events=%v\nanswer: %s", i+1, time.Since(start).Round(time.Millisecond), err, res.ToolCalls, res.Files, counts, res.Text)
		if err != nil || res.Error != "" {
			t.Fatalf("step %d failed: %v %q\nlog: %s", i+1, err, res.Error, strings.Join(lines, "\n"))
		}
		if res.ToolCalls < 1 {
			t.Fatalf("step %d: no tool call", i+1)
		}
		calls += res.ToolCalls
		b, _ := os.ReadFile(filepath.Join(ws, "hello.py"))
		switch i {
		case 0:
			if !strings.Contains(string(b), "def greet") {
				t.Fatalf("hello.py: %q", b)
			}
		case 1:
			if !strings.Contains(string(b), "Hi, ") || strings.Contains(string(b), "Hello, ") {
				t.Fatalf("hello.py was not edited: %q", b)
			}
		case 2:
			if !strings.Contains(res.Text, "Hi, world!") {
				t.Fatalf("the answer lacks the output: %q", res.Text)
			}
		}
	}
	if out, err := exec.Command("python3", filepath.Join(ws, "hello.py")).Output(); err != nil || strings.TrimSpace(string(out)) != "Hi, world!" {
		t.Fatalf("the file does not run: %q %v", out, err)
	}
	t.Logf("3 steps ok, %d tool calls; gateway log:\n%s", calls, strings.Join(lines, "\n"))
	for k := range envmap {
		if v := getenv(k); v != "" && strings.Contains(strings.Join(lines, "\n"), v) {
			t.Fatal("a key is in the log")
		}
	}
}

func menuIDs(ad *pi.Adapter) []string {
	ms, _ := ad.Models(context.Background())
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

type dumper struct {
	next http.RoundTripper
	path string
}

func (d *dumper) RoundTrip(r *http.Request) (*http.Response, error) {
	f, _ := os.OpenFile(d.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if f != nil {
		defer f.Close()
	}
	if r.Body != nil && f != nil {
		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		_, _ = f.WriteString(">>> REQUEST " + r.URL.Path + "\n")
		_, _ = f.Write(b)
		_, _ = f.WriteString("\n")
		r.Body = io.NopCloser(strings.NewReader(string(b)))
	}
	resp, err := d.next.RoundTrip(r)
	if err != nil || f == nil {
		return resp, err
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_, _ = f.WriteString("<<< RESPONSE " + resp.Status + "\n")
	_, _ = f.Write(b)
	_, _ = f.WriteString("\n")
	resp.Body = io.NopCloser(strings.NewReader(string(b)))
	return resp, nil
}
