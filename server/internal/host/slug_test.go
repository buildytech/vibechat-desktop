package host

import "testing"

func TestProjectSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Кофейня Зерно":       "kofeynya-zerno",
		"Northwind Bikes":     "northwind-bikes",
		"Студия йоги «Прана»": "studiya-yogi-prana",
		"!!!":                 "project",
		"日本":                  "project",
	} {
		if got := projectSlug(in); got != want {
			t.Errorf("projectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
