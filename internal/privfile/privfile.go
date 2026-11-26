// Package privfile writes and reads owner-only files (the server's Cursor key, the host token hash, the instance id):
// the folder is 0700, the file 0600, a write goes through a temp file and rename so a crash never leaves half a file,
// and a read refuses a file others can read or write.
package privfile

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Write stores data at path (dir 0700, file 0600, atomic).
func Write(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Read returns the file at path; it fails with os.ErrNotExist when there is none and refuses a file whose mode lets
// the group or others in (Windows has no such bits and is not checked).
func Read(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is %v: it must be 0600 (chmod 600 %s)", path, st.Mode().Perm(), path)
	}
	return os.ReadFile(path)
}
