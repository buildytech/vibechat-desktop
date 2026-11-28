package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func contourPaths(t *testing.T, doc string) paths {
	t.Helper()
	data := t.TempDir()
	t.Setenv("VIBECHAT_CONTOUR_FILE", "")
	if doc != "" {
		if err := os.WriteFile(filepath.Join(data, "contour.json"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return paths{data: data, projects: filepath.Join(data, "projects")}
}

func TestEgressDomainsListsTheChosenProviderOnly(t *testing.T) {
	var out, errb bytes.Buffer
	p := contourPaths(t, `{"runtime":"pi","provider":"gigachat","gigachat":{"scope":"GIGACHAT_API_B2B","models":[{"id":"g"}]}}`)
	if code := egressDomains(p, &out, &errb); code != 0 || out.String() != "api.giga.chat\nngw.devices.sberbank.ru\n" {
		t.Fatalf("%d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	p = contourPaths(t, `{"runtime":"pi","provider":"yandex","yandex":{"folderId":"f","models":[{"id":"y"}]}}`)
	if code := egressDomains(p, &out, &errb); code != 0 || out.String() != "ai.api.cloud.yandex.net\n" {
		t.Fatalf("%d %q", code, out.String())
	}
	out.Reset()
	errb.Reset()
	if code := egressDomains(contourPaths(t, ""), &out, &errb); code != 0 || out.Len() != 0 || !strings.Contains(errb.String(), "no provider hosts") {
		t.Fatalf("cursor contour: %d %q %q", code, out.String(), errb.String())
	}
	errb.Reset()
	if code := egressDomains(contourPaths(t, `{"runtime":"pi"}`), &out, &errb); code != 1 || !strings.Contains(errb.String(), "provider") {
		t.Fatalf("bad config: %d %q", code, errb.String())
	}
}

func TestRunDispatchesEgressDomains(t *testing.T) {
	data := t.TempDir()
	t.Setenv("VIBECHAT_DATA_DIR", data)
	t.Setenv("VIBECHAT_CONTOUR_FILE", "")
	if err := os.WriteFile(filepath.Join(data, "contour.json"), []byte(`{"runtime":"pi","provider":"yandex","yandex":{"folderId":"f","models":[{"id":"y"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"egress-domains"}, strings.NewReader(""), &out, &errb); code != 0 || out.String() != "ai.api.cloud.yandex.net\n" {
		t.Fatalf("%d %q %q", code, out.String(), errb.String())
	}
}

func TestDoctorContourChecks(t *testing.T) {
	t.Setenv("VIBECHAT_PI_DIR", "")
	t.Setenv("VIBECHAT_PI_NODE", "")
	t.Setenv("VIBECHAT_YANDEX_API_KEY", "")
	cs := contourChecks(contourPaths(t, ""))
	if len(cs) != 1 || cs[0].level != "ok" || !strings.Contains(cs[0].detail, "cursor-sdk") {
		t.Fatalf("%+v", cs)
	}
	cs = contourChecks(contourPaths(t, `{"runtime":"nope"}`))
	if len(cs) != 1 || cs[0].level != "FAIL" {
		t.Fatalf("%+v", cs)
	}
	cs = contourChecks(contourPaths(t, `{"contour":"ru","runtime":"pi","provider":"yandex","yandex":{"folderId":"f","models":[{"id":"y"}]}}`))
	byName := map[string]check{}
	for _, c := range cs {
		byName[c.name] = c
	}
	if byName["contour"].level != "ok" || byName["pi"].level != "FAIL" || !strings.Contains(byName["pi"].detail, "not set") ||
		byName["pi provider"].level != "warn" || byName["egress"].detail != "open only: ai.api.cloud.yandex.net" {
		t.Fatalf("%+v", cs)
	}
	t.Setenv("VIBECHAT_YANDEX_API_KEY", "set")
	for _, c := range contourChecks(contourPaths(t, `{"runtime":"pi","provider":"yandex","yandex":{"folderId":"f","models":[{"id":"y"}]}}`)) {
		if c.name == "pi provider" && (c.level != "ok" || strings.Contains(c.detail, "set\n")) {
			t.Fatalf("%+v", c)
		}
	}
}

// Several providers: doctor names each one, the default first, and says per provider whether its key is there; the
// egress line is the union of the hosts.
func TestDoctorContourChecksOfSeveralProviders(t *testing.T) {
	t.Setenv("VIBECHAT_PI_DIR", "")
	t.Setenv("VIBECHAT_PI_NODE", "")
	t.Setenv("VIBECHAT_CLOUDRU_KEY", "")
	t.Setenv("VIBECHAT_OPENROUTER_KEY", "or-set")
	doc := `{"contour":"ru","runtime":"pi","provider":"openai","openai":[
	 {"id":"cloudru","baseUrl":"https://foundation-models.api.cloud.ru/v1","keyEnv":"VIBECHAT_CLOUDRU_KEY","models":[{"id":"giga"}]},
	 {"id":"openrouter","baseUrl":"https://openrouter.ai/api/v1","keyEnv":"VIBECHAT_OPENROUTER_KEY","models":[{"id":"oss"}]}]}`
	var provs []check
	var egress, contourLine check
	for _, c := range contourChecks(contourPaths(t, doc)) {
		switch c.name {
		case "pi provider":
			provs = append(provs, c)
		case "egress":
			egress = c
		case "contour":
			contourLine = c
		}
	}
	if len(provs) != 2 || provs[0].level != "warn" || !strings.HasPrefix(provs[0].detail, "cloudru (default): ") ||
		provs[1].level != "ok" || !strings.HasPrefix(provs[1].detail, "openrouter: key present") {
		t.Fatalf("%+v", provs)
	}
	if egress.detail != "open only: foundation-models.api.cloud.ru, openrouter.ai" || !strings.Contains(contourLine.detail, "provider cloudru, openrouter") {
		t.Fatalf("%+v %+v", egress, contourLine)
	}
	if strings.Contains(provs[1].detail, "or-set") {
		t.Fatal("a key is in the doctor output")
	}
}
