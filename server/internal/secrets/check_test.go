package secrets

import (
	"strings"
	"testing"
)

func TestCheckFormat(t *testing.T) {
	good := "grok_" + strings.Repeat("a1", 48) + "2a6" // the shape of a Cursor-type key
	if err := CheckFormat(good); err != nil {
		t.Fatalf("good key rejected: %v", err)
	}
	for _, bad := range []string{"", good[:50] + " " + good[50:], good[:50] + "\n" + good[50:], good[:50] + "\u200b" + good[50:], "ке" + good, "a\x00b"} {
		if err := CheckFormat(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestForeign(t *testing.T) {
	for k, want := range map[string]string{"xai-abc": "Grok API (xai-…)", "XAI-abc": "Grok API (xai-…)", "grok_abc": "", "key_abc": "", "sk-x": ""} {
		if got := Foreign(k); got != want {
			t.Errorf("Foreign(%s) = %q, want %q", k, got, want)
		}
	}
}

// The diagnostic names a known public prefix and a fingerprint, never a character of the secret part.
func TestDescribeIsNotSecret(t *testing.T) {
	key := "grok_" + "SECRETMIDDLEPART" + strings.Repeat("x", 77) + "2a61"
	d := Describe(" "+key+"\n", "keychain")
	for _, want := range []string{"102 chars", `prefix "grok_"`, "fingerprint sha256:", "whitespace around", "keychain"} {
		if !strings.Contains(d, want) {
			t.Errorf("%q lacks %q", d, want)
		}
	}
	if strings.Contains(d, "2a61") || strings.Contains(Describe("abcdSECRET", ""), "abcd") {
		t.Fatalf("a key fragment leaked: %q", d)
	}
	if strings.Contains(d, "SECRET") || strings.Contains(AuthHint(key, "env"), "SECRET") {
		t.Fatal("key leaked into the diagnostic")
	}
	if h := AuthHint("xai-"+strings.Repeat("z", 40), "keychain"); !strings.Contains(h, "Grok API (xai-…) key, not a key of type Cursor") || !strings.Contains(h, "grok.com/dashboard/api-keys") {
		t.Errorf("hint: %s", h)
	}
}
