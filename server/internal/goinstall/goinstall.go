// Package goinstall installs the pinned Go toolchain (internal/gopin) on a VibeChat server:
// `vibechat-server ensure-go` (root) from the setup wizard and after every update. The archive is checked by size
// and sha256 before it is unpacked; an installed toolchain of the pinned version is left as is.
package goinstall

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

// Release is one pinned toolchain archive.
type Release struct {
	Version string // "1.27.4"
	URL     string
	SHA256  string
	Size    int64
}

// Installed answers the version of root/bin/go ("" when there is none).
func Installed(root string) string {
	cmd := procgroup.Command(filepath.Join(root, "bin", "go"), "env", "GOVERSION")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOENV=off", "GOROOT="+root)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "go")
}

// Ensure makes root the toolchain of rel: nothing when it already is; else download, check, unpack beside root
// and swap it in. It reports what it did.
func Ensure(ctx context.Context, root string, rel Release, client *http.Client) (string, error) {
	if v := Installed(root); v == rel.Version {
		return "Go " + v + " is installed at " + root, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(parent, ".go-download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", rel.URL, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, rel.Size+1))
	if err != nil {
		return "", err
	}
	if n != rel.Size {
		return "", fmt.Errorf("download %s: %d bytes, want %d", rel.URL, n, rel.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != rel.SHA256 {
		return "", fmt.Errorf("download %s: sha256 %s, want %s", rel.URL, got, rel.SHA256)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(parent, ".go-new-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err := untar(tmp, stage); err != nil {
		return "", err
	}
	fresh := filepath.Join(stage, "go")
	if v := Installed(fresh); v != rel.Version {
		return "", fmt.Errorf("the unpacked toolchain reports %q, want %s", v, rel.Version)
	}
	old := root + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Stat(root); err == nil {
		if err := os.Rename(root, old); err != nil {
			return "", err
		}
	}
	if err := os.Rename(fresh, root); err != nil {
		_ = os.Rename(old, root)
		return "", err
	}
	_ = os.RemoveAll(old)
	return "Installed Go " + rel.Version + " at " + root, nil
}

// untar unpacks a gzip tar into dst: regular files and folders only, every path inside dst.
func untar(r io.Reader, dst string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive path %q leaves the folder", hdr.Name)
		}
		target := filepath.Join(dst, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if hdr.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry %q is not a file or folder", hdr.Name)
		}
	}
}
