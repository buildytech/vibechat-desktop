package templates

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// A layer template (templates.json runner go-templ, templates/<id>/layer.json; buildytech/templates ADR 0004) is a
// pinned upstream tree over the empty workspace: the GitHub archive of one commit, accepted only when every file
// matches its SHA-256 in layer.json (no file more, none less). The checked tree is cached under CacheDir, so a host
// downloads each pinned commit once. Today only the Go starter is a layer; the target is every template as base +
// layers (framework core, data, design), see vibechat-desktop ADR 0005.

// Layer is templates/<id>/layer.json (schemaVersion 1).
type Layer struct {
	SchemaVersion int    `json:"schemaVersion"`
	Base          string `json:"base"`
	Source        struct {
		Kind    string   `json:"kind"`
		Repo    string   `json:"repo"`
		Commit  string   `json:"commit"`
		Exclude []string `json:"exclude"`
	} `json:"source"`
	Go          string            `json:"go"`
	Files       map[string]string `json:"files"`
	Executables []string          `json:"executables"`
}

var (
	repoRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hashRe   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ParseLayer reads and checks layer.json.
func ParseLayer(raw []byte) (*Layer, error) {
	var l Layer
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("layer.json: %w", err)
	}
	switch {
	case l.SchemaVersion != 1:
		return nil, fmt.Errorf("layer.json: schemaVersion %d (this build reads 1)", l.SchemaVersion)
	case l.Base != "empty":
		return nil, fmt.Errorf("layer.json: base %q (only empty)", l.Base)
	case l.Source.Kind != "github-archive" || !repoRe.MatchString(l.Source.Repo) || !commitRe.MatchString(l.Source.Commit):
		return nil, errors.New("layer.json: source must be a github-archive of owner/repo at a full commit")
	case len(l.Files) == 0:
		return nil, errors.New("layer.json: no files")
	}
	for p, h := range l.Files {
		if !layerPathOK(p) || !hashRe.MatchString(h) {
			return nil, fmt.Errorf("layer.json: bad file entry %q", p)
		}
	}
	for _, p := range l.Executables {
		if _, ok := l.Files[p]; !ok {
			return nil, fmt.Errorf("layer.json: executable %q is not a file", p)
		}
	}
	return &l, nil
}

func layerPathOK(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.Contains(p, "\x00") || path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." || part == ".git" {
			return false
		}
	}
	return true
}

// Digest identifies the layer's content (the cache folder name).
func (l *Layer) Digest() string {
	keys := make([]string, 0, len(l.Files))
	for k := range l.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	fmt.Fprintf(h, "%s@%s\n", l.Source.Repo, l.Source.Commit)
	for _, k := range keys {
		fmt.Fprintf(h, "%s %s\n", l.Files[k], k)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Where the layer cache lives and where archives come from (tests point both elsewhere).
var (
	cacheMu     sync.Mutex
	cacheDir    = filepath.Join(os.TempDir(), "vibechat-templates")
	ArchiveBase = "https://codeload.github.com"
	httpClient  = &http.Client{Timeout: 5 * time.Minute}
)

// SetCacheDir sets the folder of the checked layer trees (the server: <data>/cache/templates).
func SetCacheDir(dir string) {
	cacheMu.Lock()
	cacheDir = dir
	cacheMu.Unlock()
}

const maxArchive = 96 << 20

// layerFS is the checked tree of a layer, fetched on first use.
type layerFS struct {
	l    *Layer
	once sync.Once
	fs   fs.FS
	err  error
}

// LayerFS answers the files of l (downloaded and checked on the first Open).
func LayerFS(l *Layer) fs.FS { return &layerFS{l: l} }

func (f *layerFS) Open(name string) (fs.File, error) {
	f.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		var dir string
		dir, f.err = EnsureLayer(ctx, f.l)
		if f.err == nil {
			f.fs = os.DirFS(dir)
		}
	})
	if f.err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: f.err}
	}
	return f.fs.Open(name)
}

// EnsureLayer answers the folder of l's checked tree, downloading and checking it when it is not cached.
func EnsureLayer(ctx context.Context, l *Layer) (string, error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	dir := filepath.Join(cacheDir, strings.ReplaceAll(l.Source.Repo, "/", "-")+"-"+l.Source.Commit[:12]+"-"+l.Digest())
	if _, err := os.Stat(filepath.Join(dir, ".vibechat-layer-ok")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(cacheDir, ".fetch-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := fetchLayer(ctx, l, tmp); err != nil {
		return "", fmt.Errorf("template %s@%s: %w", l.Source.Repo, l.Source.Commit[:7], err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".vibechat-layer-ok"), []byte(l.Digest()+"\n"), 0o600); err != nil {
		return "", err
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	return dir, nil
}

func fetchLayer(ctx context.Context, l *Layer, dst string) error {
	url := strings.TrimRight(ArchiveBase, "/") + "/" + l.Source.Repo + "/tar.gz/" + l.Source.Commit
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, res.StatusCode)
	}
	zr, err := gzip.NewReader(io.LimitReader(res.Body, maxArchive))
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	exec := map[string]bool{}
	for _, p := range l.Executables {
		exec[p] = true
	}
	seen := map[string]bool{}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
				if _, listed := l.Files[stripFirst(hdr.Name)]; listed {
					return fmt.Errorf("%s is a link in the archive", stripFirst(hdr.Name))
				}
			}
			continue
		}
		rel := stripFirst(hdr.Name)
		want, listed := l.Files[rel]
		if !listed {
			if excludedBy(l.Source.Exclude, rel) {
				continue
			}
			return fmt.Errorf("the archive has %s, which layer.json does not list", rel)
		}
		if hdr.Size > 32<<20 {
			return fmt.Errorf("%s is too large", rel)
		}
		data, err := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != want {
			return fmt.Errorf("%s does not match its SHA-256 in layer.json", rel)
		}
		mode := os.FileMode(0o644)
		if exec[rel] {
			mode = 0o755
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return err
		}
		seen[rel] = true
	}
	for p := range l.Files {
		if !seen[p] {
			return fmt.Errorf("the archive has no %s", p)
		}
	}
	return nil
}

// stripFirst drops the archive's top folder (<repo>-<commit>/).
func stripFirst(name string) string {
	name = strings.TrimPrefix(name, "./")
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return ""
}

func excludedBy(ex []string, p string) bool {
	for _, e := range ex {
		if strings.HasSuffix(e, "/") && strings.HasPrefix(p, e) || p == e {
			return true
		}
	}
	return false
}
