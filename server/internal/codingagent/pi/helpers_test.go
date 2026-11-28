package pi

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
	"github.com/buildytech/vibechat-desktop/server/internal/providergw"
	"github.com/buildytech/vibechat-desktop/server/internal/providergw/stub"
)

const testKey = "SECRET-YANDEX-KEY-for-tests-only"

var testModels = []ModelEntry{{ID: "yagpt", Label: "YandexGPT", Upstream: "gpt://folder/yandexgpt/rc", ContextWindow: 32000, MaxTokens: 4096}}

// collector gathers the events of a run.
type collector struct {
	mu     sync.Mutex
	events []codingagent.Event
}

func (c *collector) emit(e codingagent.Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}
func (c *collector) wires() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, len(c.events))
	for i, e := range c.events {
		out[i] = e.Wire()
	}
	return out
}

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) log(s string) { l.mu.Lock(); l.lines = append(l.lines, s); l.mu.Unlock() }
func (l *logs) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := ""
	for _, x := range l.lines {
		s += x + "\n"
	}
	return s
}

// harness is an adapter over a stub OpenAI-compatible provider through the real gateway.
type harness struct {
	ad   *Adapter
	up   *stub.Server
	data string
	logs *logs
}

func newHarness(t *testing.T, in Install, script stub.Script, tap ...func([]byte)) *harness {
	t.Helper()
	up := stub.OpenAI(script)
	t.Cleanup(up.Close)
	client, err := providergw.NewClient([]string{"127.0.0.1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	y := &providergw.Yandex{BaseURL: up.URL + "/v1", FolderID: "folder", Key: func() string { return testKey }, Client: client}
	ls := &logs{}
	gw := providergw.New(providergw.WithModels(y, map[string]string{"yagpt": "gpt://folder/yandexgpt/rc"}), ls.log)
	data := t.TempDir()
	isolate.Configure(isolate.Config{Hide: []string{data}, Require: true})
	t.Cleanup(func() { isolate.Configure(isolate.Config{}) })
	opts := Options{Label: "Pi test", Provider: "yandex", DataDir: data, Install: in, Gateway: gw, Models: testModels, Log: ls.log, Grace: time.Second}
	if len(tap) > 0 {
		opts.Tap = tap[0]
	}
	ad := New(opts)
	t.Cleanup(ad.Close)
	return &harness{ad: ad, up: up, data: data, logs: ls}
}

func (h *harness) run(t *testing.T, ctx context.Context, cwd, resume, prompt string) (*collector, codingagent.Result, error, string) {
	t.Helper()
	sess, err := h.ad.Open(ctx, codingagent.OpenRequest{Cwd: cwd, ModelID: "yandex/yagpt", Resume: resume})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c := &collector{}
	run := sess.Start(ctx, "s1", "r1", codingagent.Prompt{Text: prompt}, c.emit)
	res, err := run.Wait()
	return c, res, err, sess.Handle()
}

func needJail(t *testing.T) {
	t.Helper()
	if err := isolate.JailAvailable(); err != nil {
		t.Skipf("no bubblewrap here: %v", err)
	}
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func providergwNoKey(t *testing.T) *providergw.Gateway {
	t.Helper()
	y := &providergw.Yandex{FolderID: "f", Key: func() string { return "" }}
	return providergw.New(providergw.WithModels(y, map[string]string{"yagpt": "x"}), nil)
}

// newGigaHarness is the same over a stub GigaChat (OAuth + functions format) through the gateway's translator.
func newGigaHarness(t *testing.T, in Install, script stub.Script) (*harness, *stub.Server) {
	t.Helper()
	up := stub.GigaChat(script)
	t.Cleanup(up.Close)
	client, err := providergw.NewClient([]string{"127.0.0.1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	tokens := &providergw.GigaTokens{AuthKey: func() string { return testKey }, Scope: "GIGACHAT_API_PERS", URL: up.URL + "/api/v2/oauth", Client: client}
	g := &providergw.GigaChat{BaseURL: up.URL + "/v1", Tokens: tokens, Client: client}
	ls := &logs{}
	gw := providergw.New(providergw.WithModels(g, map[string]string{"giga": "GigaChat-2-Pro"}), ls.log)
	data := t.TempDir()
	isolate.Configure(isolate.Config{Hide: []string{data}, Require: true})
	t.Cleanup(func() { isolate.Configure(isolate.Config{}) })
	ad := New(Options{Label: "Pi test", Provider: "gigachat", DataDir: data, Install: in, Gateway: gw, Models: []ModelEntry{{ID: "giga", Label: "GigaChat"}}, Log: ls.log, Grace: time.Second})
	t.Cleanup(ad.Close)
	return &harness{ad: ad, up: up, data: data, logs: ls}, up
}
