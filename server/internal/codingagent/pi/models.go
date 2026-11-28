package pi

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
)

// ModelEntry is one model of the contour config's menu.
type ModelEntry struct {
	// ID is the short name the Studio and Pi use: [a-z0-9][a-z0-9._-]*. The Studio's model id is "<provider>/<ID>".
	ID    string `json:"id"`
	Label string `json:"label"`
	// Upstream is the provider's own model name ("" = ID); the gateway swaps it in.
	Upstream      string `json:"upstream,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	MaxTokens     int    `json:"maxTokens,omitempty"`
	Reasoning     bool   `json:"reasoning,omitempty"`
	Images        bool   `json:"images,omitempty"`
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Validate checks the entries: ids unique and safe (they end up in argv and in a file name).
func ValidateModels(ms []ModelEntry) error {
	if len(ms) == 0 {
		return errors.New("no models are configured")
	}
	seen := map[string]bool{}
	for _, m := range ms {
		if !idPattern.MatchString(m.ID) {
			return fmt.Errorf("model id %q: use lower-case letters, digits, '.', '_' and '-'", m.ID)
		}
		if seen[m.ID] {
			return fmt.Errorf("model id %q is listed twice", m.ID)
		}
		seen[m.ID] = true
		if m.ContextWindow < 0 || m.MaxTokens < 0 {
			return fmt.Errorf("model %q: negative limits", m.ID)
		}
	}
	return nil
}

// ProviderID is the provider id of Pi's models.json: the one provider the host generates.
const ProviderID = "vibechat"

// Menu is the Studio's model list: "<provider>/<id>".
func Menu(provider string, ms []ModelEntry) []codingagent.Model {
	out := make([]codingagent.Model, 0, len(ms))
	for _, m := range ms {
		out = append(out, codingagent.Model{ID: provider + "/" + m.ID, Provider: provider, Label: firstNonEmpty(m.Label, m.ID)})
	}
	return out
}

// GatewayEnv is the environment variable of the jail that holds the run's gateway token; models.json refers to it as
// "$VIBECHAT_GATEWAY_KEY" and never contains it.
const GatewayEnv = "VIBECHAT_GATEWAY_KEY"

// ModelsJSON is Pi's models.json for a run: one provider (the host-side gateway at baseURL) and the contour's models.
// The credential is the gateway token from the environment; the real provider key is never written or sent here.
func ModelsJSON(provider, baseURL string, ms []ModelEntry, extraCompat map[string]any) ([]byte, error) {
	if err := ValidateModels(ms); err != nil {
		return nil, err
	}
	compat := map[string]any{
		// What a generic OpenAI-compatible endpoint behind the gateway can be trusted with: nothing OpenAI-specific.
		"supportsStore":                               false,
		"supportsDeveloperRole":                       false,
		"supportsReasoningEffort":                     false,
		"supportsStrictMode":                          false,
		"supportsUsageInStreaming":                    true,
		"maxTokensField":                              "max_tokens",
		"requiresNonNullAssistantContent":             true,
		"requiresAssistantAfterToolResult":            false,
		"supportsMidConvoSystemMessages":              false,
		"sendSessionAffinityHeaders":                  false,
		"requiresReasoningContentOnAssistantMessages": false,
	}
	for k, v := range extraCompat {
		compat[k] = v
	}
	models := make([]any, 0, len(ms))
	for _, m := range ms {
		input := []string{"text"}
		if m.Images {
			input = append(input, "image")
		}
		e := map[string]any{"id": m.ID, "name": firstNonEmpty(m.Label, m.ID), "reasoning": m.Reasoning, "input": input,
			"cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}}
		if m.ContextWindow > 0 {
			e["contextWindow"] = m.ContextWindow
		}
		if m.MaxTokens > 0 {
			e["maxTokens"] = m.MaxTokens
		}
		models = append(models, e)
	}
	doc := map[string]any{"providers": map[string]any{ProviderID: map[string]any{
		"name": provider, "baseUrl": baseURL, "api": "openai-completions", "apiKey": "$" + GatewayEnv,
		"compat": compat, "models": models,
	}}}
	return json.MarshalIndent(doc, "", "  ")
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
