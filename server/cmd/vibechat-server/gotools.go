package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/buildytech/vibechat-desktop/internal/gopin"
	"github.com/buildytech/vibechat-desktop/server/internal/godev"
	"github.com/buildytech/vibechat-desktop/server/internal/goinstall"
	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
)

// pinnedGo is the toolchain of this release (internal/gopin).
var pinnedGo = goinstall.Release{Version: gopin.Version, URL: gopin.URL, SHA256: gopin.SHA256, Size: gopin.Size}

// ensureGo installs the pinned Go into /opt/vibechat/go (root; idempotent).
func ensureGo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ensure-go", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", gopin.Root, "GOROOT to install into")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	msg, err := goinstall.Ensure(ctx, *root, pinnedGo, nil)
	if err != nil {
		fmt.Fprintln(stderr, "ensure-go:", err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

// goDev is the go-templ runner's dev command: it runs in the project folder inside the tools jail.
func goDev(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("go-dev", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", 0, "preview port on 127.0.0.1")
	if err := fs.Parse(args); err != nil || *port <= 0 || *port > 65535 {
		fmt.Fprintln(stderr, "usage: vibechat-server go-dev --port N")
		return 2
	}
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "go-dev:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := godev.Run(ctx, godev.Options{Dir: dir, Port: *port, Cache: isolate.GoCache(), Out: stdout}); err != nil {
		fmt.Fprintln(stderr, "go-dev:", err)
		return 1
	}
	return 0
}
