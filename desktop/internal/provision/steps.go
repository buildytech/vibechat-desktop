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

	"github.com/buildytech/vibechat-desktop/internal/gopin"
)

var (
	nameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	keyLineRe = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/=]{16,}( [ -~]{0,200})?$`)
	urlRe     = regexp.MustCompile(`^https?://[A-Za-z0-9.:\[\]-]+(/[A-Za-z0-9._~%/+-]*)?$`)
	domainRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
)

// Fixed names on the host and in the container.
const (
	Network        = "vibechatbr0"
	Profile        = "vibechat"
	Pool           = "vibechat"
	ACL            = "vibechat-egress"
	TunnelUser     = "vibechat-tunnel"
	StateDir       = "/var/lib/vibechat-provision"
	SSHDropIn      = "/etc/ssh/sshd_config.d/00-vibechat.conf"
	RollbackUnit   = "vibechat-sshd-rollback"
	FirewallUnit   = "vibechat-host-firewall"
	FirewallFile   = "/etc/vibechat/host-firewall.nft"
	FirewallTable  = "vibechat_host"
	ResolvedDropIn = "/etc/systemd/resolved.conf.d/00-vibechat.conf"
	Snapshot       = "provisioned"
	APIPort        = 18120
	PreviewPort    = 18121
)

// Bun for previews in the container: the runtime pinned in bridge.lock.json (baselineRuntime, linux-x64).
const (
	BunVersion = "1.3.9"
	BunURL     = "https://github.com/oven-sh/bun/releases/download/bun-v1.3.9/bun-linux-x64-baseline.zip"
	BunSHA256  = "104d4d037f4b35e10215c0507e1779691f39c57bd91ddeefe11cad781e3fc4b9"
)

// Limits of the user's container: at most LimitCPU CPUs and LimitMemory, and on a smaller host min(LimitCPU, nproc)
// CPUs and 75% of its memory (limitsSh); a host without swap gets a SwapSize swap file first.
const (
	LimitCPU    = "2"
	LimitMemory = "4GiB"
	LimitDisk   = "15GiB"
	SwapSize    = "2G"
	SwapFile    = "/swapfile"
)

// limitsSh computes the container's limits on the host: $cpu and $mem.
const limitsSh = `
cpu="$(nproc)"
[ "$cpu" -le 2 ] || cpu=2
mem="$(awk '/^MemTotal:/ {m = int($2 / 1024 * 3 / 4); if (m >= 4096) print "4GiB"; else print m "MiB"}' /proc/meminfo)"
`

// Snapshots of the container: daily, kept a week (the "provisioned" one from the verify step stays).
const (
	SnapshotSchedule = "@daily"
	SnapshotExpiry   = "7d"
)

// DefaultDomains is the egress allowlist: Cursor (api.cursor.com, the SDK's public API the bridge calls for Me,
// ListModels and agent create, which is not on Cursor's network page; then the agent's API hosts from
// cursor.com/docs/enterprise/network-configuration; no wildcards), npm (registry), Bun and GitHub (Bun releases, git over https, vibechat-server
// updates from GitHub releases), Go (internal/gopin.Domains: modules, checksums, the toolchain) and Debian (the container's security updates over HTTPS). TCP 443 only.
var DefaultDomains = []string{
	// Cursor
	"api.cursor.com",
	"api2.cursor.sh", "api3.cursor.sh", "api4.cursor.sh", "api5.cursor.sh", "repo42.cursor.sh",
	"agent.api5.cursor.sh", "agentn.api5.cursor.sh", "agent.us.api5.cursor.sh", "agentn.us.api5.cursor.sh",
	"agent.global.api5.cursor.sh", "agentn.global.api5.cursor.sh",
	"authenticate.cursor.sh", "authentication.cursor.sh", "prod.authentication.cursor.sh",
	// npm
	"registry.npmjs.org",
	// Debian (the container's apt over HTTPS: security updates)
	"deb.debian.org",
	// Bun
	"bun.sh",
	// GitHub (Bun releases, git, vibechat-server updates, the Go templ template at its pinned commit)
	"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com",
	// Go (Go templ projects: the module proxy, the checksum database, the toolchain for ensure-go)
	"proxy.golang.org", "sum.golang.org", "dl.google.com",
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
		{id: StepIncus, check: w.checkOf(StepIncus, incusCheck, nil), apply: w.applySubsOf(StepIncus, incusApply, nil)},
		{id: StepContainer, check: w.checkOf(StepContainer, limitsSh+containerCheck, containerVars()), apply: w.applySubsOf(StepContainer, limitsSh+containerApply, containerVars())},
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
cpu="$(nproc)"
[ "$cpu" -ge 1 ] || refuse "no CPU reported by nproc"
mem="$(awk '/^MemTotal:/ {print int($2 / 1024)}' /proc/meminfo)"
[ "$mem" -ge 1800 ] || refuse "at least 2 GiB of memory is required, this server has $mem MiB"
need=18432 why="the container's 15 GiB disk lives in an 18 GiB pool file"
if [ -z "$(swapon --noheadings --show=NAME 2>/dev/null)" ] && [ ! -e /swapfile ]; then
	need=20480 why="$why, plus a 2 GiB swap file: this server has no swap"
fi
disk="$(df -Pm /var/lib | awk 'NR == 2 {print $4}')"
[ "$disk" -ge "$need" ] || refuse "at least $((need / 1024)) GiB free on /var/lib is required ($why), this server has $disk MiB"
echo "preflight: $PRETTY_NAME, $arch, $cpu CPU, $mem MiB memory, $disk MiB free"
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
sub apt-update
apt-get update -qq
sub install-packages
apt-get install -y -qq incus curl ca-certificates unzip btrfs-progs >/dev/null
systemctl start incus.socket >/dev/null 2>&1 || true
sub admin-init
# --minimal: a local dir pool and incusbr0, no network listener (the app never gets the Incus API).
incus storage list -f csv | grep -q . || incus admin init --minimal
`

// ---- container

const containerCheck = `
[ "$(incus network get "$NET" ipv4.nat 2>/dev/null)" = true ] || exit 3
[ "$(incus network get "$NET" ipv6.address 2>/dev/null)" = none ] || exit 3
p() { incus profile get "$PROFILE" "$1" 2>/dev/null; }
d() { incus profile device get "$PROFILE" "$1" "$2" 2>/dev/null; }
[ "$(p limits.cpu)" = "$cpu" ] && [ "$(p limits.memory)" = "$mem" ] && [ "$(p security.nesting)" = true ] || exit 3
[ -n "$(swapon --noheadings --show=NAME 2>/dev/null)" ] || exit 3
[ "$(incus config get "$C" snapshots.schedule 2>/dev/null)" = "$SNAP_SCHEDULE" ] || exit 3
[ "$(incus config get "$C" snapshots.expiry 2>/dev/null)" = "$SNAP_EXPIRY" ] || exit 3
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
sub storage-pool
# A dir pool (incus admin init --minimal) cannot enforce size=15GiB; a loop-file btrfs pool can (qgroups).
incus storage show "$POOL" >/dev/null 2>&1 || incus storage create "$POOL" btrfs size=18GiB
sub bridge
incus network show "$NET" >/dev/null 2>&1 || incus network create "$NET" ipv4.address=auto ipv4.nat=true ipv6.address=none
incus network set "$NET" ipv4.nat=true ipv6.address=none
sub profile
# A small host (1 CPU, 2 GiB) gets min(2, nproc) CPUs, 75% of its memory and a swap file when it has no swap.
if [ -z "$(swapon --noheadings --show=NAME 2>/dev/null)" ]; then
	if [ ! -e "$SWAPFILE" ]; then
		if [ "$(stat -f -c %T /)" = btrfs ]; then
			btrfs filesystem mkswapfile --size "$SWAPSIZE" "$SWAPFILE" >/dev/null
		else
			fallocate -l "$SWAPSIZE" "$SWAPFILE" 2>/dev/null || dd if=/dev/zero of="$SWAPFILE" bs=1M count=2048 status=none
			chmod 600 "$SWAPFILE"
			mkswap "$SWAPFILE" >/dev/null
		fi
	fi
	swapon "$SWAPFILE"
	grep -q "^$SWAPFILE " /etc/fstab || printf '%s none swap sw 0 0\n' "$SWAPFILE" >> /etc/fstab
fi
incus profile show "$PROFILE" >/dev/null 2>&1 || incus profile create "$PROFILE"
incus profile set "$PROFILE" limits.cpu="$cpu" limits.memory="$mem" security.nesting=true
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
sub launch-image
incus info "$C" >/dev/null 2>&1 || incus launch images:debian/13 "$C" -p "$PROFILE"
[ "$(incus info "$C" | awk '/^Status:/ {print $2}')" = RUNNING ] || incus start "$C"
incus config set "$C" snapshots.schedule="$SNAP_SCHEDULE" snapshots.expiry="$SNAP_EXPIRY"
sub wait-dns
for _ in $(seq 1 90); do incus exec "$C" -- getent hosts deb.debian.org >/dev/null 2>&1 </dev/null && break; sleep 2; done
`

func containerVars() map[string]string {
	return map[string]string{"SWAPFILE": SwapFile, "SWAPSIZE": SwapSize, "SNAP_SCHEDULE": SnapshotSchedule, "SNAP_EXPIRY": SnapshotExpiry}
}

// ---- server

func (w *wizard) serverVars() map[string]string {
	return map[string]string{"DEB_URL": w.cfg.DebURL, "SUMS_URL": w.cfg.SumsURL, "BUN_URL": BunURL, "BUN_SHA": BunSHA256, "BUN_VERSION": BunVersion,
		"GO_URL": gopin.URL, "GO_SHA": gopin.SHA256, "GO_VERSION": gopin.Version, "GO_DIR": gopin.Dir}
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
[ "$(incus exec "$C" -- readlink /var/lib/vibechat/.bun/bin/node 2>/dev/null </dev/null)" = bun ] || exit 3
[ "$(incus exec "$C" -- env GOTOOLCHAIN=local "$GO_DIR/go/bin/go" env GOVERSION 2>/dev/null </dev/null)" = "go$GO_VERSION" ] || exit 3
incus exec "$C" -- dpkg-query -W -f '${Status}' unattended-upgrades 2>/dev/null </dev/null | grep -q 'install ok installed' || exit 3
incus exec "$C" -- grep -q 'Unattended-Upgrade "1"' /etc/apt/apt.conf.d/20auto-upgrades </dev/null || exit 3
if incus exec "$C" -- grep -rqs 'http://deb.debian.org' /etc/apt/sources.list /etc/apt/sources.list.d </dev/null; then exit 3; fi
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
# Go for templ projects (pinned like Bun; downloaded here, so the container needs no dl.google.com for it).
curl -fsSL --retry 3 -o "$work/go.tgz" "$GO_URL"
echo "$GO_SHA  $work/go.tgz" | sha256sum -c --strict --quiet - || { echo "Go does not match its pinned SHA-256" >&2; exit 1; }
ct() { incus exec "$C" -- "$@" </dev/null; }
ct apt-get update -qq
ct env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates unzip bubblewrap >/dev/null
# Debian over HTTPS (the egress allowlist is TCP 443 only), then security updates by themselves.
ct sh -c 'for f in /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do [ -f "$f" ] && sed -i "s#http://deb.debian.org#https://deb.debian.org#g" "$f"; done; true'
ct env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq unattended-upgrades >/dev/null
ct sh -c "printf '%s\n' 'APT::Periodic::Update-Package-Lists \"1\";' 'APT::Periodic::Unattended-Upgrade \"1\";' > /etc/apt/apt.conf.d/20auto-upgrades"
# Go before the package: its postinstall (ensure-go) then finds the pinned toolchain in place.
incus file push "$work/go.tgz" "$C/root/go.tgz"
ct sh -c 'set -e; install -d -m 755 "$1"; rm -rf "$1/go.new"; install -d -m 755 "$1/go.new"; tar -xzf /root/go.tgz -C "$1/go.new" --no-same-owner; rm -rf "$1/go"; mv "$1/go.new/go" "$1/go"; rm -rf "$1/go.new" /root/go.tgz' sh "$GO_DIR"
incus file push "$work/vibechat-server-linux-amd64.deb" "$C/root/vibechat-server.deb"
ct env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --reinstall /root/vibechat-server.deb >/dev/null
ct rm -f /root/vibechat-server.deb
ct install -d -o vibechat -g vibechat -m 755 /var/lib/vibechat/.bun /var/lib/vibechat/.bun/bin
incus file push --mode 0755 "$work/bun/bun-linux-x64-baseline/bun" "$C/var/lib/vibechat/.bun/bin/bun"
ct chown vibechat:vibechat /var/lib/vibechat/.bun/bin/bun
ct ln -sfn bun /var/lib/vibechat/.bun/bin/bunx
# node → bun: the template checks' #!/usr/bin/env node tools (ui8px, ui8kit, svelte-check); the container has no Node.
ct ln -sfn bun /var/lib/vibechat/.bun/bin/node
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
# One run at a time: the timer fires as soon as the wizard enables it, while the wizard runs this too.
exec 9> "$state/egress.lock"
flock 9
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
incus network acl show "$ACL" >/dev/null 2>&1 || incus network acl create "$ACL" 2>/dev/null || incus network acl show "$ACL" >/dev/null
incus network acl edit "$ACL" <<EOF
name: $ACL
description: VibeChat container egress allowlist (vibechat-egress)
egress:
- action: allow
  protocol: tcp
  destination: $all
  destination_port: "443"
  description: Cursor, npm, Bun, GitHub and Go
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

// resolvedDropIn turns off LLMNR and mDNS of systemd-resolved (they answer on 0.0.0.0:5355 / 5353).
const resolvedDropIn = `# VibeChat setup: no LLMNR or mDNS on this server.
[Resolve]
LLMNR=no
MulticastDNS=no
`

// firewallService loads the inbound filter at boot (its own table: Incus's rules and nftables.service stay as they
// are; the rollback timer and ExecStop delete only this table).
const firewallService = `[Unit]
Description=VibeChat setup: inbound filter of this server (SSH only)
Before=network-pre.target
Wants=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f ` + FirewallFile + `
ExecStop=-/usr/sbin/nft delete table inet ` + FirewallTable + `

[Install]
WantedBy=multi-user.target
`

func (w *wizard) lockdownVars() map[string]string {
	return map[string]string{
		"F": SSHDropIn, "WANT": sha(dropIn), "UNIT": RollbackUnit, "ROOT_KEY": w.cfg.RootKey,
		"SECS":    strconv.Itoa(int(w.cfg.RollbackAfter / time.Second)),
		"FW_UNIT": FirewallUnit, "FW_FILE": FirewallFile, "FW_TABLE": FirewallTable, "FW_SERVICE": firewallService,
		"FW_SERVICE_SHA": sha(firewallService), "RES": ResolvedDropIn, "RES_BODY": resolvedDropIn, "RES_SHA": sha(resolvedDropIn),
	}
}

// firewallRules writes $FW_FILE.new: inbound on the internet-facing interfaces (the default routes') only to
// sshd's ports, ICMP and DHCP answers; established traffic passes; everything else there is dropped. The container
// bridge, loopback and forwarding are not touched (Incus keeps its own rules). $FW_FILE is compared, then moved.
const firewallRules = `
wan="$({ ip -4 route show default; ip -6 route show default; } 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") print $(i + 1)}' | sort -u | sed 's/.*/"&"/' | paste -sd, -)"
[ -n "$wan" ] || { echo "no default route: cannot tell the internet-facing interface" >&2; exit 1; }
ports="$(/usr/sbin/sshd -T 2>/dev/null | awk '$1 == "port" {print $2}' | sort -un | paste -sd, -)"
[ -n "$ports" ] || { echo "sshd -T shows no port" >&2; exit 1; }
cat > "$FW_FILE.new" <<VIBECHAT_NFT
# VibeChat setup: inbound filter of this server (vibechat-host-firewall.service). SSH ($ports) only from the
# internet; the container bridge, loopback and forwarding are not touched.
table inet $FW_TABLE
delete table inet $FW_TABLE
table inet $FW_TABLE {
	chain input {
		type filter hook input priority filter; policy accept;
		iifname { $wan } jump wan
	}
	chain wan {
		ct state established,related accept
		ct state invalid drop
		tcp dport { $ports } accept
		meta l4proto { icmp, ipv6-icmp } accept
		udp dport { 68, 546 } accept
		drop
	}
}
VIBECHAT_NFT
`

const sshdEffective = `
eff="$(/usr/sbin/sshd -T -C user=root,host=vibechat-check,addr=192.0.2.1 2>/dev/null)"
for kv in "passwordauthentication no" "kbdinteractiveauthentication no" "permitrootlogin without-password"; do
	printf '%s\n' "$eff" | grep -qx "$kv" || { echo "sshd does not take: $kv" >&2; exit 3; }
done
`

const lockdownCheck = `
[ "$(sha256sum "$F" 2>/dev/null | awk '{print $1}')" = "$WANT" ] || exit 3
command -v nft >/dev/null 2>&1 || exit 3
[ "$(sha256sum "/etc/systemd/system/$FW_UNIT.service" 2>/dev/null | awk '{print $1}')" = "$FW_SERVICE_SHA" ] || exit 3
systemctl is-enabled -q "$FW_UNIT.service" && systemctl is-active -q "$FW_UNIT.service" || exit 3
nft list table inet "$FW_TABLE" >/dev/null 2>&1 || exit 3
` + firewallRules + `
cmp -s "$FW_FILE.new" "$FW_FILE" || { rm -f "$FW_FILE.new"; exit 3; }
rm -f "$FW_FILE.new"
if systemctl is-active -q systemd-resolved; then
	[ "$(sha256sum "$RES" 2>/dev/null | awk '{print $1}')" = "$RES_SHA" ] || exit 3
fi
if systemctl is-active -q "$UNIT.timer"; then exit 3; fi
if [ -n "$ROOT_KEY" ]; then grep -qxF "$ROOT_KEY" /root/.ssh/authorized_keys 2>/dev/null || exit 3; fi
` + sshdEffective

// lockdownArm: the safety net first. When the app does not confirm, the timer removes the drop-in, reloads sshd and
// takes the inbound filter away.
const lockdownArm = `
systemctl stop "$UNIT.timer" >/dev/null 2>&1 || true
systemctl reset-failed "$UNIT.service" "$UNIT.timer" >/dev/null 2>&1 || true
systemd-run --quiet --unit="$UNIT" --on-active="${SECS}s" --description="VibeChat setup: allow SSH password login again (not confirmed)" \
	/bin/sh -c "rm -f '$F'; if systemctl is-active -q ssh.service; then systemctl reload ssh.service; fi; systemctl disable -q --now '$FW_UNIT.service' 2>/dev/null; nft delete table inet '$FW_TABLE' 2>/dev/null; true"
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
# The inbound filter: SSH only (the rollback timer armed above takes it away unless the app's new key login works).
command -v nft >/dev/null 2>&1 || apt-get install -y -qq nftables >/dev/null
install -d -m 755 /etc/vibechat
` + firewallRules + `
nft -c -f "$FW_FILE.new" || { rm -f "$FW_FILE.new"; echo "nft refused the inbound filter; nothing changed" >&2; exit 1; }
chmod 644 "$FW_FILE.new" && mv "$FW_FILE.new" "$FW_FILE"
printf '%s' "$FW_SERVICE" > "/etc/systemd/system/$FW_UNIT.service.tmp" && mv "/etc/systemd/system/$FW_UNIT.service.tmp" "/etc/systemd/system/$FW_UNIT.service"
systemctl daemon-reload
systemctl enable -q "$FW_UNIT.service"
systemctl restart "$FW_UNIT.service"
# No LLMNR / mDNS answers from systemd-resolved.
if systemctl is-active -q systemd-resolved; then
	install -d -m 755 "$(dirname "$RES")"
	printf '%s' "$RES_BODY" > "$RES.tmp" && mv "$RES.tmp" "$RES"
	systemctl restart systemd-resolved
fi
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
