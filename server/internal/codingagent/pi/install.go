package pi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

//go:embed pi.lock.json
var lockJSON []byte

// Lock is the pinned Pi (pi.lock.json): one exact npm version and the integrity of its tarball. The package is
// self-contained (a bundled dist), so a verified extract needs no npm install and no node_modules.
type Lock struct {
	Package   string `json:"package"`
	Version   string `json:"version"`
	Tarball   string `json:"tarball"`
	Integrity string `json:"integrity"`
	Entry     string `json:"entry"`
	Node      string `json:"node"` // the minimum Node (Pi's engines field)
}

// PinnedLock is the lock this build was made with.
func PinnedLock() Lock {
	var l Lock
	if err := json.Unmarshal(lockJSON, &l); err != nil || l.Version == "" || l.Integrity == "" || l.Entry == "" {
		panic("pi.lock.json is broken")
	}
	return l
}

// MarkerFile is written next to the extracted package by fetch-pi after the tarball's integrity was checked; Locate
// needs it, so a package somebody unpacked by hand (or modified in place) is not run.
const MarkerFile = ".vibechat-pi.json"

type marker struct {
	Version   string `json:"version"`
	Integrity string `json:"integrity"`
}

// Integrity is the npm "sha512-<base64>" digest of data.
func Integrity(data []byte) string {
	sum := sha512.Sum512(data)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

// Extract verifies the npm tarball against the lock and unpacks its "package/" tree into dir (replacing what was
// there), then writes the marker. Entries are checked: only regular files and folders, no path that leaves dir.
func Extract(l Lock, tgz []byte, dir string) error {
	if got := Integrity(tgz); got != l.Integrity {
		return fmt.Errorf("pi tarball integrity %s does not match the pinned %s", got, l.Integrity)
	}
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return err
	}
	tmp := dir + ".part"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	tr := tar.NewReader(zr)
	files := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		rel, ok := strings.CutPrefix(name, "package/")
		if !ok || rel == "" || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
			if name == "package" {
				continue
			}
			return fmt.Errorf("pi tarball entry %q is outside package/", h.Name)
		}
		target := filepath.Join(tmp, filepath.FromSlash(rel))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			files++
		default:
			return fmt.Errorf("pi tarball entry %q is not a file or folder", h.Name)
		}
	}
	if files == 0 {
		return errors.New("pi tarball is empty")
	}
	if _, err := os.Stat(filepath.Join(tmp, filepath.FromSlash(l.Entry))); err != nil {
		return fmt.Errorf("pi tarball has no %s", l.Entry)
	}
	if err := checkPackage(l, tmp); err != nil {
		return err
	}
	m, _ := json.Marshal(marker{Version: l.Version, Integrity: l.Integrity})
	if err := os.WriteFile(filepath.Join(tmp, MarkerFile), append(m, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(tmp, dir)
}

func checkPackage(l Lock, dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return err
	}
	var p struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Name != l.Package || p.Version != l.Version {
		return fmt.Errorf("%s is %s %s, this build is pinned to %s %s", dir, p.Name, p.Version, l.Package, l.Version)
	}
	return nil
}

// Install is a located Node and a verified Pi package.
type Install struct {
	Node        string // absolute path of the node binary
	NodeVersion string
	Dir         string // the package folder (contains dist/bundle/cli.js)
	Version     string
	Entry       string // absolute path of the entry script
}

// Describe is a log line.
func (i Install) Describe() string {
	return fmt.Sprintf("pi %s (node %s)", i.Version, i.NodeVersion)
}

// LookPath is exec.LookPath (a parameter so tests do not need a Node).
type LookPath func(string) (string, error)

// Locate finds Node (nodeHint, else "node" on PATH) and the Pi package at dir, and refuses anything else: Node older
// than the lock's minimum, a package that is not the pinned version, one without the fetch-pi marker or with another
// integrity. Every failure is an error with what to do; there is no fallback to another Pi or another Node.
func Locate(l Lock, nodeHint, dir string, look LookPath) (Install, error) {
	var in Install
	if dir == "" {
		return in, errors.New("the Pi package folder is not set (contour config pi.dir or VIBECHAT_PI_DIR); install it with `go run ./cmd/fetch-pi`")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return in, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return in, fmt.Errorf("the Pi package folder %s does not exist; install it with `fetch-pi`", dir)
	}
	if err := checkPackage(l, abs); err != nil {
		return in, err
	}
	raw, err := os.ReadFile(filepath.Join(abs, MarkerFile))
	if err != nil {
		return in, fmt.Errorf("%s was not installed by fetch-pi (no %s); install the pinned package with `fetch-pi`", abs, MarkerFile)
	}
	var m marker
	if json.Unmarshal(raw, &m) != nil || m.Version != l.Version || m.Integrity != l.Integrity {
		return in, fmt.Errorf("%s is not the pinned Pi %s (marker %s)", abs, l.Version, strings.TrimSpace(string(raw)))
	}
	entry := filepath.Join(abs, filepath.FromSlash(l.Entry))
	if _, err := os.Stat(entry); err != nil {
		return in, fmt.Errorf("Pi %s has no %s", l.Version, l.Entry)
	}
	node := nodeHint
	if node == "" {
		if look == nil {
			return in, errors.New("node is not available")
		}
		if node, err = look("node"); err != nil {
			return in, fmt.Errorf("node %s or newer is not on PATH (set pi.node in the contour config): %v", l.Node, err)
		}
	}
	if node, err = filepath.Abs(node); err != nil {
		return in, err
	}
	ver, err := NodeVersion(node)
	if err != nil {
		return in, err
	}
	if !AtLeast(ver, l.Node) {
		return in, fmt.Errorf("%s is Node %s; Pi %s needs Node %s or newer", node, ver, l.Version, l.Node)
	}
	return Install{Node: node, NodeVersion: ver, Dir: abs, Version: l.Version, Entry: entry}, nil
}

var semver = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// NodeVersion asks the binary (`node --version`, clean environment, short timeout). Something that is not Node (Bun
// answers "1.x") fails the minimum check later; something that does not answer is an error here.
func NodeVersion(node string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := procgroup.CommandContext(ctx, node, "--version")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %v", node, err)
	}
	v := strings.TrimSpace(string(out))
	if !semver.MatchString(v) {
		return "", fmt.Errorf("%s --version answered %q, not a version", node, v)
	}
	return strings.TrimPrefix(v, "v"), nil
}

// AtLeast: version >= min (both "major.minor.patch").
func AtLeast(version, min string) bool {
	a, b := semver.FindStringSubmatch(version), semver.FindStringSubmatch(min)
	if a == nil || b == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(a[i])
		y, _ := strconv.Atoi(b[i])
		if x != y {
			return x > y
		}
	}
	return true
}
