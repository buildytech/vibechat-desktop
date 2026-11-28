package main

import (
	"os"
	goruntime "runtime"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/buildytech/vibechat-desktop/internal/appid"
)

// appMenu is the native application menu: Settings… (CmdOrCtrl+,) opens the Settings screen of the server's Studio
// through open (remote.Manager.Open: a fresh sign-in on the server's origin; not connected, the start screen); the
// rest are the platform's standard menus.
func appMenu(window func() *application.WebviewWindow, open func(route string)) *application.Menu {
	openSettings := func(*application.Context) {
		if w := window(); w != nil {
			w.Show()
			w.Focus()
		}
		go open(settingsRoute)
	}
	return buildMenu(goruntime.GOOS, settingsLabel(os.Getenv), openSettings)
}

// buildMenu has the Edit role only on macOS. On Windows (and Linux) the role binds Ctrl+V/C/X/Z/A and Backspace as
// menu accelerators: on Windows each Ctrl+V ran the role's own paste (navigator.clipboard + insertText) besides
// WebView2's, so a paste landed twice (the owner's report; the right-click menu pasted once), and Backspace went to
// the Delete item. WebView2 and WebKitGTK handle the editing keys themselves and have a context menu; macOS needs
// the role, or Cmd+V does nothing in a WKWebView.
func buildMenu(goos, label string, openSettings func(*application.Context)) *application.Menu {
	menu := application.NewMenu()
	if goos == "darwin" {
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
		menu.AddRole(application.EditMenu)
	} else {
		file := menu.AddSubmenu("File")
		file.Add(label).SetAccelerator(settingsAccelerator).OnClick(openSettings)
		file.AddSeparator()
		file.AddRole(application.Quit)
	}
	menu.AddRole(application.ViewMenu)
	menu.AddRole(application.WindowMenu)
	return menu
}
