#!/usr/bin/env bash
# CI (GitHub ubuntu runner with KVM): the setup wizard (internal/provision) end to end against a fresh Debian 13
# Incus VM shaped like a user's VPS: sshd with root password login on (as provider images do), nothing else.
# The runner serves the job's vibechat-server .deb with a SHA256SUMS over HTTP to the VM (the wizard downloads and
# checks it there); `go test -tags e2e -run TestProvisionE2E ./internal/remote` runs the wizard twice (the
# lockdown's rollback without a confirmation, then the rerun, the confirmation and the app's connection); then this
# script checks from outside: the container's limits and nesting, an unprivileged container, the egress allowlist
# (allowed hosts reachable, others not), sshd keys-only, no rollback timer left, no Incus API on the network.
# The root password and the host token stay in 0600 files / the test's memory and are never printed.
# Usage: ci-provision-e2e.sh <vibechat-server .deb>
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
deb="$(readlink -f "$1")"
[ -e /dev/kvm ] || { echo "ci-provision-e2e: no /dev/kvm on this runner" >&2; exit 1; }
work="$(mktemp -d)"
chmod 700 "$work"
http_pid=""
trap '[ -z "$http_pid" ] || kill "$http_pid" 2>/dev/null || true; rm -rf "$work"' EXIT
sudo apt-get update -qq
sudo apt-get install -y -qq qemu-system-x86 ovmf incus >/dev/null
sudo systemctl restart incus
sudo incus admin init --minimal
sudo iptables -P FORWARD ACCEPT # Docker on the runner sets FORWARD to DROP
vm=vibechat-vps
sudo incus launch -q images:debian/13 "$vm" --vm -c limits.cpu=3 -c limits.memory=6GiB -d root,size=30GiB
in_vm() { sudo incus exec "$vm" -- "$@"; }
for _ in $(seq 1 120); do in_vm true >/dev/null 2>&1 && break; sleep 2; done
for _ in $(seq 1 60); do in_vm getent hosts deb.debian.org >/dev/null 2>&1 && break; sleep 2; done

echo "::group::A VPS: Debian 13, sshd, root password login"
in_vm env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssh-server >/dev/null
umask 077
head -c 32 /dev/urandom | base64 | tr -d '/+=\n' > "$work/rootpw"
{ printf 'root:'; cat "$work/rootpw"; echo; } | sudo incus exec "$vm" -- chpasswd
in_vm sh -c 'printf "PermitRootLogin yes\nPasswordAuthentication yes\n" > /etc/ssh/sshd_config.d/50-vps-root.conf && systemctl restart ssh'
umask 022
ip="$(in_vm hostname -I | awk '{print $1}')"
echo "ci-provision-e2e: VPS at $ip:22"
echo "::endgroup::"

# The release assets as the VM sees them: the .deb and its SHA256SUMS on the runner's bridge address.
gw="$(ip -4 -o addr show incusbr0 | awk '{print $4}' | cut -d/ -f1)"
mkdir -p "$work/www"
cp "$deb" "$work/www/vibechat-server-linux-amd64.deb"
(cd "$work/www" && sha256sum vibechat-server-linux-amd64.deb > SHA256SUMS)
python3 -m http.server --bind "$gw" --directory "$work/www" 8099 >/dev/null 2>&1 &
http_pid=$!
for _ in $(seq 1 50); do in_vm curl -fsS -o /dev/null "http://$gw:8099/SHA256SUMS" 2>/dev/null && break; sleep 0.2; done

echo "::group::The wizard, twice (go test -tags e2e -run TestProvisionE2E)"
VIBECHAT_E2E_PROVISION_ADDR="$ip:22" VIBECHAT_E2E_ROOT_PASSWORD_FILE="$work/rootpw" \
	VIBECHAT_E2E_DEB_URL="http://$gw:8099/vibechat-server-linux-amd64.deb" VIBECHAT_E2E_SUMS_URL="http://$gw:8099/SHA256SUMS" \
	go test -tags e2e -count=1 -timeout 50m -run TestProvisionE2E -v ./internal/remote
echo "::endgroup::"

echo "::group::Checks from outside"
c=vibechat-owner
ct() { sudo incus exec "$vm" -- incus exec "$c" -- "$@"; }
fail() { echo "ci-provision-e2e: $*" >&2; exit 1; }
[ "$(ct nproc)" = 2 ] || fail "limits.cpu: $(ct nproc) CPUs"
[ "$(ct cat /sys/fs/cgroup/memory.max)" = 4294967296 ] || fail "limits.memory: $(ct cat /sys/fs/cgroup/memory.max)"
expanded="$(in_vm incus config show "$c" --expanded)"
grep -q 'size: 15GiB' <<<"$expanded" || fail "root size"
grep -q 'security.nesting: "true"' <<<"$expanded" || fail "security.nesting"
! grep -q 'security.privileged: "true"' <<<"$expanded" || fail "a privileged container"
[ "$(ct awk 'NR == 1 {print $2}' /proc/self/uid_map)" != 0 ] || fail "root in the container is root on the host"
echo "limits: 2 CPU, 4 GiB, 15 GiB, nesting, unprivileged: ok"
reach() { ct curl -sS -o /dev/null -m 20 "$1"; }
for u in https://registry.npmjs.org/ https://api2.cursor.sh/ https://bun.sh/ https://github.com/; do
	reach "$u" || fail "egress: $u is blocked"
	echo "egress: $u allowed"
done
for u in https://example.com/ https://1.1.1.1/ https://deb.debian.org/ http://registry.npmjs.org/; do
	if reach "$u" 2>/dev/null; then fail "egress: $u is reachable"; fi
	echo "egress: $u blocked"
done
in_vm systemctl is-enabled -q vibechat-egress.timer || fail "vibechat-egress.timer"
in_vm systemctl start vibechat-egress.service || fail "the egress refresh fails"
reach https://registry.npmjs.org/ || fail "egress after a refresh"
eff="$(in_vm /usr/sbin/sshd -T -C user=root,host=ci,addr=192.0.2.1)"
for kv in "passwordauthentication no" "kbdinteractiveauthentication no" "permitrootlogin without-password"; do
	grep -qx "$kv" <<<"$eff" || fail "sshd: not $kv"
done
if in_vm systemctl is-active -q vibechat-sshd-rollback.timer; then fail "the rollback timer is still armed"; fi
[ -z "$(in_vm incus config get core.https_address)" ] || fail "the Incus API listens on the network"
[ "$(in_vm getent passwd vibechat-tunnel | cut -d: -f7)" = /usr/sbin/nologin ] || fail "the tunnel user has a shell"
in_vm incus snapshot show "$c" provisioned >/dev/null || fail "no snapshot provisioned"
in_vm cat /var/lib/vibechat-provision/state.json
echo "ci-provision-e2e: ok"
echo "::endgroup::"
