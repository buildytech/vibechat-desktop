package host

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/internal/provision"
	"github.com/buildytech/vibechat-desktop/internal/remote"
)

func jsonFields(v any) []string {
	var out []string
	ty := reflect.TypeOf(v)
	for i := range ty.NumField() {
		out = append(out, strings.Split(ty.Field(i).Tag.Get("json"), ",")[0])
	}
	return out
}

// TestDesktopRoutesMatchTheContract: the pinned seam's desktop routes are what internal/remote answers, the host
// router does not (a vibechat-server answers 404, the Studio hides the server settings), and the JSON fields of
// remote.Connection are the contract's desktopConnection.
func TestDesktopRoutesMatchTheContract(t *testing.T) {
	s := pinnedSeam(t)
	if len(s.Desktop) != 5 || s.TunnelUser != "vibechat-tunnel" {
		t.Fatalf("desktop %v, tunnel user %q", s.Desktop, s.TunnelUser)
	}
	m, err := remote.New(remote.Options{DataDir: t.TempDir(), Creds: &remote.MemCreds{}, Prefix: s.Prefix})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range s.Desktop {
		if p := s.Prefix + r.Path; p != m.DesktopPath() && p != m.ProvisionPath() {
			t.Errorf("%s: remote answers %s and %s", r.Key(), m.DesktopPath(), m.ProvisionPath())
		}
		keys = append(keys, r.Key())
	}
	slices.Sort(keys)
	want := []string{"DELETE /desktop/connection", "GET /desktop/connection", "GET /desktop/provision", "POST /desktop/provision", "PUT /desktop/connection"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("routes %v", keys)
	}
	if !reflect.DeepEqual(jsonFields(remote.Provision{}), s.DesktopProvision) {
		t.Errorf("remote.Provision fields %v, contract %v", jsonFields(remote.Provision{}), s.DesktopProvision)
	}
	if !reflect.DeepEqual(jsonFields(remote.ProvisionInput{}), s.DesktopProvisionInput) {
		t.Errorf("remote.ProvisionInput fields %v, contract %v", jsonFields(remote.ProvisionInput{}), s.DesktopProvisionInput)
	}
	if !reflect.DeepEqual(provision.Steps, s.ProvisionSteps) {
		t.Errorf("provision.Steps %v, contract %v", provision.Steps, s.ProvisionSteps)
	}
	h := testHost(t, s)
	for _, p := range []string{"/desktop/connection", "/desktop/provision"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", s.Prefix+p, nil))
		if rec.Code != 404 {
			t.Errorf("the host router answers %s: %d", p, rec.Code)
		}
	}
	fields := jsonFields(remote.Connection{})
	if !reflect.DeepEqual(fields, s.DesktopConnection) {
		t.Errorf("remote.Connection fields %v, contract %v", fields, s.DesktopConnection)
	}
	raw, _ := json.Marshal(m.Connection())
	if !strings.Contains(string(raw), `"mode":"local"`) {
		t.Errorf("%s", raw)
	}
}
