// Package secrets keeps the owner's Cursor API key in the OS keychain (macOS Keychain, Windows Credential Manager,
// Linux Secret Service) through github.com/zalando/go-keyring; CURSOR_API_KEY in the environment is the development
// and headless fallback. The key is never logged, never put in argv, URLs or files and never handed to the webview:
// the host passes it to the cursor-sdk-bridge process through its environment only.
package secrets

import (
	"errors"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
)

const (
	// Service and User name the keychain item.
	Service = "vibechat-desktop"
	User    = "CURSOR_API_KEY"
	// EnvName is the environment fallback (and the variable the bridge reads).
	EnvName = "CURSOR_API_KEY"
)

// Keys reads the key: the keychain first, then the environment.
type Keys struct {
	// Getenv is os.Getenv unless a test sets it.
	Getenv func(string) string
	// Keyring reads the OS keychain unless a test sets it.
	Keyring func(service, user string) (string, error)
	// Set and Delete write the OS keychain unless a test sets them.
	Set    func(service, user, value string) error
	Delete func(service, user string) error
}

// Key answers the key and its source ("keychain" or "env"), or "", "" when none is set.
func (k Keys) Key() (string, string) {
	get := k.Keyring
	if get == nil {
		get = keyring.Get
	}
	if v, err := get(Service, User); err == nil && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), "keychain"
	}
	env := k.Getenv
	if env == nil {
		env = os.Getenv
	}
	if v := strings.TrimSpace(env(EnvName)); v != "" {
		return v, "env"
	}
	return "", ""
}

// Store saves the key in the OS keychain.
func (k Keys) Store(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("empty key")
	}
	set := k.Set
	if set == nil {
		set = keyring.Set
	}
	return set(Service, User, key)
}

// Forget removes the keychain item (no error when there was none).
func (k Keys) Forget() error {
	del := k.Delete
	if del == nil {
		del = keyring.Delete
	}
	if err := del(Service, User); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

// Mask is the hint the UI may show: the last four characters only.
func Mask(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 4 {
		return "••••"
	}
	return "••••" + key[len(key)-4:]
}
