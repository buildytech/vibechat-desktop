package cursor

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/server/internal/bridge"
)

// A trimmed ListModels answer of a real key (shape as of SDK 1.2.0).
const catalogJSON = `{"items":[
 {"id":"default","displayName":"Auto","variants":[{"displayName":"Auto","isDefault":true}]},
 {"id":"composer-2.5","displayName":"Composer 2.5","parameters":[{"id":"fast","values":[{"value":"false"},{"value":"true"}]}],
  "variants":[{"params":[{"id":"fast","value":"true"}],"isDefault":true},{"params":[{"id":"fast","value":"false"}]}]},
 {"id":"claude-opus-5","displayName":"Claude Opus 5","parameters":[{"id":"thinking","values":[{"value":"true"},{"value":"false"}]},
  {"id":"effort","values":[{"value":"low"},{"value":"high"}]},{"id":"fast","values":[{"value":"false"},{"value":"true"}]}],
  "variants":[{"params":[{"id":"cyber","value":"false"},{"id":"thinking","value":"true"},{"id":"effort","value":"high"},{"id":"fast","value":"false"}],"isDefault":true}]},
 {"id":"gemini-3-flash","displayName":"Gemini 3 Flash"}]}`

func testCatalog(t *testing.T) Catalog {
	var res struct{ Items Catalog }
	if err := json.Unmarshal([]byte(catalogJSON), &res); err != nil {
		t.Fatal(err)
	}
	return res.Items
}

func TestMenuComesFromTheCatalog(t *testing.T) {
	c := testCatalog(t)
	var ids []string
	for _, m := range c.Menu(nil) {
		ids = append(ids, m.ID+"="+m.Label)
	}
	want := []string{"cursor/auto=Auto", "cursor/composer-2.5=Composer 2.5", "cursor/composer-2.5-fast=Composer 2.5 Fast",
		"cursor/claude-opus-5=Claude Opus 5", "cursor/claude-opus-5-fast=Claude Opus 5 Fast", "cursor/gemini-3-flash=Gemini 3 Flash"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("%v", ids)
	}
	if got := c.Menu(map[string]bool{"cursor/claude-opus-5": true}); len(got) != 5 || got[3].ID != "cursor/claude-opus-5-fast" {
		t.Fatalf("unavailable not removed: %v", got)
	}
	if got := (Catalog{}).Menu(nil); got == nil || len(got) != 0 {
		t.Fatal("empty catalog must give an empty (non-nil) list")
	}
}

func TestSelectUsesDefaultVariantAndFast(t *testing.T) {
	c := testCatalog(t)
	check := func(id string, params []ModelParam, want Selection) {
		t.Helper()
		got, err := c.Select(id, params)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Select(%s) = %+v, %v; want %+v", id, got, err, want)
		}
	}
	check("cursor/composer-2.5-fast", nil, Selection{ID: "composer-2.5", Params: []ModelParam{{ID: "fast", Value: "true"}}})
	check("cursor/composer-2.5", nil, Selection{ID: "composer-2.5", Params: []ModelParam{{ID: "fast", Value: "false"}}})
	check("cursor/auto", nil, Selection{ID: "default"})
	check("cursor/claude-opus-5", []ModelParam{{ID: "effort", Value: "low"}, {ID: "effort", Value: "bogus"}, {ID: "fast", Value: "true"}, {ID: "x", Value: "y"}},
		Selection{ID: "claude-opus-5", Params: []ModelParam{{ID: "cyber", Value: "false"}, {ID: "thinking", Value: "true"}, {ID: "effort", Value: "low"}, {ID: "fast", Value: "false"}}})
	check("cursor/gemini-3-flash", nil, Selection{ID: "gemini-3-flash"})
	if _, err := c.Select("cursor/gpt-9", nil); !errors.Is(err, ErrUnknownModel) {
		t.Errorf("unknown model: %v", err)
	}
	if _, err := c.Select("cursor/gemini-3-flash-fast", nil); !errors.Is(err, ErrUnknownModel) {
		t.Errorf("fast of a model without fast: %v", err)
	}
}

func TestModelAccessError(t *testing.T) {
	yes := []error{
		&bridge.Error{Code: "permission_denied", Message: "This model is not available on your plan"},
		&bridge.Error{Code: "failed_precondition", Message: "Upgrade to Pro to use claude-opus-5"},
		errors.New("run failed: model claude-opus-5 is not enabled for your team"),
		// the real run error of a key without Opus in its plan (live, Nov 2026)
		errors.New("Claude Opus 5 is not included in your weekly usage Buy credits or switch to a Grok model to continue."),
	}
	no := []error{
		&bridge.Error{Code: "unauthenticated", Message: "Invalid Service Token"},
		&bridge.Error{Code: "resource_exhausted", Message: "Rate limit exceeded, try again"},
		&bridge.Error{Code: "unavailable", Message: "connection reset"},
		errors.New("boom"), nil,
	}
	for _, e := range yes {
		if !ModelAccessError(e) {
			t.Errorf("not an access error: %v", e)
		}
	}
	for _, e := range no {
		if ModelAccessError(e) {
			t.Errorf("wrongly an access error: %v", e)
		}
	}
}

func TestMarkUnavailableDropsBothVariants(t *testing.T) {
	r := &Runner{o: RunnerOptions{Keys: fixedTestKey("k1")}}
	if !r.MarkUnavailable("cursor/claude-opus-5-fast", errors.New("Claude Opus 5 is not included in your weekly usage Buy credits")) {
		t.Fatal("not marked")
	}
	if r.MarkUnavailable(AutoID, errors.New("model not available")) {
		t.Fatal("Auto must never be dropped")
	}
	menu := testCatalog(t).Menu(r.unavailable[keyID("k1")])
	for _, m := range menu {
		if strings.HasPrefix(m.ID, "cursor/claude-opus-5") {
			t.Fatalf("still listed: %v", menu)
		}
	}
	r.o.Keys = fixedTestKey("k2") // another key keeps its own list
	if len(testCatalog(t).Menu(r.unavailable[keyID("k2")])) != 6 {
		t.Fatal("unavailable leaked to another key")
	}
}

type fixedTestKey string

func (k fixedTestKey) Key() (string, string) { return string(k), "test" }
