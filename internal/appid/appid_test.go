package appid

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The packaging files must carry the fixed identity: a drifted id would orphan the WebView's saved sessions.
func TestPackagingFilesCarryTheAppID(t *testing.T) {
	root := filepath.Join("..", "..", "build")
	for file, wants := range map[string][]string{
		"darwin/Info.plist":            {"<string>" + ID + "</string>", "<string>" + Name + "</string>", "<string>" + Binary + "</string>"},
		"windows/info.json":            {`"ProductName": "` + Name + `"`, `"CompanyName": "` + Company + `"`},
		"windows/nsis/wails_tools.nsh": {`!define INFO_PROJECTNAME "` + Binary + `"`, `!define INFO_PRODUCTNAME "` + Name + `"`, `!define INFO_COMPANYNAME "` + Company + `"`},
		"linux/nfpm/nfpm.yaml":         {`name: "` + Binary + `"`, "/usr/bin/" + Binary, "applications/" + Binary + ".desktop"},
		"linux/" + Binary + ".desktop": {"Name=" + Name, "Exec=" + Binary, "Icon=" + Binary, "StartupWMClass=" + Binary},
	} {
		raw, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range wants {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s: missing %q", file, want)
			}
		}
	}
}
