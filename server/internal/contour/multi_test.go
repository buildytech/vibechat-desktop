package contour

import (
	"context"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/server/internal/providergw"
)

// Cloud.ru GigaChat-4 first, OpenRouter second: the pilot contour of an RU server, data only.
const cloudruDoc = `{"contour":"ru","runtime":"pi","provider":"openai","openai":[
 {"id":"cloudru","label":"Cloud.ru GigaChat-4","baseUrl":"https://foundation-models.api.cloud.ru/v1","keyEnv":"VIBECHAT_CLOUDRU_KEY",
  "quirks":{"echoFields":["tool_state_id"]},"body":{"provider":{"sort":"price"}},
  "models":[{"id":"gigachat-4","label":"GigaChat-4 708B","upstream":"GigaChat/GigaChat-4-708B-A41","contextWindow":1000000,"maxTokens":8192}]},
 {"id":"openrouter","label":"OpenRouter","baseUrl":"https://openrouter.ai/api/v1","keyEnv":"VIBECHAT_OPENROUTER_KEY","compat":{"supportsDeveloperRole":true},
  "models":[{"id":"gpt-oss-120b","label":"gpt-oss-120b","upstream":"openai/gpt-oss-120b","contextWindow":131072,"maxTokens":8192}]}]}`

func TestSeveralOpenAIProvidersTheFirstIsTheDefault(t *testing.T) {
	c, err := Parse([]byte(cloudruDoc), "t")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.ProviderIDs(), ","); got != "cloudru,openrouter" || c.ProviderName() != "cloudru" || c.Compat() != nil {
		t.Fatalf("%s %s", got, c.ProviderName())
	}
	hosts, err := c.EgressHosts()
	if err != nil || strings.Join(hosts, ",") != "foundation-models.api.cloud.ru,openrouter.ai" {
		t.Fatalf("egress is the union of the providers' hosts: %v %v", hosts, err)
	}
	env := map[string]string{"VIBECHAT_CLOUDRU_KEY": "k1"}
	prov, models, err := c.BuildProvider(Env{Getenv: func(n string) string { return env[n] }})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].Provider != "cloudru" || models[1].Provider != "openrouter" || models[0].Upstream != "GigaChat/GigaChat-4-708B-A41" ||
		models[0].Compat != nil || models[1].Compat["supportsDeveloperRole"] != true {
		t.Fatalf("%+v", models)
	}
	parts := providergw.Parts(prov)
	if len(parts) != 2 || parts[0].Provider.Ready() != nil || parts[1].Provider.Ready() == nil || prov.Ready() != nil {
		t.Fatalf("each provider reads its own key: %v", parts)
	}
}

func TestSeveralOpenAIProvidersValidation(t *testing.T) {
	one := func(id, model string) string {
		return `{"id":"` + id + `","baseUrl":"https://h.example/v1","keyEnv":"VIBECHAT_` + strings.ToUpper(id) + `_KEY","models":[{"id":"` + model + `"}]}`
	}
	doc := func(blocks ...string) string {
		return `{"runtime":"pi","provider":"openai","openai":[` + strings.Join(blocks, ",") + `]}`
	}
	if _, err := Parse([]byte(doc(one("a", "m1"), one("b", "m2"))), "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(`{"runtime":"pi","provider":"openai","openai":`+one("a", "m1")+`}`), "t"); err != nil {
		t.Fatalf("one block, not a list, still works: %v", err)
	}
	bad := map[string]struct{ doc, want string }{
		"same provider id": {doc(one("a", "m1"), one("a", "m2")), "listed twice"},
		"same model id":    {doc(one("a", "m"), one("b", "m")), "unique across the providers"},
		"empty list":       {`{"runtime":"pi","provider":"openai","openai":[]}`, `needs an "openai" block`},
		"null block":       {`{"runtime":"pi","provider":"openai","openai":[null]}`, "empty provider block"},
		"unknown field":    {doc(strings.Replace(one("a", "m"), `"models"`, `"apiKey":"s","models"`, 1)), "unknown field"},
		"echo reserved":    {doc(strings.Replace(one("a", "m"), `"models"`, `"quirks":{"echoFields":["role"]},"models"`, 1)), "OpenAI message format"},
		"echo bad name":    {doc(strings.Replace(one("a", "m"), `"models"`, `"quirks":{"echoFields":["Tool-State"]},"models"`, 1)), "echo field"},
		"body reserved":    {doc(strings.Replace(one("a", "m"), `"models"`, `"body":{"messages":[]},"models"`, 1)), "cannot be preset"},
		"two kinds":        {strings.Replace(doc(one("a", "m")), `"openai":[`, `"gigachat":{"scope":"GIGACHAT_API_PERS","models":[{"id":"g"}]},"openai":[`, 1), "one provider only"},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(c.doc), "contour.json"); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

// The Studio's menu follows the list: the first provider's first model is the default, and a model of the second
// provider is a valid choice too.
func TestMenuOfSeveralProvidersAndItsDefault(t *testing.T) {
	c, err := Parse([]byte(cloudruDoc), "t")
	if err != nil {
		t.Fatal(err)
	}
	ad, err := Build(c, Env{Getenv: func(string) string { return "" }, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := ad.Models(context.Background())
	if len(ms) != 2 || ms[0].ID != "cloudru/gigachat-4" || ms[0].Provider != "cloudru" || ms[1].ID != "openrouter/gpt-oss-120b" || ms[1].Provider != "openrouter" {
		t.Fatalf("%+v", ms)
	}
	if ad.DefaultModel().ID != "cloudru/gigachat-4" {
		t.Fatalf("default %+v", ad.DefaultModel())
	}
	for _, id := range []string{"cloudru/gigachat-4", "openrouter/gpt-oss-120b"} {
		if m := ad.CheckModel(id); m != "" {
			t.Fatalf("%s: %s", id, m)
		}
	}
	for _, id := range []string{"cloudru/gpt-oss-120b", "openrouter/gigachat-4", "gigachat-4", "cursor/auto"} {
		if ad.CheckModel(id) == "" {
			t.Fatalf("%s accepted", id)
		}
	}
	if got := ad.Capabilities().Label; got != "Pi · Cloud.ru GigaChat-4 · OpenRouter" {
		t.Fatalf("label %q", got)
	}
}

// The example contour shipped in docs/contours stays valid, and is the one the RU pilot server runs.
func TestShippedExampleContour(t *testing.T) {
	c, err := Load("../../../docs/contours/ru-cloudru-openrouter.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.ProviderIDs(), ","); got != "cloudru,openrouter" {
		t.Fatal(got)
	}
	hosts, _ := c.EgressHosts()
	if strings.Join(hosts, ",") != "foundation-models.api.cloud.ru,openrouter.ai" {
		t.Fatal(hosts)
	}
}
