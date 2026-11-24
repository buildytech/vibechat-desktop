//go:build !server

package main

import (
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func accelerators(m *application.Menu, out map[string]string) {
	for i := 0; m.ItemAt(i) != nil; i++ {
		item := m.ItemAt(i)
		if a := item.GetAccelerator(); a != "" {
			// CmdOrCtrl reads "cmd+" when the test runs on macOS.
			out[strings.Replace(strings.ToLower(a), "cmd+", "ctrl+", 1)] = item.Label()
		}
		if sub := item.GetSubmenu(); sub != nil {
			accelerators(sub, out)
		}
	}
}

// Windows: the Edit role's Ctrl+V pasted a second time on top of WebView2's own paste; no menu item may take the
// editing keys (or Enter) from the page there.
func TestMenuLeavesEditingKeysToThePage(t *testing.T) {
	editing := []string{"ctrl+v", "ctrl+c", "ctrl+x", "ctrl+a", "ctrl+z", "ctrl+shift+z", "backspace", "enter", "return"}
	control := map[string]string{}
	role := application.NewMenu()
	role.AddRole(application.EditMenu)
	accelerators(role, control)
	if control["ctrl+v"] != "Paste" || control["backspace"] == "" {
		t.Fatalf("the check no longer sees the Edit role's keys: %v", control)
	}
	if goruntime.GOOS == "darwin" {
		t.Skip("macOS role menus need a running app; the Windows and Linux runners check this")
	}
	for _, goos := range []string{"windows", "linux"} {
		got := map[string]string{}
		accelerators(buildMenu(goos, "Settings…", nil), got)
		for _, key := range editing {
			if label, taken := got[key]; taken {
				t.Errorf("%s: %q is bound to the menu item %q", goos, key, label)
			}
		}
		if got["ctrl+,"] != "Settings…" {
			t.Errorf("%s: Settings accelerator missing: %v", goos, got)
		}
	}
}
