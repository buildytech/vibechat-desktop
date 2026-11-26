package secrets

import (
	"errors"
	"os"
	"strings"

	"github.com/buildytech/vibechat-desktop/internal/privfile"
)

// File keeps the Cursor key in one owner-only file (0600 in a 0700 folder): the server's store (vibechat-server runs
// as its own system user with no desktop keychain). It has no environment fallback: the key the server uses is the
// one set through Settings or `vibechat-server --set-key`, nothing else. A file readable by others is refused, not
// used.
type File struct {
	Path string
}

// Key answers the key and "file", or "", "" when there is none or the file is not private (logged by the caller
// through Check).
func (f File) Key() (string, string) {
	raw, err := privfile.Read(f.Path)
	if err != nil {
		return "", ""
	}
	if v := strings.TrimSpace(string(raw)); v != "" {
		return v, "file"
	}
	return "", ""
}

// Check reports why Key answers nothing: nil when the key is there or simply not set yet.
func (f File) Check() error {
	_, err := privfile.Read(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Store saves the key (atomic, 0600).
func (f File) Store(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("empty key")
	}
	return privfile.Write(f.Path, []byte(key+"\n"))
}

// Forget removes the key file (no error when there was none).
func (f File) Forget() error {
	if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
