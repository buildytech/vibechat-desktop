// BuildY VibeChat desktop: the vibecode Studio in a Wails 3 window, with its host seam (/runtime/v1/*) answered in
// process by Go. Build tags: none for the desktop app, `server` for the headless HTTP mode (tests, the exam).
//
//	vibechat              open the app
//	vibechat --set-key    read the Cursor API key from stdin and keep it in the OS keychain (never pass it in argv)
//	vibechat --forget-key remove it from the keychain
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v3/pkg/application"

	vibechat "github.com/buildytech/vibechat-desktop"
	"github.com/buildytech/vibechat-desktop/internal/appid"
	"github.com/buildytech/vibechat-desktop/internal/host"
	"github.com/buildytech/vibechat-desktop/internal/secrets"
	"github.com/buildytech/vibechat-desktop/internal/webapp"
)

func main() {
	setKey := flag.Bool("set-key", false, "read the Cursor API key from stdin and store it in the OS keychain")
	forgetKey := flag.Bool("forget-key", false, "remove the Cursor API key from the OS keychain")
	flag.Parse()
	switch {
	case *setKey:
		fmt.Fprintln(os.Stderr, "Paste the Cursor API key and press Enter:")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			log.Fatal("no key read from stdin")
		}
		if err := (secrets.Keys{}).Store(line); err != nil {
			log.Fatalf("keychain: %v", err)
		}
		fmt.Fprintln(os.Stderr, "Stored in the OS keychain.")
		return
	case *forgetKey:
		if err := (secrets.Keys{}).Forget(); err != nil {
			log.Fatalf("keychain: %v", err)
		}
		fmt.Fprintln(os.Stderr, "Removed from the OS keychain.")
		return
	}

	seam, err := host.LoadSeam(vibechat.HostSeam())
	if err != nil {
		log.Fatal(err)
	}
	h := host.New(host.Options{Seam: seam, Version: appid.Version})

	app := application.New(application.Options{
		Name:        appid.Name,
		Description: "BuildY VibeChat desktop (the vibecode Studio in a Wails window)",
		Mac:         application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: true},
		// The WebView2 data (the Studio's local state) lives under the fixed app id, not the binary name.
		Windows: application.WindowsOptions{WebviewUserDataPath: webviewDataDir()},
		Server:  serverOptions(),
		Assets: application.AssetOptions{
			Handler:    webapp.Handler(vibechat.Frontend()),
			Middleware: host.Middleware(seam.Prefix, h),
		},
	})
	var mainWindow *application.WebviewWindow
	if menu := appMenu(app, func() *application.WebviewWindow { return mainWindow }); menu != nil {
		app.Menu.Set(menu)
	}
	mainWindow = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:              appid.Name,
		Width:              1440,
		Height:             900,
		URL:                "/",
		UseApplicationMenu: true,
	})
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

// webviewDataDir is <UserConfigDir>/tw.buildy.vibechat/WebView2 (used on Windows only).
func webviewDataDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		return ""
	}
	return filepath.Join(base, appid.ID, "WebView2")
}
