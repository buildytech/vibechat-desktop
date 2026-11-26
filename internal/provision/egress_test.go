package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const stubIncus = `#!/bin/bash
echo "$*" >> "$STUB/calls"
case "$1 $2" in
"network get") echo 10.20.30.1/24 ;;
"network acl")
	case "$3" in
	show) [ -e "$STUB/acl.made" ] ;;
	create) touch "$STUB/acl.made" ;;
	edit) cat > "$STUB/acl.yaml" ;;
	esac ;;
"network set") ;;
"info "*) echo "Status: RUNNING" ;;
"exec "*) printf '127.0.0.1 localhost\n# vibechat-egress begin\n9.9.9.9 old\n# vibechat-egress end\n::1 ip6-localhost\n' | sed '/^# vibechat-egress begin/,/^# vibechat-egress end/d' ;;
"file push") cp "${@: -2:1}" "$STUB/hosts.pushed"; echo "${@:3}" > "$STUB/push.args" ;;
*) echo "stub incus: $*" >&2; exit 1 ;;
esac
`

const stubGetent = `#!/bin/bash
[ "$1" = ahostsv4 ] || exit 2
f="$STUB/dns/$2"
[ -e "$f" ] || exit 2
while read -r ip; do echo "$ip STREAM $2"; echo "$ip DGRAM"; done < "$f"
`

func egressEnv(t *testing.T) (dir string, run func() (string, error)) {
	dir = t.TempDir()
	bin, etc, state := filepath.Join(dir, "bin"), filepath.Join(dir, "etc"), filepath.Join(dir, "state")
	for _, d := range []string{bin, etc, filepath.Join(dir, "dns")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string, mode os.FileMode) {
		if err := os.WriteFile(p, []byte(s), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "incus"), stubIncus, 0o755)
	write(filepath.Join(bin, "getent"), stubGetent, 0o755)
	w := &wizard{cfg: cfg()}
	_ = w.cfg.defaults()
	w.cfg.Domains = []string{"api2.cursor.sh", "registry.npmjs.org"}
	files := w.egressFiles()
	write(filepath.Join(etc, "egress.conf"), files["/etc/vibechat/egress.conf"], 0o644)
	write(filepath.Join(etc, "egress.domains"), files["/etc/vibechat/egress.domains"], 0o644)
	write(filepath.Join(dir, "egress"), egressScript, 0o755)
	return dir, func() (string, error) {
		cmd := exec.Command("bash", filepath.Join(dir, "egress"))
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "STUB="+dir, "VIBECHAT_EGRESS_ETC="+etc, "VIBECHAT_EGRESS_STATE="+state)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
}

func TestEgressScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the egress script runs on a Linux host; Git Bash cannot chmod")
	}
	dir, run := egressEnv(t)
	dns := func(name, ips string) {
		if err := os.WriteFile(filepath.Join(dir, "dns", name), []byte(ips), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return string(b)
	}
	dns("api2.cursor.sh", "203.0.113.10\n203.0.113.11\n")
	dns("registry.npmjs.org", "198.51.100.7\n")
	if out, err := run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	acl := read("acl.yaml")
	for _, want := range []string{
		"destination: 198.51.100.7/32,203.0.113.10/32,203.0.113.11/32\n", `destination_port: "443"`, "destination: 10.20.30.1/32", "protocol: udp", "ingress: []",
	} {
		if !strings.Contains(acl, want) {
			t.Errorf("ACL lacks %q:\n%s", want, acl)
		}
	}
	calls := read("calls")
	if !strings.Contains(calls, "network acl create vibechat-egress") ||
		!strings.Contains(calls, "network set vibechatbr0 security.acls=vibechat-egress security.acls.default.egress.action=reject") {
		t.Errorf("calls:\n%s", calls)
	}
	hosts := read("hosts.pushed")
	if !strings.Contains(hosts, "127.0.0.1 localhost\n::1 ip6-localhost\n# vibechat-egress begin") || strings.Contains(hosts, "9.9.9.9") ||
		!strings.Contains(hosts, "203.0.113.10 api2.cursor.sh\n") || !strings.HasSuffix(hosts, "# vibechat-egress end\n") {
		t.Errorf("hosts:\n%s", hosts)
	}
	if !strings.Contains(read("push.args"), "--mode 0644") {
		t.Error("/etc/hosts pushed without mode 0644")
	}
	// The CDN rotated: the previous run's addresses stay allowed one more round.
	dns("api2.cursor.sh", "203.0.113.12\n")
	if out, err := run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if acl := read("acl.yaml"); !strings.Contains(acl, "destination: 198.51.100.7/32,203.0.113.10/32,203.0.113.11/32,203.0.113.12/32\n") {
		t.Errorf("rotation:\n%s", acl)
	}
	if strings.Count(read("calls"), "acl create") != 1 {
		t.Error("the ACL was created twice")
	}
	// Nothing resolves (DNS down): the ACL stays as it was.
	_ = os.Remove(filepath.Join(dir, "dns", "api2.cursor.sh"))
	_ = os.Remove(filepath.Join(dir, "dns", "registry.npmjs.org"))
	before := read("acl.yaml")
	if _, err := run(); err == nil {
		t.Error("an empty resolution succeeded")
	}
	if read("acl.yaml") != before {
		t.Error("an empty resolution changed the ACL")
	}
}
