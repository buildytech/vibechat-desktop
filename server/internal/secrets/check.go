package secrets

import (
	"crypto/sha256"
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

// CheckFormat rejects what cannot be a key: empty, whitespace inside, non-ASCII or control characters. key must be
// trimmed already. Prefixes are not judged here (grok_… keys are Cursor keys); Cursor itself decides.
func CheckFormat(key string) error {
	if key == "" {
		return errors.New("the key is empty")
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

// knownPrefixes are the public key prefixes Describe may name (the type of a key, not a part of its secret).
var knownPrefixes = []string{"grok_", "key_", "crsr_", "xai-", "sk-"}

// Describe is the non-secret diagnostic of a key for logs and errors: length, its known public prefix (grok_, xai-…;
// nothing of an unknown one), a short SHA-256 fingerprint to tell two keys apart, whitespace, non-ASCII and where
// it came from. No character of the secret part.
func Describe(key, source string) string {
	raw := key
	key = strings.TrimSpace(key)
	prefix := "unknown prefix"
	for _, p := range knownPrefixes {
		if len(key) > len(p) && strings.EqualFold(key[:len(p)], p) {
			prefix = fmt.Sprintf("prefix %q", strings.ToLower(p))
			break
		}
	}
	sum := sha256.Sum256([]byte(key))
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
	if source == "" {
		source = "unknown"
	}
	return fmt.Sprintf("key: %d chars, %s, fingerprint sha256:%x, %s%s, from %s", len(key), prefix, sum[:4], ws, ascii, source)
}

// AuthHint explains a key Cursor rejected (unauthenticated / permission denied).
func AuthHint(key, source string) string {
	who := ""
	if f := Foreign(key); f != "" {
		who = "This is a " + f + " key, not a key of type Cursor. "
	}
	return "Cursor rejected the API key (" + Describe(key, source) + "). " + who + "Check it in Settings: " + WhereToGet + "."
}
