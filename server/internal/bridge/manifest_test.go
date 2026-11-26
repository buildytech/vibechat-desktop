package bridge

import (
	"encoding/json"
	"testing"
)

// The upstream v1.2.0 manifest still says bridgeVersion 1.0.0: the log and doctor report the pinned release.
func TestManifestDescribe(t *testing.T) {
	var m Manifest
	if err := json.Unmarshal([]byte(`{"bridgeVersion":"1.0.0","sdkVersion":"1.2.0","release":{"version":"1.2.0"}}`), &m); err != nil {
		t.Fatal(err)
	}
	if got := m.Describe(); got != "release 1.2.0 (manifest bridgeVersion 1.0.0, sdk 1.2.0)" {
		t.Fatal(got)
	}
	m.BridgeVersion = "1.2.0"
	if got := m.Describe(); got != "release 1.2.0 (sdk 1.2.0)" {
		t.Fatal(got)
	}
	m.Release.Version = ""
	if got := m.Describe(); got != "1.2.0 (sdk 1.2.0; no release stamp)" {
		t.Fatal(got)
	}
}
