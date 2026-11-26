package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/buildytech/vibechat-desktop/internal/appid"
	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

const (
	releaseRepo  = "buildytech/vibechat-desktop"
	sumsAsset    = "SHA256SUMS"
	maxDebBytes  = 512 << 20
	maxSumsBytes = 1 << 20
)

// debAsset is the release asset of this architecture (stable name, see scripts/package-server.sh).
func debAsset() string { return "vibechat-server-linux-" + runtime.GOARCH + ".deb" }

// updater installs the newest official release: the GitHub "latest" release tag, its SHA256SUMS and the .deb,
// checked against the sum before dpkg sees it. The sum comes from the same release, so it proves integrity (a broken
// or swapped download), not origin; release signing comes later.
type updater struct {
	api      string // https://api.github.com
	download string // https://github.com
	repo     string
	client   *http.Client
	current  string
	install  func(deb string) error
	out      io.Writer
}

func (u *updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("user-agent", "vibechat-server/"+u.current)
	req.Header.Set("accept", "application/vnd.github+json")
	res, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, limit)
	}
	return b, nil
}

func (u *updater) latestTag(ctx context.Context) (string, error) {
	b, err := u.get(ctx, u.api+"/repos/"+u.repo+"/releases/latest", 1<<20)
	if err != nil {
		return "", err
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(b, &rel); err != nil || rel.Tag == "" {
		return "", fmt.Errorf("latest release: no tag (%v)", err)
	}
	return rel.Tag, nil
}

// parseVersion reads "v1.2.3" / "1.2.3"; ok is false for anything else ("dev", pre-releases).
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// newer reports whether a is a later version than b.
func newer(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// sumFor finds name in a SHA256SUMS file ("<hex>  <name>" or "<hex> *<name>").
func sumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			if _, err := hex.DecodeString(f[0]); err != nil || len(f[0]) != 64 {
				return "", fmt.Errorf("%s: bad sum for %s", sumsAsset, name)
			}
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s has no %s", sumsAsset, name)
}

// errUpToDate: nothing to install.
var errUpToDate = errors.New("up to date")

// run checks and, unless checkOnly, installs. It answers errUpToDate when the server is current.
func (u *updater) run(ctx context.Context, checkOnly bool) error {
	cur, ok := parseVersion(u.current)
	if !ok {
		return fmt.Errorf("this build is %q, not a release: no automatic updates", u.current)
	}
	tag, err := u.latestTag(ctx)
	if err != nil {
		return err
	}
	latest, ok := parseVersion(tag)
	if !ok {
		return fmt.Errorf("latest release tag %q is not a version", tag)
	}
	if !newer(latest, cur) {
		fmt.Fprintf(u.out, "vibechat-server %s is up to date (latest %s)\n", u.current, tag)
		return errUpToDate
	}
	fmt.Fprintf(u.out, "vibechat-server %s → %s\n", u.current, tag)
	if checkOnly {
		return nil
	}
	base := u.download + "/" + u.repo + "/releases/download/" + tag + "/"
	sums, err := u.get(ctx, base+sumsAsset, maxSumsBytes)
	if err != nil {
		return err
	}
	want, err := sumFor(sums, debAsset())
	if err != nil {
		return err
	}
	deb, err := u.get(ctx, base+debAsset(), maxDebBytes)
	if err != nil {
		return err
	}
	got := sha256.Sum256(deb)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s: sha256 mismatch, not installed", debAsset())
	}
	dir, err := os.MkdirTemp("", "vibechat-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, debAsset())
	if err := os.WriteFile(file, deb, 0o600); err != nil {
		return err
	}
	if err := u.install(file); err != nil {
		return fmt.Errorf("install %s: %w", tag, err)
	}
	fmt.Fprintf(u.out, "installed vibechat-server %s\n", tag)
	return nil
}

func dpkgInstall(deb string) error {
	if os.Geteuid() != 0 {
		return errors.New("installing needs root (vibechat-update.service runs as root)")
	}
	cmd := procgroup.Command("dpkg", "-i", deb)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func update(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	checkOnly := fs.Bool("check", false, "only report whether a newer release exists")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	u := &updater{
		api: "https://api.github.com", download: "https://github.com", repo: releaseRepo,
		client: &http.Client{Timeout: 10 * time.Minute}, current: appid.Version, install: dpkgInstall, out: stdout,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	switch err := u.run(ctx, *checkOnly); {
	case err == nil, errors.Is(err, errUpToDate):
		return 0
	default:
		fmt.Fprintln(stderr, "update:", err)
		return 1
	}
}
