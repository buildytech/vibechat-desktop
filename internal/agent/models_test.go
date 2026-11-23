package agent

import (
	"reflect"
	"testing"
)

func TestGrok47Selection(t *testing.T) {
	got := ToSelection("cursor/grok-4.7", []ModelParam{{"reasoning_effort", "xhigh"}, {"context", "500k"}, {"fast", "false"}})
	want := Selection{ID: "grok-4.7", Params: []ModelParam{{"context", "500k"}, {"reasoning_effort", "xhigh"}, {"fast", "false"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	// legacy effort id, bad values → defaults; never sends "effort"
	got = ToSelection("cursor/grok-4.7", []ModelParam{{"effort", "low"}, {"context", "9m"}})
	want = Selection{ID: "grok-4.7", Params: []ModelParam{{"context", "256k"}, {"reasoning_effort", "low"}, {"fast", "true"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
}

func TestOtherSelections(t *testing.T) {
	if s := ToSelection("cursor/auto", nil); s.ID != "auto" || s.Params != nil {
		t.Fatal(s)
	}
	if s := ToSelection("cursor/composer-2.5-fast", nil); s.ID != "composer-2.5" || s.Params[0].Value != "true" {
		t.Fatal(s)
	}
	if !Allowed("cursor/grok-4.7") || Allowed("cursor/grok-4.5") {
		t.Fatal("allowlist")
	}
}
