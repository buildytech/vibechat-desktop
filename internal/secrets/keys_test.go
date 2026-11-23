package secrets

import (
	"errors"
	"strings"
	"testing"
)

func TestKeychainFirstThenEnv(t *testing.T) {
	env := func(v string) func(string) string {
		return func(name string) string {
			if name == EnvName {
				return v
			}
			return ""
		}
	}
	none := func(string, string) (string, error) { return "", errors.New("not found") }
	stored := func(string, string) (string, error) { return " from-keychain ", nil }
	cases := []struct {
		keys        Keys
		key, source string
	}{
		{Keys{Keyring: stored, Getenv: env("from-env")}, "from-keychain", "keychain"},
		{Keys{Keyring: none, Getenv: env(" from-env ")}, "from-env", "env"},
		{Keys{Keyring: none, Getenv: env("  ")}, "", ""},
	}
	for i, c := range cases {
		key, source := c.keys.Key()
		if key != c.key || source != c.source {
			t.Errorf("case %d: got source %q (key matches: %v)", i, source, key == c.key)
		}
	}
}

func TestStoreTrimsAndRefusesEmpty(t *testing.T) {
	var got string
	k := Keys{Set: func(_, _, v string) error { got = v; return nil }}
	if err := k.Store("  "); err == nil {
		t.Fatal("empty key stored")
	}
	if err := k.Store(" abc \n"); err != nil || got != "abc" {
		t.Fatalf("stored %q, err %v", got, err)
	}
}

func TestMaskShowsOnlyTheTail(t *testing.T) {
	m := Mask("cursor_secretvalue1234")
	if !strings.HasSuffix(m, "1234") || strings.Contains(m, "secret") {
		t.Fatalf("mask %q", m)
	}
}
