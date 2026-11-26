// fetch-bridge downloads the pinned cursor-sdk-bridge standalone build and the SDK platform helper (cursorsandbox,
// rg) of bridge.lock.json, verifies their digests and lays them out as the app expects:
//
//	<out>/manifest.json, <out>/bin/cursor-sdk-bridge[.exe], <out>/proto/...
//	<out>/bin/node_modules/@cursor/sdk-<platform>/{bin/cursorsandbox[.exe],bin/rg[.exe],vendor/...,package.json}
//
// On x64 the bridge is recompiled on the pinned Bun baseline runtime (no AVX2 needed, see baseline.go).
//
// Usage: go run ./cmd/fetch-bridge -out bin/bridge [-platform linux-x64] [-cache .bridge-cache]
//
//	go run ./cmd/fetch-bridge -verify <bridge dir>   (x64: the bridge runs on the baseline runtime)
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type lock struct {
	Bridge struct {
		Version string            `json:"version"`
		URL     string            `json:"url"`
		SHA256  map[string]string `json:"sha256"`
	} `json:"bridge"`
	Helper struct {
		Version   string            `json:"version"`
		URL       string            `json:"url"`
		Integrity map[string]string `json:"integrity"`
	} `json:"helper"`
	// Baseline is the Bun runtime the x64 bridges are recompiled on (see baseline.go).
	Baseline struct {
		Bun       string            `json:"bun"`
		URL       string            `json:"url"`
		Platforms []string          `json:"platforms"`
		SHA256    map[string]string `json:"sha256"`
	} `json:"baselineRuntime"`
}

func platform() string {
	os := runtime.GOOS
	if os == "windows" {
		os = "win32"
	}
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	return os + "-" + arch
}

func main() {
	out := flag.String("out", "bin/bridge", "destination folder (replaced)")
	plat := flag.String("platform", platform(), "bridge platform: linux-x64, darwin-arm64, win32-x64, ...")
	lockPath := flag.String("lock", "server/bridge.lock.json", "lock file (run from the repository root)")
	cache := flag.String("cache", "", "optional download cache folder")
	verify := flag.String("verify", "", "only check that the bridge folder runs on the pinned Bun baseline runtime (x64)")
	flag.Parse()
	raw, err := os.ReadFile(*lockPath)
	if err != nil {
		log.Fatal(err)
	}
	var l lock
	if err := json.Unmarshal(raw, &l); err != nil {
		log.Fatal(err)
	}
	if *verify != "" {
		if err := verifyDir(l, *verify, *cache); err != nil {
			log.Fatalf("fetch-bridge verify: %v", err)
		}
		return
	}
	sum, ok := l.Bridge.SHA256[*plat]
	if !ok {
		log.Fatalf("no bridge build for %s", *plat)
	}
	integrity, ok := l.Helper.Integrity[*plat]
	if !ok {
		log.Fatalf("no sandbox helper for %s: the agent cannot run on this platform", *plat)
	}
	bridgeTar := fetch(strings.ReplaceAll(l.Bridge.URL, "{platform}", *plat), *cache)
	if got := sha256.Sum256(bridgeTar); hex.EncodeToString(got[:]) != sum {
		log.Fatalf("bridge %s: sha256 %x, want %s", *plat, got, sum)
	}
	helperTgz := fetch(strings.ReplaceAll(l.Helper.URL, "{platform}", *plat), *cache)
	h := sha512.Sum512(helperTgz)
	if got := "sha512-" + base64.StdEncoding.EncodeToString(h[:]); got != integrity {
		log.Fatalf("helper %s: integrity %s, want %s", *plat, got, integrity)
	}
	if err := os.RemoveAll(*out); err != nil {
		log.Fatal(err)
	}
	if err := untar(bridgeTar, *out, ""); err != nil {
		log.Fatalf("bridge: %v", err)
	}
	helperDir := filepath.Join(*out, "bin", "node_modules", "@cursor", "sdk-"+*plat)
	if err := untar(helperTgz, helperDir, "package/"); err != nil {
		log.Fatalf("helper: %v", err)
	}
	if err := stampRelease(filepath.Join(*out, "manifest.json"), l.Bridge.Version); err != nil {
		log.Fatalf("bridge manifest: %v", err)
	}
	runtimeNote := "upstream runtime"
	if needsBaseline(l, *plat) {
		if err := rebuildBaseline(l, *plat, *out, *cache); err != nil {
			log.Fatalf("bridge baseline rebuild: %v", err)
		}
		runtimeNote = "rebuilt on Bun " + l.Baseline.Bun + " baseline (no AVX2)"
	}
	fmt.Printf("fetch-bridge: cursor-sdk-bridge %s (%s) + @cursor/sdk-%s %s -> %s\n", l.Bridge.Version, runtimeNote, *plat, l.Helper.Version, *out)
}

func fetch(url, cache string) []byte {
	if cache != "" {
		if b, err := os.ReadFile(filepath.Join(cache, filepath.Base(url))); err == nil {
			return b
		}
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		res, err := client.Get(url)
		if err == nil && res.StatusCode == http.StatusOK {
			b, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err == nil {
				if cache != "" {
					_ = os.MkdirAll(cache, 0o755)
					_ = os.WriteFile(filepath.Join(cache, filepath.Base(url)), b, 0o644)
				}
				return b
			}
			last = err
		} else if err == nil {
			res.Body.Close()
			last = fmt.Errorf("HTTP %d", res.StatusCode)
		} else {
			last = err
		}
		time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
	}
	log.Fatalf("download %s: %v", url, last)
	return nil
}

// untar extracts a .tar.gz into dst, keeping only entries under prefix (stripped). Paths may not leave dst.
func untar(data []byte, dst, prefix string) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		name = strings.TrimPrefix(name, prefix)
		if name == "" {
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		if rel, err := filepath.Rel(dst, target); err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("unsafe path %q", hdr.Name)
		}
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
		}
	}
}

// stampRelease adds "release": {"version": <lock's bridge version>} to the bridge manifest: the upstream manifest's
// bridgeVersion is not bumped with the releases, and doctor / the log report the pinned release from here.
func stampRelease(manifestPath, version string) error {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	m["release"] = map[string]any{"version": version}
	enc, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(manifestPath, append(enc, '\n'), 0o644)
}
