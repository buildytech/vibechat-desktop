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
