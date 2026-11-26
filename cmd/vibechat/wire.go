package main

import (
	"log"

	"github.com/buildytech/vibechat-desktop/internal/appid"
	"github.com/buildytech/vibechat-desktop/internal/host"
	"github.com/buildytech/vibechat-desktop/internal/hostwire"
	"github.com/buildytech/vibechat-desktop/internal/preview"
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
