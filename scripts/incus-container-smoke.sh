#!/usr/bin/env bash
# The production shape on a Debian 13 host (CI: inside the Debian 13 Incus VM), run as root: Incus from Debian, one
# container images:debian/13 with the planned limits (2 CPU, 4 GiB, 15 GiB) and security.nesting=true (user
# namespaces for the SDK sandbox's bubblewrap), then scripts/server-smoke.sh inside it.
#   incus-container-smoke.sh <deb> <server-smoke.sh> [--require-sandbox]
set -euo pipefail
deb="$(readlink -f "$1")"
smoke="$(readlink -f "$2")"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq incus >/dev/null
incus admin init --minimal
incus launch -q images:debian/13 vibechat-ci -c security.nesting=true -c limits.cpu=2 -c limits.memory=4GiB -d root,size=15GiB
for _ in $(seq 1 60); do incus exec vibechat-ci -- getent hosts deb.debian.org >/dev/null 2>&1 && break; sleep 2; done
incus file push "$deb" vibechat-ci/root/vibechat-server.deb
incus file push "$smoke" vibechat-ci/root/server-smoke.sh
incus exec vibechat-ci -- bash /root/server-smoke.sh /root/vibechat-server.deb ${3:-}
