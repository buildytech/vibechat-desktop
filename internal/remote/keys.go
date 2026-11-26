package remote

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"
)

// keyComment ends the app's public key line in authorized_keys.
const keyComment = "vibechat-desktop"

// ErrHostKeyChanged: the server presented an SSH host key other than the pinned one.
var ErrHostKeyChanged = errors.New("the server's SSH host key is not the pinned one")

// ensureSigner answers the app's SSH key from the keychain, generating and storing an ed25519 key the first time.
func ensureSigner(c Creds) (ssh.Signer, error) {
	raw, err := c.Get(CredSSHKey)
	switch {
	case err == nil:
		s, perr := ssh.ParsePrivateKey([]byte(raw))
		if perr == nil {
			return s, nil
		}
		// An unreadable item is replaced: the public line changes and has to be installed again.
	case !errors.Is(err, ErrNoCred):
		return nil, fmt.Errorf("system keychain: %w", err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, keyComment)
	if err != nil {
		return nil, err
	}
	if err := c.Set(CredSSHKey, string(pem.EncodeToMemory(block))); err != nil {
		return nil, fmt.Errorf("system keychain: %w", err)
	}
	return ssh.NewSignerFromKey(priv)
}

// authorizedLine is the public key as one authorized_keys line.
func authorizedLine(pub ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + keyComment
}

// pinCallback checks the server's host key. With no pin (the first connect to this server) it accepts the key and
// reports it through seen (trust on first use); with a pin only that exact key passes.
func pinCallback(pinned ssh.PublicKey, seen *ssh.PublicKey) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if pinned == nil {
			*seen = key
			return nil
		}
		if key.Type() == pinned.Type() && bytes.Equal(key.Marshal(), pinned.Marshal()) {
			*seen = key
			return nil
		}
		return fmt.Errorf("%w: pinned %s, presented %s", ErrHostKeyChanged, ssh.FingerprintSHA256(pinned), ssh.FingerprintSHA256(key))
	}
}

// parsePin reads a pinned key saved by marshalPin ("" = none).
func parsePin(s string) (ssh.PublicKey, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s))
	return k, err
}

func marshalPin(k ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

// fingerprint is "SHA256:…" of a saved pin, "" without one.
func fingerprint(pin string) string {
	k, err := parsePin(pin)
	if err != nil || k == nil {
		return ""
	}
	return ssh.FingerprintSHA256(k)
}
