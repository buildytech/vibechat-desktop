package main

import "testing"

func TestSettingsLabel(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, c := range []struct {
		vars map[string]string
		want string
	}{
		{map[string]string{}, "Settings…"},
		{map[string]string{"LANG": "ru_RU.UTF-8"}, "Настройки…"},
		{map[string]string{"LANG": "en_US.UTF-8"}, "Settings…"},
		{map[string]string{"LC_ALL": "en_GB.UTF-8", "LANG": "ru_RU.UTF-8"}, "Settings…"},
	} {
		if got := settingsLabel(env(c.vars)); got != c.want {
			t.Errorf("%v: got %q, want %q", c.vars, got, c.want)
		}
	}
	if settingsRoute != "/settings" || settingsAccelerator != "CmdOrCtrl+," {
		t.Fatal("the Settings item changed")
	}
}
