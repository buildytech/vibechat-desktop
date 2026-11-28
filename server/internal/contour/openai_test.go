package contour

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/server/internal/providergw"
	"github.com/buildytech/vibechat-desktop/server/internal/providergw/stub"
)

// A provider of the OpenAI-compatible kind is one block of the config: where, how the key is sent, which headers,
// which quirks, which models. No code per provider.
func openAIDoc(base, extra string) string {
	return `{"contour":"ru","runtime":"pi","provider":"openai","openai":{"id":"gptoss","label":"gpt-oss-120b","baseUrl":"` + base + `",
 "keyEnv":"VIBECHAT_GPTOSS_KEY","headers":{"X-Tenant":"acme"},` + extra + `"compat":{"supportsDeveloperRole":true},
 "models":[{"id":"oss120","label":"gpt-oss-120b","upstream":"openai/gpt-oss-120b","contextWindow":128000,"maxTokens":4096}]}}`
}

func TestOpenAIBlockValidation(t *testing.T) {
	if _, err := Parse([]byte(openAIDoc("https://llm.example.ru/v1", "")), "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(`{"runtime":"pi","provider":"openai","openai":{"id":"local","baseUrl":"http://127.0.0.1:8000/v1","noKey":true,"models":[{"id":"m"}]}}`), "t"); err != nil {
		t.Fatalf("a keyless loopback endpoint: %v", err)
	}
	m := `"models":[{"id":"a"}]`
	bad := map[string]struct{ doc, want string }{
		"no block":          {`{"runtime":"pi","provider":"openai"}`, `needs an "openai" block`},
		"bad id":            {`{"runtime":"pi","provider":"openai","openai":{"id":"GPT oss","baseUrl":"https://h/v1","keyEnv":"VIBECHAT_X_KEY",` + m + `}}`, "openai.id"},
		"no base url":       {`{"runtime":"pi","provider":"openai","openai":{"id":"x","keyEnv":"VIBECHAT_X_KEY",` + m + `}}`, "baseUrl is required"},
		"http base url":     {`{"runtime":"pi","provider":"openai","openai":{"id":"x","baseUrl":"http://evil.example/v1","keyEnv":"VIBECHAT_X_KEY",` + m + `}}`, "must be https"},
		"key env anything":  {`{"runtime":"pi","provider":"openai","openai":{"id":"x","baseUrl":"https://h/v1","keyEnv":"HOME",` + m + `}}`, "keyEnv"},
		"key and noKey":     {`{"runtime":"pi","provider":"openai","openai":{"id":"x","baseUrl":"https://h/v1","keyEnv":"VIBECHAT_X_KEY","noKey":true,` + m + `}}`, "exclude"},
		"no key at all":     {`{"runtime":"pi","provider":"openai","openai":{"id":"x","baseUrl":"https://h/v1",` + m + `}}`, "keyEnv"},
		"auth header":       {`{"runtime":"pi","provider":"openai","openai":{"id":"x","baseUrl":"https://h/v1","keyEnv":"VIBECHAT_X_KEY","headers":{"Authorization":"Bearer x"},` + m + `}}`, "set by the gateway"},
		"header injection":  {`{"runtime":"pi","provider":"openai","openai":{"id":"x","baseUrl":"https://h/v1","keyEnv":"VIBECHAT_X_KEY","headers":{"X-A":"a\r\nX-B: b"},` + m + `}}`, "bad value"},
		"bad auth scheme":   {`{"runtime":"pi","provider":"openai","openai":{"id":"x","baseUrl":"https://h/v1","keyEnv":"VIBECHAT_X_KEY","auth":{"scheme":"Bearer x y"},` + m + `}}`, "auth scheme"},
		"unknown compat":    {`{"runtime":"pi","provider":"openai","openai":{"id":"x","baseUrl":"https://h/v1","keyEnv":"VIBECHAT_X_KEY","compat":{"apiKey":"x"},` + m + `}}`, "compat key"},
		"compat wrong type": {`{"runtime":"pi","provider":"openai","openai":{"id":"x","baseUrl":"https://h/v1","keyEnv":"VIBECHAT_X_KEY","compat":{"supportsStore":"yes"},` + m + `}}`, "true or false"},
		"unknown field":     {`{"runtime":"pi","provider":"openai","openai":{"id":"x","baseUrl":"https://h/v1","keyEnv":"VIBECHAT_X_KEY","apiKey":"s",` + m + `}}`, "unknown field"},
		"two providers":     {`{"runtime":"pi","provider":"openai","openai":{"id":"x","baseUrl":"https://h/v1","keyEnv":"VIBECHAT_X_KEY",` + m + `},"gigachat":{"scope":"GIGACHAT_API_PERS",` + m + `}}`, "one provider only"},
		"cursor with block": {`{"runtime":"cursor-sdk","openai":{"id":"x","baseUrl":"https://h/v1"}}`, "takes no provider"},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(c.doc), "contour.json"); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestOpenAIBlockEgressAndMenu(t *testing.T) {
	c, err := Parse([]byte(openAIDoc("https://llm.example.ru/v1", "")), "t")
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := c.EgressHosts()
	if err != nil || len(hosts) != 1 || hosts[0] != "llm.example.ru" {
		t.Fatalf("%v %v", hosts, err)
	}
	if c.ProviderName() != "gptoss" || c.Compat()["supportsDeveloperRole"] != true {
		t.Fatalf("%q %v", c.ProviderName(), c.Compat())
	}
}

func serve(t *testing.T, c Config, key string) (string, string) {
	t.Helper()
	prov, _, err := c.BuildProvider(Env{Getenv: func(n string) string {
		if n == "VIBECHAT_GPTOSS_KEY" {
			return key
		}
		return ""
	}})
	if err != nil {
		t.Fatal(err)
	}
	gw := providergw.New(prov, nil)
	base, err := gw.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	tok, _ := gw.Mint()
	return base, tok
}

func chat(t *testing.T, base, tok string, body map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, base+"/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out)
}

func twoWrites(n int, _ stub.Request) stub.Reply {
	if n == 0 {
		return stub.Reply{Calls: []stub.Call{{Name: "write", Args: map[string]any{"path": "a"}}, {Name: "write", Args: map[string]any{"path": "b"}}}}
	}
	return stub.Reply{Text: "ok"}
}

func TestOpenAIBlockSendsTheKeyAsConfiguredAndOnlyIroningQuirksWhenAsked(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	for _, tc := range []struct {
		name, extra, wantAuth string
		unique                bool
	}{
		{"defaults: Bearer, ids untouched", "", "Bearer k-1", false},
		{"Api-Key scheme and the unique-id quirk", `"auth":{"scheme":"Api-Key"},"quirks":{"uniqueToolCallIds":true},`, "Api-Key k-1", true},
		{"own header, bare key", `"auth":{"header":"X-Api-Key","scheme":"none"},`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := stub.OpenAI(twoWrites, stub.NameAsID)
			defer up.Close()
			c, err := Parse([]byte(openAIDoc(up.URL+"/v1", tc.extra)), "t")
			if err != nil {
				t.Fatal(err)
			}
			base, tok := serve(t, c, " k-1\n")
			out := chat(t, base, tok, map[string]any{"model": "oss120", "messages": msgs})
			r := up.Requests()[0]
			if r.Body["model"] != "openai/gpt-oss-120b" || r.Headers.Get("X-Tenant") != "acme" {
				t.Fatalf("model/header: %s %v", r.Raw, r.Headers)
			}
			if tc.wantAuth != "" && r.Authorization != tc.wantAuth {
				t.Fatalf("auth %q", r.Authorization)
			}
			if tc.wantAuth == "" && (r.Headers.Get("X-Api-Key") != "k-1" || r.Authorization != "") {
				t.Fatalf("bare key header: %v", r.Headers)
			}
			unique := strings.Contains(out, `"id":"vc`)
			if unique != tc.unique || (!tc.unique && !strings.Contains(out, `"id":"write"`)) {
				t.Fatalf("quirk off/on mismatch (unique=%v): %s", unique, out)
			}
			if bytes.Contains(r.Raw, []byte(tok)) {
				t.Fatal("the gateway token went upstream")
			}
		})
	}
}

func TestOpenAIBlockWithoutKeyIsNotReadyAndModelsAreAllowListed(t *testing.T) {
	up := stub.OpenAI(twoWrites)
	defer up.Close()
	c, _ := Parse([]byte(openAIDoc(up.URL+"/v1", "")), "t")
	prov, _, err := c.BuildProvider(Env{Getenv: func(string) string { return "" }})
	if err != nil || prov.Ready() == nil {
		t.Fatalf("no key must not be ready: %v", err)
	}
	base, tok := serve(t, c, "k")
	if out := chat(t, base, tok, map[string]any{"model": "openai/gpt-oss-20b", "messages": []any{}}); !strings.Contains(out, "not configured") {
		t.Fatalf("an unlisted model must be refused: %s", out)
	}
	if len(up.Requests()) != 0 {
		t.Fatal("a refused model reached the provider")
	}
}
