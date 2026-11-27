package provision

import (
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/internal/gopin"
)

// TestGoIsProvisioned: the egress allowlist has Go's hosts and the server step installs the pinned toolchain
// (checked by its SHA-256, before the package so its postinstall finds it).
func TestGoIsProvisioned(t *testing.T) {
	have := map[string]bool{}
	for _, d := range DefaultDomains {
		have[d] = true
	}
	for _, d := range gopin.Domains {
		if !have[d] {
			t.Fatalf("DefaultDomains lacks %s", d)
		}
	}
	w := &wizard{}
	v := w.serverVars()
	if v["GO_SHA"] != gopin.SHA256 || v["GO_URL"] != gopin.URL || v["GO_VERSION"] != gopin.Version || v["GO_DIR"] != gopin.Dir {
		t.Fatalf("server vars %v", v)
	}
	if !strings.Contains(serverApply, `sha256sum -c --strict --quiet - || { echo "Go does not match`) {
		t.Fatal("Go is not checked by its SHA-256")
	}
	if strings.Index(serverApply, "go.tgz\" \"$C/root/go.tgz\"") > strings.Index(serverApply, "vibechat-server.deb\"") {
		t.Fatal("Go must be in place before the package installs")
	}
	if !strings.Contains(serverCheck, `"$GO_DIR/go/bin/go" env GOVERSION`) {
		t.Fatal("the server check does not look at Go")
	}
}
