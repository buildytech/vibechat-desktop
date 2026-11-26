package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	nameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	keyLineRe = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/=]{16,}( [ -~]{0,200})?$`)
	urlRe     = regexp.MustCompile(`^https?://[A-Za-z0-9.:\[\]-]+(/[A-Za-z0-9._~%/+-]*)?$`)
	domainRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
)

// Fixed names on the host and in the container.
const (
	Network      = "vibechatbr0"
	Profile      = "vibechat"
	Pool         = "vibechat"
	ACL          = "vibechat-egress"
	TunnelUser   = "vibechat-tunnel"
	StateDir     = "/var/lib/vibechat-provision"
	SSHDropIn    = "/etc/ssh/sshd_config.d/00-vibechat.conf"
	RollbackUnit = "vibechat-sshd-rollback"
	Snapshot     = "provisioned"
	APIPort      = 18120
	PreviewPort  = 18121
)

// Bun for previews in the container: the runtime pinned in bridge.lock.json (baselineRuntime, linux-x64).
const (
	BunVersion = "1.3.9"
	BunURL     = "https://github.com/oven-sh/bun/releases/download/bun-v1.3.9/bun-linux-x64-baseline.zip"
	BunSHA256  = "104d4d037f4b35e10215c0507e1779691f39c57bd91ddeefe11cad781e3fc4b9"
)

// Limits of the user's container.
const (
	LimitCPU    = "2"
	LimitMemory = "4GiB"
	LimitDisk   = "15GiB"
)

// DefaultDomains is the egress allowlist: Cursor (the agent's API hosts from cursor.com/docs/enterprise/
// network-configuration, no wildcards), npm (registry), Bun and GitHub (Bun releases, git over https, vibechat-server
// updates from GitHub releases). TCP 443 only.
var DefaultDomains = []string{
	// Cursor
	"api2.cursor.sh", "api3.cursor.sh", "api4.cursor.sh", "api5.cursor.sh", "repo42.cursor.sh",
	"agent.api5.cursor.sh", "agentn.api5.cursor.sh", "agent.us.api5.cursor.sh", "agentn.us.api5.cursor.sh",
	"agent.global.api5.cursor.sh", "agentn.global.api5.cursor.sh",
	"authenticate.cursor.sh", "authentication.cursor.sh", "prod.authentication.cursor.sh",
	// npm
	"registry.npmjs.org",
	// Bun
	"bun.sh",
	// GitHub (Bun releases, git, vibechat-server updates)
	"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com",
}

// quote is a shell single-quoted word.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// script is a step's bash: the marker line (step and phase, also read by the tests' fake host), strict mode, the
// variables, the body.
func script(id, phase string, vars map[string]string, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# vibechat-step:%s:%s\nset -euo pipefail\nexport DEBIAN_FRONTEND=noninteractive LC_ALL=C\n", id, phase)
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, quote(vars[k]))
	}
	b.WriteString(strings.TrimLeft(body, "\n"))
	return b.String()
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (w *wizard) vars(extra map[string]string) map[string]string {
	v := map[string]string{"C": w.cfg.Container, "NET": Network, "PROFILE": Profile, "POOL": Pool, "ACL": ACL, "STATE": StateDir}
	for k, x := range extra {
		v[k] = x
	}
	return v
}

func (w *wizard) steps() []step {
	return []step{
		{id: StepPreflight, check: w.preflight},
		{id: StepIncus, check: w.checkOf(StepIncus, incusCheck, nil), apply: w.applyOf(StepIncus, incusApply, nil)},
		{id: StepContainer, check: w.checkOf(StepContainer, containerCheck, nil), apply: w.applyOf(StepContainer, containerApply, nil)},
		{id: StepServer, check: w.checkOf(StepServer, serverCheck, w.serverVars()), apply: w.applyOf(StepServer, serverApply, w.serverVars())},
		{id: StepEgress, check: w.checkOf(StepEgress, egressCheck, w.egressVars()), apply: w.applyOf(StepEgress, egressApply(w.egressFiles()), w.egressVars())},
		{id: StepToken, check: w.tokenCheck, apply: w.tokenApply},
		{id: StepTunnel, check: w.checkOf(StepTunnel, tunnelCheck, w.tunnelVars()), apply: w.applyOf(StepTunnel, tunnelApply, w.tunnelVars())},
		{id: StepVerify, check: w.verify},
		{id: StepLockdown, check: w.checkOf(StepLockdown, lockdownCheck, w.lockdownVars()), apply: w.lockdown},
	}
}

func (w *wizard) checkOf(id, body string, extra map[string]string) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		return w.checkScript(ctx, script(id, "check", w.vars(extra), body), nil)
	}
}

func (w *wizard) applyOf(id, body string, extra map[string]string) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := w.sh(ctx, script(id, "apply", w.vars(extra), body), nil)
		return err
	}
}

// ---- preflight

const preflightBody = `
refuse() { echo "refuse: $*" >&2; exit 4; }
[ "$(id -u)" = 0 ] || refuse "the setup needs root"
[ -r /etc/os-release ] || refuse "no /etc/os-release: Debian 13 is required"
. /etc/os-release
[ "${ID:-}" = debian ] && [ "${VERSION_ID:-}" = 13 ] || refuse "Debian 13 is required, this server runs ${PRETTY_NAME:-an unknown system}"
arch="$(dpkg --print-architecture)"
[ "$arch" = amd64 ] || refuse "an amd64 (x86_64) server is required, this one is $arch"
[ -d /run/systemd/system ] || refuse "systemd is required"
mem="$(awk '/^MemTotal:/ {print int($2 / 1024)}' /proc/meminfo)"
[ "$mem" -ge 1800 ] || refuse "at least 2 GiB of memory is required, this server has $mem MiB"
disk="$(df -Pm /var/lib | awk 'NR == 2 {print $4}')"
[ "$disk" -ge 18432 ] || refuse "at least 18 GiB free on /var/lib is required (the container's 15 GiB disk lives in an 18 GiB pool file), this server has $disk MiB"
echo "preflight: $PRETTY_NAME, $arch, $mem MiB memory, $disk MiB free"
`

func (w *wizard) preflight(ctx context.Context) (bool, error) {
	res, err := w.r.Run(ctx, script(StepPreflight, "check", nil, preflightBody), nil)
	if err != nil {
		return false, err
	}
	switch res.Code {
	case 0:
		return true, nil
	case refuseExit:
		msg := strings.TrimSpace(strings.TrimPrefix(lastLine(res.Stderr), "refuse:"))
		return false, &Error{Code: ProblemUnsupported, Message: msg}
	}
	return false, fmt.Errorf("exit %d: %s", res.Code, tail(res.Stderr))
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

// ---- incus

const incusCheck = `
for c in incus curl unzip sha256sum mkfs.btrfs; do command -v "$c" >/dev/null 2>&1 || exit 3; done
incus storage list -f csv 2>/dev/null | grep -q . || exit 3
incus profile device get default root pool >/dev/null 2>&1 || exit 3
`

const incusApply = `
apt-get update -qq
apt-get install -y -qq incus curl ca-certificates unzip btrfs-progs >/dev/null
systemctl start incus.socket >/dev/null 2>&1 || true
# --minimal: a local dir pool and incusbr0, no network listener (the app never gets the Incus API).
incus storage list -f csv | grep -q . || incus admin init --minimal
`

// ---- container

const containerCheck = `
[ "$(incus network get "$NET" ipv4.nat 2>/dev/null)" = true ] || exit 3
[ "$(incus network get "$NET" ipv6.address 2>/dev/null)" = none ] || exit 3
p() { incus profile get "$PROFILE" "$1" 2>/dev/null; }
d() { incus profile device get "$PROFILE" "$1" "$2" 2>/dev/null; }
[ "$(p limits.cpu)" = 2 ] && [ "$(p limits.memory)" = 4GiB ] && [ "$(p security.nesting)" = true ] || exit 3
[ "$(p security.privileged)" != true ] || exit 3
[ "$(d root size)" = 15GiB ] && [ "$(d root pool)" = "$POOL" ] && [ "$(d root path)" = / ] && [ "$(d eth0 network)" = "$NET" ] || exit 3
[ "$(d api listen)" = tcp:127.0.0.1:18120 ] && [ "$(d api connect)" = tcp:127.0.0.1:18120 ] || exit 3
[ "$(d preview listen)" = tcp:127.0.0.1:18121 ] && [ "$(d preview connect)" = tcp:127.0.0.1:18121 ] || exit 3
incus info "$C" >/dev/null 2>&1 || exit 3
[ "$(incus info "$C" | awk '/^Status:/ {print $2}')" = RUNNING ] || exit 3
incus config show "$C" | grep -qx -- "- $PROFILE" || exit 3
[ "$(incus config get "$C" security.privileged)" != true ] || exit 3
`

const containerApply = `
# A dir pool (incus admin init --minimal) cannot enforce size=15GiB; a loop-file btrfs pool can (qgroups).
incus storage show "$POOL" >/dev/null 2>&1 || incus storage create "$POOL" btrfs size=18GiB
incus network show "$NET" >/dev/null 2>&1 || incus network create "$NET" ipv4.address=auto ipv4.nat=true ipv6.address=none
incus network set "$NET" ipv4.nat=true ipv6.address=none
incus profile show "$PROFILE" >/dev/null 2>&1 || incus profile create "$PROFILE"
incus profile set "$PROFILE" limits.cpu=2 limits.memory=4GiB security.nesting=true
incus profile unset "$PROFILE" security.privileged >/dev/null 2>&1 || true
dev() {
	local name="$1" type="$2"
	shift 2
	if incus profile device get "$PROFILE" "$name" type >/dev/null 2>&1; then
		incus profile device set "$PROFILE" "$name" "$@"
	else
		incus profile device add "$PROFILE" "$name" "$type" "$@"
	fi
}
dev root disk path=/ pool="$POOL" size=15GiB
dev eth0 nic network="$NET" name=eth0
# The host's loopback ports into the container's loopback (sshd's permitopen for the tunnel user).
dev api proxy listen=tcp:127.0.0.1:18120 connect=tcp:127.0.0.1:18120 bind=host
dev preview proxy listen=tcp:127.0.0.1:18121 connect=tcp:127.0.0.1:18121 bind=host
incus info "$C" >/dev/null 2>&1 || incus launch images:debian/13 "$C" -p "$PROFILE"
[ "$(incus info "$C" | awk '/^Status:/ {print $2}')" = RUNNING ] || incus start "$C"
for _ in $(seq 1 90); do incus exec "$C" -- getent hosts deb.debian.org >/dev/null 2>&1 </dev/null && break; sleep 2; done
`

// ---- server

func (w *wizard) serverVars() map[string]string {
	return map[string]string{"DEB_URL": w.cfg.DebURL, "SUMS_URL": w.cfg.SumsURL, "BUN_URL": BunURL, "BUN_SHA": BunSHA256, "BUN_VERSION": BunVersion}
}

const sumsLine = `awk '$2 ~ /^\*?vibechat-server-linux-amd64\.deb$/' "$work/SHA256SUMS"`

const serverCheck = `
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
curl -fsSL --retry 3 -o "$work/SHA256SUMS" "$SUMS_URL"
want="$(` + sumsLine + ` | awk '{print $1}')"
[ -n "$want" ] || { echo "SHA256SUMS does not list vibechat-server-linux-amd64.deb" >&2; exit 1; }
[ "$(cat "$STATE/server.sha256" 2>/dev/null)" = "$want" ] || exit 3
incus exec "$C" -- dpkg-query -W -f '${Status}' vibechat-server 2>/dev/null </dev/null | grep -q 'install ok installed' || exit 3
for c in curl bwrap; do incus exec "$C" -- sh -c "command -v $c" >/dev/null 2>&1 </dev/null || exit 3; done
[ "$(incus exec "$C" -- /var/lib/vibechat/.bun/bin/bun --version 2>/dev/null </dev/null)" = "$BUN_VERSION" ] || exit 3
`

const serverApply = `
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
curl -fsSL --retry 3 -o "$work/SHA256SUMS" "$SUMS_URL"
curl -fsSL --retry 3 -o "$work/vibechat-server-linux-amd64.deb" "$DEB_URL"
` + sumsLine + ` > "$work/want"
[ -s "$work/want" ] || { echo "SHA256SUMS does not list vibechat-server-linux-amd64.deb" >&2; exit 1; }
(cd "$work" && sha256sum -c --strict --quiet want) || { echo "the server package does not match SHA256SUMS" >&2; exit 1; }
curl -fsSL --retry 3 -o "$work/bun.zip" "$BUN_URL"
echo "$BUN_SHA  $work/bun.zip" | sha256sum -c --strict --quiet - || { echo "Bun does not match its pinned SHA-256" >&2; exit 1; }
unzip -q -o "$work/bun.zip" -d "$work/bun"
ct() { incus exec "$C" -- "$@" </dev/null; }
ct apt-get update -qq
ct env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates unzip bubblewrap >/dev/null
incus file push "$work/vibechat-server-linux-amd64.deb" "$C/root/vibechat-server.deb"
ct env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --reinstall /root/vibechat-server.deb >/dev/null
ct rm -f /root/vibechat-server.deb
ct install -d -o vibechat -g vibechat -m 755 /var/lib/vibechat/.bun /var/lib/vibechat/.bun/bin
incus file push --mode 0755 "$work/bun/bun-linux-x64-baseline/bun" "$C/var/lib/vibechat/.bun/bin/bun"
ct chown vibechat:vibechat /var/lib/vibechat/.bun/bin/bun
ct ln -sfn bun /var/lib/vibechat/.bun/bin/bunx
install -d -m 700 "$STATE"
awk '{print $1}' "$work/want" > "$STATE/server.sha256"
`

// ---- egress

func (w *wizard) egressFiles() map[string]string {
	conf := fmt.Sprintf("CONTAINER=%s\nNETWORK=%s\nACL=%s\n", w.cfg.Container, Network, ACL)
	return map[string]string{
		"/etc/vibechat/egress.conf":                   conf,
		"/etc/vibechat/egress.domains":                "# The container's egress allowlist (TCP 443), one name per line; vibechat-egress resolves them.\n" + strings.Join(w.cfg.Domains, "\n") + "\n",
		"/usr/local/sbin/vibechat-egress":             egressScript,
		"/etc/systemd/system/vibechat-egress.service": egressService,
		"/etc/systemd/system/vibechat-egress.timer":   egressTimer,
	}
}

func (w *wizard) egressVars() map[string]string {
	files := w.egressFiles()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var sums []string
	for _, p := range paths {
		sums = append(sums, sha(files[p])+"  "+p)
	}
	return map[string]string{"SUMS": strings.Join(sums, "\n") + "\n"}
}

const egressCheck = `
printf '%s' "$SUMS" | sha256sum -c --strict --quiet - >/dev/null 2>&1 || exit 3
systemctl is-enabled -q vibechat-egress.timer || exit 3
[ "$(incus network get "$NET" security.acls)" = "$ACL" ] || exit 3
[ "$(incus network get "$NET" security.acls.default.egress.action)" = reject ] || exit 3
incus network acl show "$ACL" 2>/dev/null | grep -q 'destination_port:.*443' || exit 3
incus exec "$C" -- grep -q '^# vibechat-egress begin' /etc/hosts </dev/null || exit 3
`

func egressApply(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString("install -d -m 755 /etc/vibechat\n")
	for i, p := range paths {
		mode := "644"
		if strings.HasPrefix(p, "/usr/local/sbin/") {
			mode = "755"
		}
		marker := "VIBECHAT_FILE_" + strconv.Itoa(i)
		fmt.Fprintf(&b, "cat > %s.tmp <<'%s'\n%s%s\nchmod %s %s.tmp && mv %s.tmp %s\n", p, marker, files[p], marker, mode, p, p, p)
	}
	b.WriteString("systemctl daemon-reload\nsystemctl enable -q --now vibechat-egress.timer\n/usr/local/sbin/vibechat-egress\n")
	return b.String()
}

// egressScript runs on the host (the timer, and once from the wizard): resolve the allowlist, write the ACL, pin
// the names in the container's /etc/hosts.
const egressScript = `#!/bin/bash
# vibechat-egress (installed by the VibeChat setup wizard): the egress allowlist of the VibeChat container.
# Incus network ACLs take addresses, not names: the names in /etc/vibechat/egress.domains are resolved here (IPv4,
# TCP 443), the ACL on the container's bridge allows exactly those addresses (and the previous run's, as CDN answers
# rotate) and rejects everything else, and the container's /etc/hosts pins the names to the same addresses, so its
# answers always match the ACL. vibechat-egress.timer runs this every 10 minutes.
set -euo pipefail
etc="${VIBECHAT_EGRESS_ETC:-/etc/vibechat}"
state="${VIBECHAT_EGRESS_STATE:-/var/lib/vibechat-provision}"
. "$etc/egress.conf"
install -d -m 700 "$state"
cur="$(mktemp)" hosts="$(mktemp)"
trap 'rm -f "$cur" "$hosts" "$hosts.new"' EXIT
while read -r d; do
	case "$d" in '' | '#'*) continue ;; esac
	for ip in $(getent ahostsv4 "$d" | awk '{print $1}' | sort -u); do
		echo "$ip" >> "$cur"
		echo "$ip $d" >> "$hosts"
	done
done < "$etc/egress.domains"
if [ ! -s "$cur" ]; then
	echo "vibechat-egress: no name resolved; the allowlist stays as it was" >&2
	exit 1
fi
touch "$state/egress.prev"
all="$(sort -u "$cur" "$state/egress.prev" | grep -E '^[0-9]+(\.[0-9]+){3}$' | sed 's|$|/32|' | paste -sd, -)"
sort -u "$cur" > "$state/egress.prev"
gw="$(incus network get "$NETWORK" ipv4.address | cut -d/ -f1)"
incus network acl show "$ACL" >/dev/null 2>&1 || incus network acl create "$ACL"
incus network acl edit "$ACL" <<EOF
name: $ACL
description: VibeChat container egress allowlist (vibechat-egress)
egress:
- action: allow
  protocol: tcp
  destination: $all
  destination_port: "443"
  description: Cursor, npm, Bun and GitHub
  state: enabled
- action: allow
  protocol: udp
  destination: $gw/32
  destination_port: "53"
  description: DNS of the bridge
  state: enabled
- action: allow
  protocol: tcp
  destination: $gw/32
  destination_port: "53"
  description: DNS of the bridge
  state: enabled
ingress: []
EOF
incus network set "$NETWORK" security.acls="$ACL" security.acls.default.egress.action=reject security.acls.default.ingress.action=allow
if [ "$(incus info "$CONTAINER" 2>/dev/null | awk '/^Status:/ {print $2}')" = RUNNING ]; then
	{
		incus exec "$CONTAINER" -- sed '/^# vibechat-egress begin/,/^# vibechat-egress end/d' /etc/hosts </dev/null
		echo "# vibechat-egress begin (written by vibechat-egress on the host)"
		sort -u "$hosts"
		echo "# vibechat-egress end"
	} > "$hosts.new"
	incus file push --uid 0 --gid 0 --mode 0644 "$hosts.new" "$CONTAINER/etc/hosts"
fi
echo "vibechat-egress: $(wc -l < "$cur") addresses for $(grep -cvE '^(#|$)' "$etc/egress.domains") names"
`

const egressService = `[Unit]
Description=Refresh the egress allowlist of the VibeChat container
After=network-online.target incus.service
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/vibechat-egress
`

const egressTimer = `[Unit]
Description=Refresh the egress allowlist of the VibeChat container every 10 minutes

[Timer]
OnBootSec=1min
OnUnitActiveSec=10min

[Install]
WantedBy=timers.target
`

// ---- token

const tokenCheck = `
hdr="$(cat)"
incus exec "$C" -- systemctl is-active -q vibechat-server.service </dev/null || exit 3
code="$(printf '%s\n' "$hdr" | incus exec "$C" -- curl -s -o /dev/null -w '%{http_code}' -X POST -H @- http://127.0.0.1:18120/runtime/v1/auth/ticket)" || exit 3
case "$code" in 2??) exit 0 ;; esac
exit 3
`

// tokenApply's stdout is the token and nothing else.
const tokenApply = `
incus exec "$C" -- runuser -u vibechat -- env VIBECHAT_DATA_DIR=/var/lib/vibechat vibechat-server token new 2>/dev/null </dev/null
incus exec "$C" -- systemctl enable -q vibechat-server.service >/dev/null 2>&1 </dev/null || true
incus exec "$C" -- systemctl restart vibechat-server.service >/dev/null </dev/null
for _ in $(seq 1 100); do
	incus exec "$C" -- curl -fsS -o /dev/null http://127.0.0.1:18120/runtime/v1/host/hello >/dev/null 2>&1 </dev/null && exit 0
	sleep 0.3
done
echo "vibechat-server does not answer after the restart" >&2
exit 1
`

func (w *wizard) tokenCheck(ctx context.Context) (bool, error) {
	tok := strings.TrimSpace(w.h.TokenGet())
	if tok == "" {
		return false, nil
	}
	return w.checkScript(ctx, script(StepToken, "check", w.vars(nil), tokenCheck), []byte("Authorization: Bearer "+tok+"\n"))
}

func (w *wizard) tokenApply(ctx context.Context) error {
	res, err := w.r.Run(ctx, script(StepToken, "apply", w.vars(nil), tokenApply), nil)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("exit %d: %s", res.Code, tail(res.Stderr)) // stderr only: stdout may hold the token
	}
	tok := strings.TrimSpace(string(res.Stdout))
	if tok == "" || len(tok) > 256 || strings.ContainsAny(tok, " \n\r\t") {
		return errors.New("vibechat-server token new did not print one token")
	}
	if err := w.h.TokenSet(tok); err != nil {
		return fmt.Errorf("the host token was not saved in the system keychain: %v", err)
	}
	return nil
}

// ---- tunnel

// TunnelKeyLine is the tunnel user's authorized_keys line for the app's key: port forwarding to the two ports only.
func TunnelKeyLine(appKey string) string {
	return fmt.Sprintf(`restrict,port-forwarding,permitopen="127.0.0.1:%d",permitopen="127.0.0.1:%d" %s`, APIPort, PreviewPort, appKey)
}

func (w *wizard) tunnelVars() map[string]string {
	return map[string]string{"U": TunnelUser, "H": "/home/" + TunnelUser, "LINE": TunnelKeyLine(w.cfg.AppKey)}
}

const tunnelCheck = `
K="$H/.ssh/authorized_keys"
id -u "$U" >/dev/null 2>&1 || exit 3
[ "$(getent passwd "$U" | cut -d: -f7)" = /usr/sbin/nologin ] || exit 3
grep -qxF "$LINE" "$K" 2>/dev/null || exit 3
[ "$(stat -c '%U %a' "$K")" = "$U 600" ] && [ "$(stat -c '%U %a' "$H/.ssh")" = "$U 700" ] || exit 3
# Every key of the tunnel user is restricted to port forwarding.
if grep -v '^restrict,port-forwarding,permitopen=' "$K" | grep -q .; then exit 3; fi
`

const tunnelApply = `
K="$H/.ssh/authorized_keys"
id -u "$U" >/dev/null 2>&1 || adduser --disabled-password --gecos "" --home "$H" --shell /usr/sbin/nologin "$U" >/dev/null
usermod -s /usr/sbin/nologin "$U"
install -d -m 700 -o "$U" -g "$U" "$H/.ssh"
touch "$K"
grep -qxF "$LINE" "$K" || printf '%s\n' "$LINE" >> "$K"
grep '^restrict,port-forwarding,permitopen=' "$K" > "$K.new" || true
mv "$K.new" "$K"
chown "$U:$U" "$K"
chmod 600 "$K"
`

// ---- verify

const doctorBody = `
incus exec "$C" -- runuser -u vibechat -- env VIBECHAT_DATA_DIR=/var/lib/vibechat vibechat-server doctor </dev/null
`

const snapshotBody = `
incus snapshot show "$C" "$SNAP" >/dev/null 2>&1 || incus snapshot create "$C" "$SNAP"
`

func (w *wizard) verify(ctx context.Context) (bool, error) {
	res, err := w.r.Run(ctx, script(StepVerify, "doctor", w.vars(nil), doctorBody), nil)
	if err != nil {
		return false, err
	}
	if res.Code != 0 {
		return false, fmt.Errorf("vibechat-server doctor: %s", tail(append(res.Stdout, res.Stderr...)))
	}
	if err := w.h.Verify(ctx); err != nil {
		return false, fmt.Errorf("the app's key login to the server does not work: %v", err)
	}
	if _, err := w.sh(ctx, script(StepVerify, "snapshot", w.vars(map[string]string{"SNAP": Snapshot}), snapshotBody), nil); err != nil {
		return false, fmt.Errorf("snapshot: %v", err)
	}
	return true, nil
}

// ---- lockdown

// SSHDropIn's content. 00- sorts first: sshd takes the first value of a keyword, Debian includes
// sshd_config.d/*.conf at the top of sshd_config, and cloud images add 50-cloud-init.conf with password login on.
const dropIn = `# VibeChat setup: SSH keys only (root too). Remove this file to allow password login again.
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
`

func (w *wizard) lockdownVars() map[string]string {
	return map[string]string{
		"F": SSHDropIn, "WANT": sha(dropIn), "UNIT": RollbackUnit, "ROOT_KEY": w.cfg.RootKey,
		"SECS": strconv.Itoa(int(w.cfg.RollbackAfter / time.Second)),
	}
}

const sshdEffective = `
eff="$(/usr/sbin/sshd -T -C user=root,host=vibechat-check,addr=192.0.2.1 2>/dev/null)"
for kv in "passwordauthentication no" "kbdinteractiveauthentication no" "permitrootlogin without-password"; do
	printf '%s\n' "$eff" | grep -qx "$kv" || { echo "sshd does not take: $kv" >&2; exit 3; }
done
`

const lockdownCheck = `
[ "$(sha256sum "$F" 2>/dev/null | awk '{print $1}')" = "$WANT" ] || exit 3
if systemctl is-active -q "$UNIT.timer"; then exit 3; fi
if [ -n "$ROOT_KEY" ]; then grep -qxF "$ROOT_KEY" /root/.ssh/authorized_keys 2>/dev/null || exit 3; fi
` + sshdEffective

// lockdownArm: the safety net first. When the app does not confirm, the timer removes the drop-in and reloads sshd.
const lockdownArm = `
systemctl stop "$UNIT.timer" >/dev/null 2>&1 || true
systemctl reset-failed "$UNIT.service" "$UNIT.timer" >/dev/null 2>&1 || true
systemd-run --quiet --unit="$UNIT" --on-active="${SECS}s" --description="VibeChat setup: allow SSH password login again (not confirmed)" \
	/bin/sh -c "rm -f '$F'; if systemctl is-active -q ssh.service; then systemctl reload ssh.service; fi"
systemctl is-active -q "$UNIT.timer"
`

func lockdownApplyBody() string {
	return `
if [ -n "$ROOT_KEY" ]; then
	install -d -m 700 /root/.ssh
	touch /root/.ssh/authorized_keys
	chmod 600 /root/.ssh/authorized_keys
	grep -qxF "$ROOT_KEY" /root/.ssh/authorized_keys || printf '%s\n' "$ROOT_KEY" >> /root/.ssh/authorized_keys
fi
umask 022
cat > "$F.tmp" <<'VIBECHAT_DROPIN'
` + dropIn + `VIBECHAT_DROPIN
mv "$F.tmp" "$F"
undo() { rm -f "$F"; systemctl stop "$UNIT.timer" >/dev/null 2>&1 || true; }
if ! /usr/sbin/sshd -t; then undo; echo "sshd -t refused the configuration; nothing changed" >&2; exit 1; fi
( ` + strings.ReplaceAll(strings.TrimSpace(sshdEffective), "exit 3", "exit 5") + ` ) || { undo; echo "another sshd setting wins over $F; nothing changed" >&2; exit 1; }
if systemctl is-active -q ssh.service; then systemctl reload ssh.service; fi
`
}

const lockdownCancel = `
systemctl stop "$UNIT.timer" >/dev/null 2>&1 || true
systemctl reset-failed "$UNIT.service" "$UNIT.timer" >/dev/null 2>&1 || true
if systemctl is-active -q "$UNIT.timer"; then echo "the rollback timer is still active" >&2; exit 1; fi
`

func (w *wizard) lockdown(ctx context.Context) error {
	v := w.vars(w.lockdownVars())
	if _, err := w.sh(ctx, script(StepLockdown, "arm", v, lockdownArm), nil); err != nil {
		return fmt.Errorf("the rollback timer: %v", err)
	}
	if _, err := w.sh(ctx, script(StepLockdown, "apply", v, lockdownApplyBody()), nil); err != nil {
		return err
	}
	if err := w.h.Confirm(ctx); err != nil {
		return &Error{Code: ProblemNotConfirmed, Message: fmt.Sprintf("the key login after the change was not confirmed (%v); password login comes back by itself within %s", err, w.cfg.RollbackAfter)}
	}
	if _, err := w.sh(ctx, script(StepLockdown, "cancel", v, lockdownCancel), nil); err != nil {
		return fmt.Errorf("cancelling the rollback timer: %v", err)
	}
	return nil
}

// ---- state

// record writes the steps done so far to StateDir/state.json on the host (for people; the checks decide).
func (w *wizard) record(ctx context.Context, done map[string]string) {
	raw, _ := json.MarshalIndent(map[string]any{"container": w.cfg.Container, "steps": done}, "", "  ")
	body := `install -d -m 700 "$STATE"
cat > "$STATE/state.json.tmp" && mv "$STATE/state.json.tmp" "$STATE/state.json"
`
	if _, err := w.sh(ctx, script("state", "write", w.vars(nil), body), append(raw, '\n')); err != nil {
		w.h.Log("provision: state.json: " + err.Error())
	}
}
