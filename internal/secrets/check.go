package secrets

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// WhereToGet is where a working key comes from: the API keys page lists keys of type "Cursor" (grok_…), which the
// Cursor SDK accepts, and "Grok API" keys (xai-…), which it does not.
const WhereToGet = "create or copy an API key of type \"Cursor\" (grok_…) at https://grok.com/dashboard/api-keys; " +
	"\"Grok API\" keys (xai-…) do not work for the agent"

// Foreign names the other key type this looks like, or "".
func Foreign(key string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "xai-") {
		return "Grok API (xai-…)"
	}
	return ""
}

// Duplicated reports a key that is one string repeated twice (pasted into a field that already held it): even
// length, equal halves.
func Duplicated(key string) bool {
	k := strings.TrimSpace(key)
	if len(k) < 16 || len(k)%2 != 0 {
		return false
	}
	return k[:len(k)/2] == k[len(k)/2:]
}

// ErrPastedTwice is the save error of a duplicated key.
var ErrPastedTwice = errors.New("key appears to be pasted twice (Ключ вставлен дважды): clear the field and paste it once")

// CheckFormat rejects what cannot be a key: empty, whitespace inside, non-ASCII or control characters. key must be
// trimmed already. Prefixes are not judged here (grok_… keys are Cursor keys); Cursor itself decides.
func CheckFormat(key string) error {
	if key == "" {
		return errors.New("the key is empty")
	}
	if Duplicated(key) {
		return ErrPastedTwice
	}
	for _, r := range key {
		switch {
		case unicode.IsSpace(r):
			return errors.New("the key has spaces or line breaks inside: copy it again")
		case r > unicode.MaxASCII || unicode.IsControl(r):
			return errors.New("the key has non-ASCII or invisible characters: copy it again from grok.com/dashboard/api-keys")
		}
	}
	return nil
}

// Describe is the non-secret diagnostic of a key for logs and errors: length, the first 4 characters, whitespace,
// non-ASCII and where it came from. Never more of the key.
func Describe(key, source string) string {
	raw := key
	key = strings.TrimSpace(key)
	prefix := key
	if len(prefix) > 4 {
		prefix = prefix[:4]
	}
	ws := "no whitespace"
	if raw != key {
		ws = "whitespace around (trimmed)"
	}
	if strings.IndexFunc(key, unicode.IsSpace) >= 0 {
		ws = "WHITESPACE INSIDE"
	}
	ascii := ""
	if strings.IndexFunc(key, func(r rune) bool { return r > unicode.MaxASCII || unicode.IsControl(r) }) >= 0 {
		ascii = ", NON-ASCII/CONTROL CHARACTERS"
	}
	if Duplicated(key) {
		ascii += fmt.Sprintf(", LOOKS DUPLICATED (pasted twice: the same %d characters twice)", len(key)/2)
	}
	if source == "" {
		source = "unknown"
	}
	return fmt.Sprintf("key: %d chars, starts with %q, ends with %q, %s%s, from %s", len(key), prefix, strings.TrimPrefix(Mask(key), "••••"), ws, ascii, source)
}

// AuthHint explains a key Cursor rejected (unauthenticated / permission denied).
func AuthHint(key, source string) string {
	who := ""
	if Duplicated(key) {
		who = "The key looks duplicated: it was pasted twice (Ключ вставлен дважды). Paste it once in Settings. "
	}
	if f := Foreign(key); f != "" {
		who = "This is a " + f + " key, not a key of type Cursor. "
	}
	return "Cursor rejected the API key (" + Describe(key, source) + "). " + who + "Check it in Settings: " + WhereToGet + "."
}
