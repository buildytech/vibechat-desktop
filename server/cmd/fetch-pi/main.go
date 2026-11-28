// fetch-pi installs the pinned Pi coding agent (server/internal/codingagent/pi/pi.lock.json) for the RU contour: it
// downloads the npm tarball of @earendil-works/pi-coding-agent (a self-contained bundle: no npm install, no
// node_modules), checks its sha512 integrity against the lock, unpacks it and writes the marker the server's Locate
// requires. Pi needs Node >= the lock's minimum at run time; this tool does not install Node.
//
// Usage: go run ./cmd/fetch-pi -out <dir>            download, verify and install
//
//	go run ./cmd/fetch-pi -out <dir> -tarball f.tgz     install a tarball already on disk (offline; same verification)
//	go run ./cmd/fetch-pi -verify <dir> [-node path]    check an installed Pi and the Node as the server does
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent/pi"
)

const maxTarball = 200 << 20

func main() {
	out := flag.String("out", "", "folder to install Pi into (replaced)")
	tarball := flag.String("tarball", "", "use this tarball instead of downloading it")
	verify := flag.String("verify", "", "check an installed Pi folder and Node, install nothing")
	node := flag.String("node", "", "node binary for -verify (default: node on PATH)")
	flag.Parse()
	log.SetFlags(0)
	lock := pi.PinnedLock()
	switch {
	case *verify != "":
		in, err := pi.Locate(lock, *node, *verify, exec.LookPath)
		if err != nil {
			log.Fatalf("fetch-pi: %v", err)
		}
		fmt.Println("ok:", in.Describe(), in.Dir)
	case *out != "":
		var data []byte
		var err error
		if *tarball != "" {
			data, err = os.ReadFile(*tarball)
		} else {
			data, err = download(lock.Tarball)
		}
		if err != nil {
			log.Fatalf("fetch-pi: %v", err)
		}
		if err := pi.Extract(lock, data, *out); err != nil {
			log.Fatalf("fetch-pi: %v", err)
		}
		fmt.Printf("installed %s %s into %s\n", lock.Package, lock.Version, *out)
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func download(url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTarball+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxTarball {
		return nil, errors.New("tarball is too large")
	}
	return data, nil
}
