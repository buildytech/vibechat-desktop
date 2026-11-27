package remote_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/desktop/internal/provision"
	"github.com/buildytech/vibechat-desktop/desktop/internal/remote"
	"github.com/buildytech/vibechat-desktop/protocol"
)

func jsonFields(v any) []string {
	var out []string
	ty := reflect.TypeOf(v)
	for i := range ty.NumField() {
		out = append(out, strings.Split(ty.Field(i).Tag.Get("json"), ",")[0])
	}
	return out
}

// TestDesktopRoutesMatchTheContract: the pinned seam's desktop routes are what remote answers, and the JSON fields
// of Connection, Provision, ProvisionInput and the wizard's steps are the contract's.
func TestDesktopRoutesMatchTheContract(t *testing.T) {
	s, err := protocol.Pinned()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Desktop) != 6 || s.TunnelUser != "vibechat-tunnel" {
		t.Fatalf("desktop %v, tunnel user %q", s.Desktop, s.TunnelUser)
	}
	m, err := remote.New(remote.Options{DataDir: t.TempDir(), Creds: &remote.MemCreds{}, Prefix: s.Prefix})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range s.Desktop {
		if p := s.Prefix + r.Path; p != m.DesktopPath() && p != m.ProvisionPath() && p != m.ArchivePath() {
			t.Errorf("%s: remote answers %s, %s and %s", r.Key(), m.DesktopPath(), m.ProvisionPath(), m.ArchivePath())
		}
		keys = append(keys, r.Key())
	}
	slices.Sort(keys)
	want := []string{"DELETE /desktop/connection", "GET /desktop/connection", "GET /desktop/provision", "POST /desktop/archive", "POST /desktop/provision", "PUT /desktop/connection"}
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
	if !reflect.DeepEqual(provision.Substeps, s.ProvisionSubsteps) {
		t.Errorf("provision.Substeps %v, contract %v", provision.Substeps, s.ProvisionSubsteps)
	}
	if !reflect.DeepEqual(jsonFields(remote.Archive{}), s.DesktopArchive) || !reflect.DeepEqual(jsonFields(remote.ArchiveInput{}), s.DesktopArchiveInput) {
		t.Errorf("remote.Archive %v / ArchiveInput %v, contract %v / %v", jsonFields(remote.Archive{}), jsonFields(remote.ArchiveInput{}), s.DesktopArchive, s.DesktopArchiveInput)
	}
	if fields := jsonFields(remote.Connection{}); !reflect.DeepEqual(fields, s.DesktopConnection) {
		t.Errorf("remote.Connection fields %v, contract %v", fields, s.DesktopConnection)
	}
	raw, _ := json.Marshal(m.Connection())
	if !strings.Contains(string(raw), `"mode":"local"`) {
		t.Errorf("%s", raw)
	}
}
