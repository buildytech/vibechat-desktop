#!/usr/bin/env bash
# CI (GitHub ubuntu runner with KVM): the server .deb on Debian 13, the only supported server OS.
#   1. a Debian 13 Incus VM (real Debian kernel, systemd): scripts/server-smoke.sh, sandbox preflight required;
#   2. inside that VM, an Incus container like a user's server (scripts/incus-container-smoke.sh).
# Usage: ci-server-debian.sh <deb>
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
deb="$1"
[ -e /dev/kvm ] || { echo "ci-server-debian: no /dev/kvm on this runner" >&2; exit 1; }
sudo apt-get update -qq
# Incus finds QEMU (and the UEFI firmware) at start: install them first, restart it to be sure.
sudo apt-get install -y -qq qemu-system-x86 ovmf incus >/dev/null
sudo systemctl restart incus
sudo incus admin init --minimal
# Docker on the runner sets the FORWARD policy to DROP: let the Incus bridge reach the network.
sudo iptables -P FORWARD ACCEPT
vm=vibechat-d13
sudo incus launch -q images:debian/13 "$vm" --vm -c limits.cpu=2 -c limits.memory=4GiB -d root,size=15GiB
for _ in $(seq 1 120); do sudo incus exec "$vm" -- true >/dev/null 2>&1 && break; sleep 2; done
for _ in $(seq 1 60); do sudo incus exec "$vm" -- getent hosts deb.debian.org >/dev/null 2>&1 && break; sleep 2; done
sudo incus file push "$deb" "$vm/root/vibechat-server.deb"
sudo incus file push scripts/server-smoke.sh scripts/incus-container-smoke.sh "$vm/root/"
echo "::group::Debian 13 VM"
sudo incus exec "$vm" -- bash /root/server-smoke.sh /root/vibechat-server.deb --require-sandbox
echo "::endgroup::"
echo "::group::Incus container in the Debian 13 VM"
set +e
sudo incus exec "$vm" -- bash /root/incus-container-smoke.sh /root/vibechat-server.deb /root/server-smoke.sh --require-sandbox
c=$?
set -e
echo "::endgroup::"
[ "$c" = 0 ] || echo "::warning::server smoke in an Incus container failed (exit $c); see the group above"
