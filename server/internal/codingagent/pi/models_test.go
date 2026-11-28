package pi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelsJSONPointsAtTheGatewayAndHoldsNoSecret(t *testing.T) {
	raw, err := ModelsJSON("Yandex AI Studio", "http://127.0.0.1:4555/v1", []ModelEntry{
		{ID: "yagpt", Label: "YandexGPT", Upstream: "gpt://folder/yandexgpt/rc", ContextWindow: 32000, MaxTokens: 4096},
		{ID: "vision", Images: true, Reasoning: true},
	}, map[string]any{"supportsUsageInStreaming": false})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Providers map[string]struct {
			BaseURL string         `json:"baseUrl"`
			API     string         `json:"api"`
			APIKey  string         `json:"apiKey"`
			Compat  map[string]any `json:"compat"`
			Models  []struct {
				ID            string   `json:"id"`
				Name          string   `json:"name"`
				Input         []string `json:"input"`
				Reasoning     bool     `json:"reasoning"`
				ContextWindow int      `json:"contextWindow"`
				MaxTokens     int      `json:"maxTokens"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	p := doc.Providers[ProviderID]
	if len(doc.Providers) != 1 || p.BaseURL != "http://127.0.0.1:4555/v1" || p.API != "openai-completions" || p.APIKey != "vibechat-no-key" {
		t.Fatalf("%s", raw)
	}
	if p.Compat["supportsStore"] != false || p.Compat["supportsDeveloperRole"] != false || p.Compat["maxTokensField"] != "max_tokens" || p.Compat["supportsUsageInStreaming"] != false {
		t.Fatalf("compat %v", p.Compat)
	}
	if len(p.Models) != 2 || p.Models[0].ID != "yagpt" || p.Models[0].ContextWindow != 32000 || p.Models[1].Input[1] != "image" || !p.Models[1].Reasoning {
		t.Fatalf("models %+v", p.Models)
	}
	if strings.Contains(string(raw), "gpt://") {
		t.Fatal("the provider's own model name is host-side only")
	}
}

func TestValidateModels(t *testing.T) {
	good := []ModelEntry{{ID: "a"}, {ID: "b.c-d_1"}}
	if err := ValidateModels(good); err != nil {
		t.Fatal(err)
	}
	for name, ms := range map[string][]ModelEntry{
		"none": nil, "upper": {{ID: "A"}}, "space": {{ID: "a b"}}, "slash": {{ID: "gpt://x/y"}}, "dup": {{ID: "a"}, {ID: "a"}},
		"dash first": {{ID: "-rf"}}, "empty": {{ID: ""}}, "long": {{ID: strings.Repeat("a", 65)}}, "negative": {{ID: "a", MaxTokens: -1}},
	} {
		if ValidateModels(ms) == nil {
			t.Errorf("%s accepted", name)
		}
		if _, err := ModelsJSON("p", "http://x", ms, nil); err == nil {
			t.Errorf("%s: models.json made", name)
		}
	}
}

// A contour with several providers: the menu carries each model's own provider id, and a provider's compat keys ride on
// its models only.
func TestMenuAndModelsJSONOfSeveralProviders(t *testing.T) {
	ms := []ModelEntry{{ID: "giga", Label: "GigaChat-4", Provider: "cloudru"},
		{ID: "oss", Label: "gpt-oss", Provider: "openrouter", Compat: map[string]any{"supportsDeveloperRole": true}}}
	menu := Menu("cloudru", ms)
	if len(menu) != 2 || menu[0].ID != "cloudru/giga" || menu[1].ID != "openrouter/oss" || menu[1].Provider != "openrouter" {
		t.Fatalf("%+v", menu)
	}
	raw, err := ModelsJSON("Cloud.ru · OpenRouter", "http://127.0.0.1:18199/v1", ms, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Providers map[string]struct {
			Models []map[string]any `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	got := doc.Providers[ProviderID].Models
	if len(got) != 2 || got[0]["compat"] != nil || got[1]["compat"].(map[string]any)["supportsDeveloperRole"] != true {
		t.Fatalf("%s", raw)
	}
}
