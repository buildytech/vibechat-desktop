package main

import "strings"

// settingsRoute is the Settings screen of the server's Studio (studio apps/studio/src/settings, ADR 0007: it opens
// the first section; the connection is the Server section).
const settingsRoute = "/settings"

// settingsAccelerator opens Settings the way desktop apps do.
const settingsAccelerator = "CmdOrCtrl+,"

// settingsLabel is the menu item in the system language: "Настройки…" for Russian, else "Settings…".
func settingsLabel(getenv func(string) string) string {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG", "LANGUAGE"} {
		if v := strings.ToLower(getenv(name)); v != "" {
			if strings.HasPrefix(v, "ru") {
				return "Настройки…"
			}
			break
		}
	}
	return "Settings…"
}
