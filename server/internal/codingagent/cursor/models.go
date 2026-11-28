// Package cursor is the Cursor SDK runtime of the coding-agent seam (internal/codingagent): it drives Cursor agents through the cursor-sdk-bridge for the host seam: one bridge process per app
// (started on first use with the key from the keychain), one Cursor agent per Studio session (created or resumed with
// the SDK sandbox on), and the translation of the bridge's run stream into Agent Contract v1 events.
package cursor

import (
	"fmt"
	"strings"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
)

// ModelParam is a Cursor model parameter value.
type ModelParam = codingagent.ModelParam

// Selection is the bridge's ModelSelection.
type Selection struct {
	ID     string       `json:"id"`
	Params []ModelParam `json:"params,omitempty"`
}

// Model is AgentModel of the contract.
type Model = codingagent.Model

// CatalogModel is one item of the key's model list (SdkCursorService.ListModels, the SDK's /v1/models).
type CatalogModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Parameters  []struct {
		ID     string `json:"id"`
		Values []struct {
			Value string `json:"value"`
		} `json:"values"`
	} `json:"parameters"`
	Variants []struct {
		Params    []ModelParam `json:"params"`
		IsDefault bool         `json:"isDefault"`
	} `json:"variants"`
}

// Catalog is the model list of the current key, in Cursor's order. Nothing is hard-coded: the menu and the
// selections are built from it.
type Catalog []CatalogModel

// AutoID is the Studio id of Cursor's "default" (Auto) model.
const AutoID = "cursor/auto"

// ErrUnknownModel: the model is not in the key's list (or was marked unavailable for it).
var ErrUnknownModel error = unknownModel{}

type unknownModel struct{}

func (unknownModel) Error() string        { return "model not available for this Cursor key" }
func (unknownModel) Is(target error) bool { return target == codingagent.ErrUnknownModel }

func (m CatalogModel) hasParam(id, value string) bool {
	for _, p := range m.Parameters {
		if p.ID == id {
			for _, v := range p.Values {
				if v.Value == value {
					return true
				}
			}
		}
	}
	return false
}

// hasFast: the model offers a Fast variant, shown as its own menu entry "<id>-fast".
func (m CatalogModel) hasFast() bool {
	return m.hasParam("fast", "true") && m.hasParam("fast", "false")
}

func (m CatalogModel) label() string {
	if s := strings.TrimSpace(m.DisplayName); s != "" {
		return s
	}
	return m.ID
}

// Menu is the Studio model list: Auto, then every model of the key, a Fast entry next to models that have one.
// unavailable holds menu ids that failed for this key with an access or plan error.
func (c Catalog) Menu(unavailable map[string]bool) []Model {
	out := []Model{}
	add := func(id, label string) {
		if !unavailable[id] {
			out = append(out, Model{ID: id, Provider: "cursor", Label: label})
		}
	}
	for _, m := range c {
		if m.ID == "default" {
			add(AutoID, m.label())
			continue
		}
		add("cursor/"+m.ID, m.label())
		if m.hasFast() {
			add("cursor/"+m.ID+"-fast", m.label()+" Fast")
		}
	}
	return out
}

// Select maps a menu id (and optional Studio params) to the bridge selection: the model's default variant, Studio
// params that the model accepts on top, and fast=true/false for the Fast/plain entries.
func (c Catalog) Select(modelID string, params []ModelParam) (Selection, error) {
	bare := strings.TrimPrefix(modelID, "cursor/")
	if bare == "auto" || bare == "default" {
		for _, m := range c {
			if m.ID == "default" {
				return Selection{ID: "default"}, nil
			}
		}
		return Selection{}, fmt.Errorf("%w: Auto", ErrUnknownModel)
	}
	find := func(id string) *CatalogModel {
		for i := range c {
			if c[i].ID == id {
				return &c[i]
			}
		}
		return nil
	}
	m, fast := find(bare), ""
	if m != nil && m.hasFast() {
		fast = "false"
	}
	if m == nil && strings.HasSuffix(bare, "-fast") {
		if b := find(strings.TrimSuffix(bare, "-fast")); b != nil && b.hasFast() {
			m, fast = b, "true"
		}
	}
	if m == nil {
		return Selection{}, fmt.Errorf("%w: %s", ErrUnknownModel, modelID)
	}
	var sel []ModelParam
	for _, v := range m.Variants {
		if v.IsDefault {
			sel = append(sel, v.Params...)
			break
		}
	}
	put := func(id, value string) {
		for i := range sel {
			if sel[i].ID == id {
				sel[i].Value = value
				return
			}
		}
		sel = append(sel, ModelParam{ID: id, Value: value})
	}
	for _, p := range params {
		if p.ID != "fast" && m.hasParam(p.ID, p.Value) {
			put(p.ID, p.Value)
		}
	}
	if fast != "" {
		put("fast", fast)
	}
	return Selection{ID: m.ID, Params: sel}, nil
}

// ModelAccessError reports a run or create failure that means "this key may not use this model" (plan, access,
// model not enabled), as opposed to a bad key, a rate limit or a network error.
func ModelAccessError(err error) bool {
	if err == nil || IsUnauthenticated(err) {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, transient := range []string{"rate limit", "rate-limit", "too many requests", "timeout", "unavailable: ", "network", "connection"} {
		if strings.Contains(s, transient) {
			return false
		}
	}
	coded := strings.Contains(s, "permission_denied") || strings.Contains(s, "failed_precondition") ||
		strings.Contains(s, "resource_exhausted") || strings.Contains(s, "invalid_argument") || strings.Contains(s, "not_found")
	if plan := mentionsModelAccess(strings.ReplaceAll(s, "model", "")); plan {
		return true // plan / access wording, with or without a code
	}
	return coded && strings.Contains(s, "model")
}

func mentionsModelAccess(msg string) bool {
	s := strings.ToLower(msg)
	for _, w := range []string{"model", "plan", "upgrade", "subscription", "not available", "not enabled", "not allowed",
		"not included", "buy credits", "weekly usage", "usage limit"} {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}
