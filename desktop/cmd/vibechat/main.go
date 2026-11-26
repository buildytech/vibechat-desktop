// BuildY VibeChat desktop: a window to the owner's vibechat-server (Debian 13). The app runs no agent, no projects
// and no templates on this computer. Not connected, the window shows the start screen (desktop/shell: connect a
// server, or set up a new one with the wizard); connected, it shows the server's Studio through the SSH tunnel
// (desktop/internal/remote). The app answers only its own routes (/runtime/v1/desktop/*); any other seam path is 404.
//
//	vibechat                    open the app
//	vibechat --version          print the version
//	vibechat --probe-localhost  check that this webview frames http://<name>.preview.localhost:<port> (CI)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/buildytech/vibechat-desktop/desktop/shell"
	"github.com/buildytech/vibechat-desktop/internal/appid"
	"github.com/buildytech/vibechat-desktop/internal/applog"
	"github.com/buildytech/vibechat-desktop/internal/webapp"
	"github.com/buildytech/vibechat-desktop/protocol"
)

// logName is the desktop log: <data>/logs/vibechat.log (Windows: %APPDATA%\tw.buildy.vibechat\logs\vibechat.log).
const logName = "vibechat.log"

func main() {
	showVersion := flag.Bool("version", false, "print the app version and exit")
	probe := flag.Bool("probe-localhost", false, "check that this webview frames http://<name>.preview.localhost:<port> (the server preview gateway), print JSON, exit (CI)")
	flag.Parse()
	switch {
	case *showVersion:
		fmt.Println(appid.Name, appid.Version)
		return
	case *probe:
		os.Exit(probeLocalhost())
	}

	seam, err := protocol.Pinned()
	if err != nil {
		log.Fatal(err)
	}
	data := dataDir()
	if f := applog.Setup(data, logName); f != "" {
		log.Printf("vibechat %s: log %s", appid.Version, f)
	}
	var mainWindow *application.WebviewWindow
	var app *application.App
	conn := newRemote(seam, data, func(u string) {
		if mainWindow != nil {
			mainWindow.SetURL(u)
		}
	}, func(name string) (string, error) { return saveAs(app, mainWindow, name) })
	defer conn.Close()

	app = application.New(application.Options{
		Name:        appid.Name,
		Description: "BuildY VibeChat desktop (a window to your vibechat-server)",
		Mac:         application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: true},
		// The WebView2 data (the Studio's local state) lives under the fixed app id, not the binary name.
		Windows: application.WindowsOptions{WebviewUserDataPath: webviewDataDir()},
		Assets: application.AssetOptions{
			Handler: webapp.Handler(shell.Frontend()),
			// The desktop routes (connection, setup wizard) first; every other seam path is 404 (no local host).
			Middleware: func(next http.Handler) http.Handler { return conn.Middleware(seamNotFound(seam.Prefix, next)) },
		},
	})
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
	// A saved, active server: reconnect at start (the start screen shows "Connecting…", then the server's Studio, or
	// the choice with the problem).
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		conn.StartResume(context.Background())
	})
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

// saveAs is the system's Save dialog over the main window, in the Downloads folder, for the project's ZIP
// (POST /desktop/archive): the chosen path, or "" when the user closed it.
func saveAs(app *application.App, w *application.WebviewWindow, name string) (string, error) {
	if app == nil {
		return "", errors.New("the app is not running yet")
	}
	d := app.Dialog.SaveFile().SetFilename(name).CanCreateDirectories(true).AddFilter("ZIP", "*.zip")
	if dir := application.Path(application.PathDownload); dir != "" {
		d.SetDirectory(dir)
	}
	if w != nil {
		d.AttachToWindow(w)
	}
	return d.PromptForSingleSelection()
}

// webviewDataDir is <UserConfigDir>/tw.buildy.vibechat/WebView2 (used on Windows only).
func webviewDataDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		return ""
	}
	return filepath.Join(base, appid.ID, "WebView2")
}
