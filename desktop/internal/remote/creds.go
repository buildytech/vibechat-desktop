package remote

import (
	"errors"
	"sync"

	"github.com/zalando/go-keyring"

	"github.com/buildytech/vibechat-desktop/internal/appid"
)

// Keychain item names (service appid.KeychainService) of the server connection.
const (
	// CredSSHKey is the app's SSH private key (OpenSSH PEM, ed25519).
	CredSSHKey = "SSH_KEY"
	// CredHostToken is the server's host token (`vibechat-server token new`).
	CredHostToken = "SERVER_HOST_TOKEN"
)

// ErrNoCred: the item is not in the keychain.
var ErrNoCred = errors.New("not in the keychain")

// Creds is the OS keychain as the connection sees it. Values never go to files, logs, argv or the webview.
type Creds interface {
	// Get answers ErrNoCred when the item is missing.
	Get(name string) (string, error)
	Set(name, value string) error
	// Delete of a missing item is not an error.
	Delete(name string) error
}

// Keyring is the OS keychain (macOS Keychain, Windows Credential Manager, Linux Secret Service).
type Keyring struct{}

func (Keyring) Get(name string) (string, error) {
	v, err := keyring.Get(appid.KeychainService, name)
	if errors.Is(err, keyring.ErrNotFound) || (err == nil && v == "") {
		return "", ErrNoCred
	}
	return v, err
}

func (Keyring) Set(name, value string) error { return keyring.Set(appid.KeychainService, name, value) }

func (Keyring) Delete(name string) error {
	if err := keyring.Delete(appid.KeychainService, name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

// MemCreds is an in-memory keychain (tests).
type MemCreds struct {
	mu sync.Mutex
	m  map[string]string
	// Fail, when set, is returned by every call (a locked or missing keychain).
	Fail error
}

func (c *MemCreds) Get(name string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Fail != nil {
		return "", c.Fail
	}
	v, ok := c.m[name]
	if !ok || v == "" {
		return "", ErrNoCred
	}
	return v, nil
}

func (c *MemCreds) Set(name, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Fail != nil {
		return c.Fail
	}
	if c.m == nil {
		c.m = map[string]string{}
	}
	c.m[name] = value
	return nil
}

func (c *MemCreds) Delete(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Fail != nil {
		return c.Fail
	}
	delete(c.m, name)
	return nil
}
