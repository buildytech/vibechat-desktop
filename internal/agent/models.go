// Package agent drives Cursor agents through the cursor-sdk-bridge for the host seam: one bridge process per app
// (started on first use with the key from the keychain), one Cursor agent per Studio session (created or resumed with
// the SDK sandbox on), and the translation of the bridge's run stream into Agent Contract v1 events.
package agent

import "strings"

// ModelParam is a Cursor model parameter value.
type ModelParam struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// Selection is the bridge's ModelSelection.
type Selection struct {
	ID     string       `json:"id"`
	Params []ModelParam `json:"params,omitempty"`
}

// Model is AgentModel of the contract.
type Model struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Label    string `json:"label"`
}

// Allowlist is the Studio's model list (vibecode host-core CURSOR_MODEL_LABELS), in menu order.
var Allowlist = []Model{
	{ID: "cursor/auto", Provider: "cursor", Label: "Auto"},
	{ID: "cursor/composer-2.5", Provider: "cursor", Label: "Composer 2.5"},
	{ID: "cursor/composer-2.5-fast", Provider: "cursor", Label: "Composer 2.5 Fast"},
	{ID: "cursor/grok-4.7", Provider: "cursor", Label: "Grok 4.7"},
	{ID: "cursor/claude-opus-5", Provider: "cursor", Label: "Opus 5"},
}

// Allowed reports whether id is in the allowlist.
func Allowed(id string) bool {
	for _, m := range Allowlist {
		if m.ID == id {
			return true
		}
	}
	return false
}

func pick(params []ModelParam, ids []string, def string, ok map[string]bool) string {
	for _, id := range ids {
		for _, p := range params {
			if p.ID == id && ok[p.Value] {
				return p.Value
			}
		}
	}
	return def
}

func set(vals ...string) map[string]bool {
	m := map[string]bool{}
	for _, v := range vals {
		m[v] = true
	}
	return m
}

var boolVals = set("true", "false")

// ToSelection maps a Studio model id and params to the Cursor selection (port of host-core toCursorModelSelection).
// Unknown values fall back to the model's defaults; grok-4.7 always sends reasoning_effort (never effort).
func ToSelection(modelID string, params []ModelParam) Selection {
	bare := strings.TrimPrefix(modelID, "cursor/")
	switch bare {
	case "auto", "default", "":
		return Selection{ID: "auto"}
	case "composer-2.5-fast":
		return Selection{ID: "composer-2.5", Params: []ModelParam{{"fast", "true"}}}
	case "composer-2.5":
		return Selection{ID: "composer-2.5", Params: []ModelParam{{"fast", "false"}}}
	case "grok-4.7":
		return Selection{ID: "grok-4.7", Params: []ModelParam{
			{"context", pick(params, []string{"context"}, "256k", set("256k", "500k"))},
			{"reasoning_effort", pick(params, []string{"reasoning_effort", "effort"}, "high", set("low", "medium", "high", "xhigh"))},
			{"fast", pick(params, []string{"fast"}, "true", boolVals)},
		}}
	case "claude-opus-5", "opus-5", "opus":
		return Selection{ID: "claude-opus-5", Params: []ModelParam{
			{"thinking", pick(params, []string{"thinking"}, "true", boolVals)},
			{"context", pick(params, []string{"context"}, "1m", set("300k", "1m"))},
			{"effort", pick(params, []string{"effort"}, "high", set("low", "medium", "high", "xhigh", "max"))},
			{"fast", pick(params, []string{"fast"}, "false", boolVals)},
		}}
	}
	if strings.HasSuffix(bare, "-fast") {
		return Selection{ID: strings.TrimSuffix(bare, "-fast"), Params: []ModelParam{{"fast", "true"}}}
	}
	return Selection{ID: bare, Params: params}
}
