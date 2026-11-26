package host

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/internal/remote"
)

// TestDesktopRoutesMatchTheContract: the pinned seam's desktop routes are what internal/remote answers, the host
// router does not (a vibechat-server answers 404, the Studio hides the server settings), and the JSON fields of
// remote.Connection are the contract's desktopConnection.
func TestDesktopRoutesMatchTheContract(t *testing.T) {
	s := pinnedSeam(t)
	if len(s.Desktop) != 3 || s.TunnelUser != "vibechat-tunnel" {
		t.Fatalf("desktop %v, tunnel user %q", s.Desktop, s.TunnelUser)
	}
	m, err := remote.New(remote.Options{DataDir: t.TempDir(), Creds: &remote.MemCreds{}, Prefix: s.Prefix})
	if err != nil {
		t.Fatal(err)
	}
	var methods []string
	for _, r := range s.Desktop {
		if s.Prefix+r.Path != m.DesktopPath() {
			t.Errorf("%s: remote answers %s", r.Key(), m.DesktopPath())
		}
		methods = append(methods, r.Method)
	}
	slices.Sort(methods)
	if !reflect.DeepEqual(methods, []string{"DELETE", "GET", "PUT"}) {
		t.Errorf("methods %v", methods)
	}
	h := testHost(t, s)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", s.Prefix+"/desktop/connection", nil))
	if rec.Code != 404 {
		t.Errorf("the host router answers the desktop route: %d", rec.Code)
	}
	var fields []string
	ty := reflect.TypeOf(remote.Connection{})
	for i := range ty.NumField() {
		fields = append(fields, strings.Split(ty.Field(i).Tag.Get("json"), ",")[0])
	}
	if !reflect.DeepEqual(fields, s.DesktopConnection) {
		t.Errorf("remote.Connection fields %v, contract %v", fields, s.DesktopConnection)
	}
	raw, _ := json.Marshal(m.Connection())
	if !strings.Contains(string(raw), `"mode":"local"`) {
		t.Errorf("%s", raw)
	}
}
