package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/codingagent/pi"
	"github.com/buildytech/vibechat-desktop/server/internal/contour"
	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
	"github.com/buildytech/vibechat-desktop/server/internal/providergw/stub"
)

// The RU contour through the real host routes: contour.json -> the Pi adapter (real, pinned Pi in the real jail) -> the
// provider gateway -> a stub Yandex AI Studio. Needs bubblewrap, Node >= 22.19 and the pinned Pi installed by fetch-pi:
//
//	go run ./cmd/fetch-pi -out /tmp/pi && VIBECHAT_PI_DIR=/tmp/pi go test ./server/internal/host -run PiContour -v
func TestPiContourEndToEndThroughTheHostRoutes(t *testing.T) {
	if err := isolate.JailAvailable(); err != nil {
		t.Skipf("no bubblewrap: %v", err)
	}
	dir := os.Getenv("VIBECHAT_PI_DIR")
	if dir == "" {
		t.Skip("VIBECHAT_PI_DIR is not set (the pinned Pi installed by fetch-pi)")
	}
	if _, err := pi.Locate(pi.PinnedLock(), os.Getenv("VIBECHAT_PI_NODE"), dir, exec.LookPath); err != nil {
		t.Skipf("no usable Pi/Node: %v", err)
	}
	up := stub.OpenAI(func(n int, req stub.Request) stub.Reply {
		if n == 0 {
			return stub.Reply{Text: "Adding a page. ", Calls: []stub.Call{{Name: "write", Args: map[string]any{"path": "about.html", "content": "<h1>About</h1>\n"}}}}
		}
		return stub.Reply{Text: "Added about.html."}
	})
	defer up.Close()
	cfg, err := contour.Parse([]byte(`{"contour":"ru","runtime":"pi","provider":"yandex","yandex":{"folderId":"b1gtest","baseUrl":"`+up.URL+`/v1","models":[{"id":"yagpt","label":"YandexGPT","upstream":"gpt://{folder}/yandexgpt/rc"}]}}`), "test")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pi = contour.PiConfig{Node: os.Getenv("VIBECHAT_PI_NODE"), Dir: dir}
	const key = "yc-test-key-not-real-0001"
	var logs []string
	ad, err := contour.Build(cfg, contour.Env{Getenv: func(n string) string {
		if n == contour.EnvYandexKey {
			return key
		}
		return ""
	}, LookPath: exec.LookPath, DataDir: t.TempDir(), Log: func(s string) { logs = append(logs, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := ad.Ready(); err != nil {
		t.Fatal(err)
	}
	e := newEnvAgents(t, pinnedSeam(t), true, codingagent.RuntimePi, []codingagent.Adapter{ad})
	t.Cleanup(ad.Close)
	isolate.Configure(isolate.Config{Hide: []string{e.dir}, Require: true})
	t.Cleanup(func() { isolate.Configure(isolate.Config{}) })

	_, caps := goldenCall(t, e, "GET", "/host/capabilities", nil)
	if caps.(map[string]any)["agent_sandbox"] != true {
		t.Fatalf("capabilities %v", caps)
	}
	_, p := goldenCall(t, e, "POST", "/projects", map[string]any{"name": "ru", "templateId": "mini"})
	pid := p.(map[string]any)["project"].(map[string]any)["id"].(string)
	base := "/projects/" + pid + "/agent"
	_, ac := goldenCall(t, e, "GET", base+"/capabilities", nil)
	m := ac.(map[string]any)
	models := m["models"].([]any)
	if m["runtime"] != "pi" || m["runtimeLabel"] != "Pi · Yandex AI Studio" || len(models) != 1 || models[0].(map[string]any)["id"] != "yandex/yagpt" {
		t.Fatalf("capabilities %v", m)
	}
	_, s := goldenCall(t, e, "POST", base+"/sessions", nil)
	sid := s.(map[string]any)["session"].(map[string]any)["id"].(string)
	ch, unsub := e.host.bus.Subscribe(sid)
	defer unsub()

	code, out := goldenCall(t, e, "POST", base+"/sessions/"+sid+"/messages", map[string]any{"text": "add an about page", "modelId": "yandex/yagpt"})
	o := out.(map[string]any)
	if code != 200 || o["status"] != "idle" || o["toolCalls"] != float64(1) || o["filesChanged"] != float64(1) || !strings.Contains(o["text"].(string), "Added about.html.") {
		t.Fatalf("outcome %d %v\n%s", code, o, strings.Join(logs, "\n"))
	}
	var types []string
drain:
	for {
		select {
		case ev := <-ch:
			types = append(types, ev.Wire()["type"].(string))
		default:
			break drain
		}
	}
	joined := strings.Join(types, " ")
	for _, want := range []string{"session.status", "message.delta", "tool.started", "tool.finished", "usage", "files.changed"} {
		if !strings.Contains(joined, want) {
			t.Errorf("event %s missing from: %s", want, joined)
		}
	}
	// the change landed in the project, a Cursor-only model is refused for this runtime, a Cursor key is not asked for
	if got := findFile(t, e.dir, "about.html"); got != "<h1>About</h1>\n" {
		t.Fatalf("about.html: %q", got)
	}
	if code, _ := goldenCall(t, e, "POST", base+"/sessions/"+sid+"/messages", map[string]any{"text": "x", "modelId": "cursor/auto"}); code != 400 {
		t.Fatalf("a Cursor model on a Pi chat: %d", code)
	}
	if strings.Contains(strings.Join(logs, "\n"), key) || len(up.Requests()) != 2 || up.Requests()[0].Authorization != "Api-Key "+key {
		t.Fatalf("provider requests %d, logs %v", len(up.Requests()), logs)
	}
	_, cr := goldenCall(t, e, "GET", "/me/credentials", nil)
	for _, c := range cr.(map[string]any)["credentials"].([]any) {
		if c.(map[string]any)["runtime"] == "pi" {
			t.Fatalf("Pi has no key slot in Settings: %v", c)
		}
	}
}

func findFile(t *testing.T, root, name string) string {
	t.Helper()
	var found string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Base(p) == name && !strings.Contains(p, "/.git/") {
			if b, rerr := os.ReadFile(p); rerr == nil && found == "" {
				found = string(b)
			}
		}
		return nil
	})
	return found
}
