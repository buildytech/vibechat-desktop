// vibechat-server: the VibeChat host on the user's own server (Debian 13, one Incus container per user). Plain
// net/http, no Wails, no window: the host seam (/runtime/v1/*) and the Studio on 127.0.0.1:18120, the preview gateway
// (<projectId>.preview.localhost) on 127.0.0.1:18121, both reached only through the desktop app's SSH tunnel. Every
// request needs the host token (Bearer, from the app's Go side) or the window session it bought; the Cursor key is a
// 0600 file on the server.
//
//	vibechat-server [serve]      run the host (systemd: vibechat-server.service, user vibechat)
//	vibechat-server token new    make a new host token: print it once, keep only its sha256 (the old one stops working)
//	vibechat-server set-key      read the Cursor API key from stdin into the server's key file
//	vibechat-server forget-key   remove it
//	vibechat-server doctor       check this machine: bridge + SDK sandbox preflight, Landlock, user namespaces, tools
//	vibechat-server update       install the newest official release if it is newer (vibechat-update.timer, root)
//	vibechat-server --check-key  ask Cursor whether the stored key works (no key printed)
//	vibechat-server --version
//	vibechat-server --hook <ev>  the agent's permission hook (stdin JSON → stdout JSON)
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildytech/vibechat-desktop/internal/appid"
	"github.com/buildytech/vibechat-desktop/internal/auth"
	"github.com/buildytech/vibechat-desktop/internal/hostwire"
	"github.com/buildytech/vibechat-desktop/internal/sandbox"
	"github.com/buildytech/vibechat-desktop/internal/secrets"
)

const (
	defaultDataDir       = "/var/lib/vibechat"
	defaultListen        = "127.0.0.1:18120"
	defaultPreviewListen = "127.0.0.1:18121"
)

// paths of one server install (VIBECHAT_DATA_DIR, VIBECHAT_PROJECTS_DIR override the folders).
type paths struct{ data, projects string }

func (p paths) tokenHash() string  { return filepath.Join(p.data, "auth", "token.sha256") }
func (p paths) cursorKey() string  { return filepath.Join(p.data, "secrets", "cursor-key") }
func (p paths) instanceID() string { return filepath.Join(p.data, "instance-id") }
func (p paths) keys() secrets.File { return secrets.File{Path: p.cursorKey()} }

func serverPaths(getenv func(string) string) paths {
	p := paths{data: getenv("VIBECHAT_DATA_DIR"), projects: getenv("VIBECHAT_PROJECTS_DIR")}
	if p.data == "" {
		p.data = defaultDataDir
	}
	if p.projects == "" {
		p.projects = filepath.Join(p.data, "projects")
	}
	return p
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--hook" {
		in, _ := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
		_, _ = os.Stdout.Write(sandbox.RunHook(os.Args[2], in, os.Getenv))
		return
	}
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	p := serverPaths(os.Getenv)
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(p, args, stderr)
	case "--version", "-version", "version":
		fmt.Fprintln(stdout, "vibechat-server", appid.Version)
		return 0
	case "token":
		if len(args) != 1 || args[0] != "new" {
			fmt.Fprintln(stderr, "usage: vibechat-server token new")
			return 2
		}
		return tokenNew(p, stdout, stderr)
	case "set-key", "--set-key":
		return setKey(p, stdin, stderr)
	case "forget-key", "--forget-key":
		if err := p.keys().Forget(); err != nil {
			fmt.Fprintln(stderr, "forget-key:", err)
			return 1
		}
		fmt.Fprintln(stderr, "Removed the Cursor key from this server.")
		return 0
	case "doctor":
		return doctor(p, defaultListen, stdout)
	case "update":
		return update(args, stdout, stderr)
	case "--check-key", "check-key":
		return hostwire.CheckKey(p.data, "journalctl -u vibechat-server", p.keys(), "Settings in the app or `vibechat-server set-key`")
	case "--bridge-smoke":
		return hostwire.BridgeSmoke(p.data, "stderr")
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, "vibechat-server [serve | token new | set-key | forget-key | doctor | update | --check-key | --version]")
		return 0
	}
	fmt.Fprintf(stderr, "vibechat-server: unknown command %q (try --help)\n", cmd)
	return 2
}

// tokenNew makes the host token: the token goes to stdout once (the desktop app's setup reads it over SSH), only its
// sha256 stays on disk. A running server reloads nothing: restart it.
func tokenNew(p paths, stdout, stderr io.Writer) int {
	tok, err := auth.NewToken()
	if err != nil {
		fmt.Fprintln(stderr, "token:", err)
		return 1
	}
	if err := auth.WriteTokenHash(p.tokenHash(), tok); err != nil {
		fmt.Fprintln(stderr, "token:", err)
		return 1
	}
	if err := ownLikeData(p.data, p.tokenHash()); err != nil {
		fmt.Fprintln(stderr, "token:", err)
		return 1
	}
	fmt.Fprintln(stdout, tok)
	fmt.Fprintln(stderr, "New host token above (shown once; the old one no longer works). Then: systemctl restart vibechat-server")
	return 0
}

func setKey(p paths, stdin io.Reader, stderr io.Writer) int {
	fmt.Fprintln(stderr, "Paste the Cursor API key and press Enter:")
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(stderr, "set-key: no key read from stdin")
		return 1
	}
	key := strings.TrimSpace(line)
	if err := secrets.CheckFormat(key); err != nil {
		fmt.Fprintln(stderr, "set-key:", err)
		return 1
	}
	if err := p.keys().Store(key); err != nil {
		fmt.Fprintln(stderr, "set-key:", err)
		return 1
	}
	if err := ownLikeData(p.data, p.cursorKey()); err != nil {
		fmt.Fprintln(stderr, "set-key:", err)
		return 1
	}
	fmt.Fprintln(stderr, "Stored on this server (", p.cursorKey(), ", 0600).")
	return 0
}
