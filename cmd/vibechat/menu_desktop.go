//go:build !server

package main

import (
	"os"
	goruntime "runtime"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/buildytech/vibechat-desktop/internal/appid"
)

// appMenu is the native application menu: Settings… (CmdOrCtrl+,) navigates the main window to the Settings screen
// ; the rest are the platform's standard menus.
func appMenu(app *application.App, window func() *application.WebviewWindow) *application.Menu {
	openSettings := func(*application.Context) {
		if w := window(); w != nil {
			w.SetURL(settingsRoute)
			w.Show()
			w.Focus()
		}
	}
	label := settingsLabel(os.Getenv)
	menu := app.NewMenu()
	if goruntime.GOOS == "darwin" {
		appSub := menu.AddSubmenu(appid.Name)
		appSub.AddRole(application.About)
		appSub.AddSeparator()
		appSub.Add(label).SetAccelerator(settingsAccelerator).OnClick(openSettings)
		appSub.AddSeparator()
		appSub.AddRole(application.Hide)
		appSub.AddRole(application.HideOthers)
		appSub.AddRole(application.ShowAll)
		appSub.AddSeparator()
		appSub.AddRole(application.Quit)
	} else {
		file := menu.AddSubmenu("File")
		file.Add(label).SetAccelerator(settingsAccelerator).OnClick(openSettings)
		file.AddSeparator()
		file.AddRole(application.Quit)
	}
	menu.AddRole(application.EditMenu)
	menu.AddRole(application.ViewMenu)
	menu.AddRole(application.WindowMenu)
	return menu
}
