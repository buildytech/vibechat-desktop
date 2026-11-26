package remote

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testZip = "PK\x05\x06" + "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00" // an empty ZIP

func postJSON(t *testing.T, c *http.Client, u, body string, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", u, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = jsonDecode(res, &out)
	return res.StatusCode, out
}

// TestArchiveSavedThroughTheTunnel: POST /desktop/archive from the signed-in window asks SaveAs with the server's
// file name and writes the ZIP there through the tunnel; a closed dialog saves nothing; a missing project is a 502
// with the server's words (404 stays "not in the app" for the Studio); no session 401, another origin 403.
func TestArchiveSavedThroughTheTunnel(t *testing.T) {
	r := newRig(t, 1)
	dir := t.TempDir()
	var asked []string
	choose := filepath.Join(dir, "site.zip")
	r.m.o.SaveAs = func(name string) (string, error) {
		asked = append(asked, name)
		return choose, nil
	}
	if conn := r.connect(testToken); conn.Problem != nil {
		t.Fatalf("connect: %+v", conn.Problem)
	}
	u := r.navigated()
	origin := u[:strings.Index(u, "/auth/")]
	c := browser(t)
	signIn(t, c, u, origin)
	route := origin + "/runtime/v1/desktop/archive"

	code, out := postJSON(t, c, route, `{"projectId":"abc123"}`, nil)
	if code != 200 || out["saved"] != true || out["file"] != choose {
		t.Fatalf("save: %d %v", code, out)
	}
	if raw, _ := os.ReadFile(choose); string(raw) != testZip || len(asked) != 1 || asked[0] != "moy-lending.zip" {
		t.Fatalf("file %q, asked %v", raw, asked)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*.part")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}

	choose = ""
	if code, out := postJSON(t, c, route, `{"projectId":"abc123"}`, nil); code != 200 || out["saved"] != false {
		t.Fatalf("closed dialog: %d %v", code, out)
	}
	if code, out := postJSON(t, c, route, `{"projectId":"gone"}`, nil); code != http.StatusBadGateway || out["error"] != "Project not found" {
		t.Fatalf("missing project: %d %v", code, out)
	}
	if code, _ := postJSON(t, c, route, `{"projectId":"../x"}`, nil); code != 400 {
		t.Fatalf("bad id: %d", code)
	}
	if code, _ := postJSON(t, http.DefaultClient, route, `{"projectId":"abc123"}`, nil); code != 401 {
		t.Fatalf("no session: %d", code)
	}
	if code, _ := postJSON(t, c, route, `{"projectId":"abc123"}`, map[string]string{"Origin": "http://evil.example"}); code != 403 {
		t.Fatalf("other origin: %d", code)
	}
	if len(asked) != 2 {
		t.Fatalf("the dialog opened %d times, want 2 (not for refused requests)", len(asked))
	}
}

func TestArchiveName(t *testing.T) {
	for cd, want := range map[string]string{
		`attachment; filename="moy-lending.zip"`: "moy-lending.zip",
		`attachment; filename="../../etc/x.zip"`: "x.zip",
		`attachment; filename="..\\a\\b.zip"`:    "b.zip",
		`attachment; filename=".hidden"`:         "project.zip",
		`attachment; filename="site"`:            "site.zip",
		``:                                       "project.zip",
	} {
		res := &http.Response{Header: http.Header{"Content-Disposition": {cd}}}
		if got := archiveName(res); got != want {
			t.Errorf("%q → %q, want %q", cd, got, want)
		}
	}
}

func jsonDecode(res *http.Response, v any) error { return json.NewDecoder(res.Body).Decode(v) }
