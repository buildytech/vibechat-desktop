//go:build server

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// appMenu: the headless build has no window menu.
func appMenu(*application.App, func() *application.WebviewWindow) *application.Menu { return nil }
