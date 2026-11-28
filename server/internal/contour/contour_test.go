package contour

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
)

const yandexDoc = `{"contour":"ru","runtime":"pi","provider":"yandex","yandex":{"folderId":"b1gabc","models":[
 {"id":"yagpt","label":"YandexGPT","upstream":"gpt://{folder}/yandexgpt/rc","contextWindow":32000},{"id":"alice","upstream":"gpt://{folder}/aliceai-llm/latest"}]}}`
const gigaDoc = `{"contour":"ru","runtime":"pi","provider":"gigachat","gigachat":{"scope":"GIGACHAT_API_PERS","models":[{"id":"giga","upstream":"GigaChat-2-Pro"}]}}`

func TestDefaultContourIsCursor(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || c.RuntimeID() != codingagent.RuntimeCursorSDK || c.UsesPi() {
		t.Fatalf("%+v %v", c, err)
	}
	if c, err = Load(""); err != nil || c.UsesPi() {
		t.Fatalf("%+v %v", c, err)
	}
	hosts, err := c.EgressHosts()
	if err != nil || len(hosts) != 0 {
		t.Fatalf("%v %v", hosts, err)
	}
	if c, err = Parse([]byte(`{"runtime":"cursor-sdk","contour":"eu"}`), "t"); err != nil || c.RuntimeID() != "cursor-sdk" {
		t.Fatalf("%v", err)
	}
}

func TestValidation(t *testing.T) {
	good := map[string]string{"yandex": yandexDoc, "gigachat": gigaDoc}
	for name, doc := range good {
		if _, err := Parse([]byte(doc), name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]struct{ doc, want string }{
		"unknown field (a key typed in)": {`{"runtime":"pi","provider":"yandex","apiKey":"AQVN-secret","yandex":{"folderId":"f","models":[{"id":"a"}]}}`, "unknown field"},
		"key inside the block":           {`{"runtime":"pi","provider":"yandex","yandex":{"folderId":"f","apiKey":"x","models":[{"id":"a"}]}}`, "unknown field"},
		"unknown runtime":                {`{"runtime":"openrouter"}`, "unknown runtime"},
		"pi without provider":            {`{"runtime":"pi"}`, `needs "provider"`},
		"unknown provider":               {`{"runtime":"pi","provider":"anthropic"}`, "unknown provider"},
		"provider without its block":     {`{"runtime":"pi","provider":"yandex"}`, `needs a "yandex" block`},
		"two providers":                  {`{"runtime":"pi","provider":"yandex","yandex":{"folderId":"f","models":[{"id":"a"}]},"gigachat":{"scope":"GIGACHAT_API_PERS","models":[{"id":"a"}]}}`, "one provider only"},
		"cursor with a provider":         {`{"runtime":"cursor-sdk","provider":"yandex"}`, "takes no provider"},
		"no folder":                      {`{"runtime":"pi","provider":"yandex","yandex":{"models":[{"id":"a"}]}}`, "folderId"},
		"no models":                      {`{"runtime":"pi","provider":"yandex","yandex":{"folderId":"f"}}`, "no models"},
		"bad scope":                      {`{"runtime":"pi","provider":"gigachat","gigachat":{"scope":"X","models":[{"id":"a"}]}}`, "scope"},
		"http base url":                  {`{"runtime":"pi","provider":"yandex","yandex":{"folderId":"f","baseUrl":"http://evil.example/v1","models":[{"id":"a"}]}}`, "must be https"},
		"bad model id":                   {`{"runtime":"pi","provider":"yandex","yandex":{"folderId":"f","models":[{"id":"gpt://x"}]}}`, "model id"},
		"not json":                       {`{`, "contour"},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.doc), "contour.json")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
			if strings.Contains(err.Error(), "AQVN-secret") {
				t.Fatal("the error repeats a secret")
			}
		})
	}
	if _, err := Parse([]byte(`{"runtime":"pi","provider":"yandex","yandex":{"folderId":"f","baseUrl":"http://127.0.0.1:9/v1","models":[{"id":"a"}]}}`), "t"); err != nil {
		t.Fatalf("a loopback stub URL is allowed: %v", err)
	}
}

func TestEgressListsOnlyTheChosenProvider(t *testing.T) {
	y, _ := Parse([]byte(yandexDoc), "t")
	hosts, err := y.EgressHosts()
	if err != nil || strings.Join(hosts, ",") != "ai.api.cloud.yandex.net" {
		t.Fatalf("%v %v", hosts, err)
	}
	g, _ := Parse([]byte(gigaDoc), "t")
	hosts, err = g.EgressHosts()
	if err != nil || strings.Join(hosts, ",") != "api.giga.chat,ngw.devices.sberbank.ru" {
		t.Fatalf("%v %v", hosts, err)
	}
	for _, h := range append(hosts, "ai.api.cloud.yandex.net") {
		if strings.Contains(h, "cursor") || strings.Contains(h, "openai") || strings.Contains(h, "anthropic") {
			t.Fatalf("a foreign host in the list: %s", h)
		}
	}
}

func TestBuildResolvesModelsAndFailsClosedWhenSomethingIsMissing(t *testing.T) {
	c, _ := Parse([]byte(yandexDoc), "t")
	noKey := Env{Getenv: func(string) string { return "" }, DataDir: t.TempDir(), LookPath: func(string) (string, error) { return "", errors.New("none") }}
	ad, err := Build(c, noKey)
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := ad.Models(nil)
	if len(ms) != 2 || ms[0].ID != "yandex/yagpt" || ms[0].Label != "YandexGPT" || ms[1].Label != "alice" {
		t.Fatalf("%+v", ms)
	}
	// Pi is not installed: the reason is the install's, not a fallback
	if err := ad.Ready(); err == nil || (!strings.Contains(err.Error(), "folder is not set") && !strings.Contains(err.Error(), "bubblewrap")) {
		t.Fatalf("%v", err)
	}
	if ad.EgressHosts()[0] != "ai.api.cloud.yandex.net" {
		t.Fatalf("%v", ad.EgressHosts())
	}
	if _, err := Build(Config{}, noKey); err == nil {
		t.Fatal("the Cursor contour has no Pi adapter")
	}
	if _, err := Build(Config{Runtime: "pi"}, noKey); err == nil {
		t.Fatal("an invalid config built an adapter")
	}
}

func TestProviderKeysComeFromTheEnvironmentPerCall(t *testing.T) {
	c, _ := Parse([]byte(yandexDoc), "t")
	key := ""
	prov, _, err := c.BuildProvider(Env{Getenv: func(n string) string {
		if n == EnvYandexKey {
			return " " + key + "\n"
		}
		return ""
	}})
	if err != nil {
		t.Fatal(err)
	}
	if prov.Ready() == nil {
		t.Fatal("ready without a key")
	}
	key = "k-1"
	if err := prov.Ready(); err != nil {
		t.Fatalf("the key set later (a service restart is not needed for rotation) is not seen: %v", err)
	}
	g, _ := Parse([]byte(gigaDoc), "t")
	gp, _, err := g.BuildProvider(Env{Getenv: func(string) string { return "" }})
	if err != nil || gp.Ready() == nil {
		t.Fatalf("gigachat without a key must not be ready: %v", err)
	}
	if _, _, err := (Config{Runtime: "pi", Provider: "gigachat", GigaChat: &GigaChatConfig{Scope: "GIGACHAT_API_PERS", CAFile: "/nonexistent/ca.pem", Models: g.GigaChat.Models}}).BuildProvider(Env{}); err == nil {
		t.Fatal("a missing CA bundle must be an error, not a silent system-roots fallback")
	}
}

func TestLoadReadsTheFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "contour.json")
	if err := os.WriteFile(p, []byte(gigaDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil || !c.UsesPi() || c.Provider != "gigachat" {
		t.Fatalf("%+v %v", c, err)
	}
	if err := os.WriteFile(p, []byte(`{"runtime":"nope"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("the error must name the file: %v", err)
	}
}
