package main

import (
	"log"
	"os"
	"runtime"

	"github.com/buildytech/vibechat-desktop/internal/appid"
	"github.com/buildytech/vibechat-desktop/internal/host"
	"github.com/buildytech/vibechat-desktop/internal/hostwire"
	"github.com/buildytech/vibechat-desktop/internal/preview"
	"github.com/buildytech/vibechat-desktop/internal/remote"
	"github.com/buildytech/vibechat-desktop/internal/secrets"
)

// logName is the desktop log: <data>/logs/vibechat.log (Windows: %APPDATA%\tw.buildy.vibechat\logs\vibechat.log).
const logName = "vibechat.log"

// newHost builds the in-process host seam: OS keychain, per-project preview ports, no auth (the Wails window is the
// only client).
func newHost(seam host.Seam) (*host.Host, func()) {
	data, projects := hostwire.DesktopDirs()
	if f := hostwire.SetupLog(data, logName); f != "" {
		log.Printf("vibechat %s: log %s", appid.Version, f)
	}
	h, closeHost, err := hostwire.New(seam, hostwire.Config{
		DataDir: data, ProjectsDir: projects, Keys: secrets.Keys{}, Preview: preview.New(func(s string) { log.Print(s) }),
	})
	if err != nil {
		log.Fatal(err)
	}
	return h, closeHost
}

func bridgeSmoke() int {
	data, _ := hostwire.DesktopDirs()
	return hostwire.BridgeSmoke(data, hostwire.SetupLog(data, logName))
}

// checkKey: on Windows run it as `vibechat.exe --check-key > check.txt` (the GUI build has no console).
func checkKey() int {
	data, _ := hostwire.DesktopDirs()
	return hostwire.CheckKey(data, hostwire.SetupLog(data, logName), secrets.Keys{}, "Settings in the app or `vibechat --set-key`")
}

// newRemote is the connection to the owner's vibechat-server (SSH key and host token in the OS keychain, the saved
// server in <data>/remote.json). The app speaks the pinned seam's protocol and accepts servers down to its
// minClientProtocol (one compatibility window for both sides).
func newRemote(seam host.Seam, navigate func(string)) *remote.Manager {
	data, _ := hostwire.DesktopDirs()
	m, err := remote.New(remote.Options{
		DataDir: data, Creds: remote.Keyring{}, AppVersion: appid.Version,
		AppProtocol: seam.Protocol, MinServerProtocol: seam.MinClientProtocol,
		Prefix: seam.Prefix, SessionPath: seam.Auth.SessionPath, TicketParam: seam.Auth.TicketParam,
		Navigate: navigate, Log: func(s string) { log.Print(s) }, PreviewPortPerProject: previewPortPerProject(),
	})
	if err != nil {
		log.Fatal(err)
	}
	return m
}

// previewPortPerProject: macOS does not resolve *.localhost (CI probe: the system resolver answers "no such host",
// WKWebView "Load failed"; WebView2 and WebKitGTK resolve it), so there every preview gets its own 127.0.0.1 port.
// VIBECHAT_PREVIEW_PORTS=1 / 0 forces it on / off.
func previewPortPerProject() bool {
	switch os.Getenv("VIBECHAT_PREVIEW_PORTS") {
	case "1":
		return true
	case "0":
		return false
	}
	return runtime.GOOS == "darwin"
}
