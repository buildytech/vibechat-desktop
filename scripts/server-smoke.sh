#!/usr/bin/env bash
# Install smoke of the server .deb on a Debian 13 machine with systemd, run as root there (CI: an Incus VM, and an
# Incus container inside it like a real server). Checks: package and units, host token (sha256 only, 0600), the
# unit starts, hello is public, no auth → 401, Bearer → ticket → HttpOnly session cookie → Studio and the seam, the
# preview gateway answers 404 for an unknown project, then `vibechat-server doctor` (the SDK sandbox preflight).
#   server-smoke.sh <deb> [--require-sandbox]   with the flag a doctor FAIL fails the smoke
set -euo pipefail
deb="$(readlink -f "$1")"
require_sandbox="${2:-}"
. /etc/os-release
echo "server-smoke: $PRETTY_NAME, kernel $(uname -r), $(systemd-detect-virt 2>/dev/null || echo unknown virt)"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl jq >/dev/null
apt-get install -y -qq "$deb" >/dev/null
dpkg -s vibechat-server | grep -E '^(Version|Description):'
systemctl is-enabled vibechat-update.timer vibechat-server.service
as_vibechat() { runuser -u vibechat -- env VIBECHAT_DATA_DIR=/var/lib/vibechat "$@"; }
tok="$(mktemp)"
trap 'rm -f "$tok" /tmp/vibechat-jar /tmp/vibechat-hello.json' EXIT
as_vibechat vibechat-server token new 2>/dev/null > "$tok"
stat -c '%a %U %n' /var/lib/vibechat /var/lib/vibechat/auth/token.sha256
if grep -qF "$(cat "$tok")" /var/lib/vibechat/auth/token.sha256; then echo "server-smoke: the token is on disk" >&2; exit 1; fi
systemctl start vibechat-server.service
for _ in $(seq 1 100); do curl -fsS http://127.0.0.1:18120/runtime/v1/host/hello > /tmp/vibechat-hello.json 2>/dev/null && break; sleep 0.2; done
cat /tmp/vibechat-hello.json; echo
jq -e '.protocol == 1 and (.instanceId | length) >= 16 and .previewPort == 18121' /tmp/vibechat-hello.json >/dev/null
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
test "$(code http://127.0.0.1:18120/runtime/v1/projects)" = 401
test "$(code http://127.0.0.1:18120/)" = 401
test "$(code -H 'Host: attacker.example:18120' http://127.0.0.1:18120/runtime/v1/host/hello)" = 421
ticket="$(curl -fsS -X POST -H "Authorization: Bearer $(cat "$tok")" http://127.0.0.1:18120/runtime/v1/auth/ticket | jq -r .ticket)"
curl -fsS -c /tmp/vibechat-jar -o /dev/null "http://127.0.0.1:18120/auth/session?ticket=$ticket"
grep -q '#HttpOnly_127.0.0.1.*vibechat_session' /tmp/vibechat-jar
test "$(code "http://127.0.0.1:18120/auth/session?ticket=$ticket")" = 401
curl -fsS -b /tmp/vibechat-jar http://127.0.0.1:18120/runtime/v1/host/capabilities; echo
curl -fsS -b /tmp/vibechat-jar http://127.0.0.1:18120/ | grep -qi '<!doctype html'
test "$(code -b /tmp/vibechat-jar -X POST http://127.0.0.1:18120/runtime/v1/auth/ticket)" = 401
test "$(code -H 'Host: abc.preview.localhost:18121' http://127.0.0.1:18121/)" = 404
echo "server-smoke: auth flow ok"
echo "--- kernel: lsm=$(cat /sys/kernel/security/lsm 2>/dev/null || echo '?'), unprivileged_userns_clone=$(cat /proc/sys/kernel/unprivileged_userns_clone 2>/dev/null || echo n/a), apparmor_restrict_unprivileged_userns=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo n/a)"
set +e
as_vibechat vibechat-server doctor
doctor=$?
set -e
journalctl -u vibechat-server --no-pager | tail -5
if [ "$doctor" != 0 ]; then
	if [ "$require_sandbox" = --require-sandbox ]; then echo "server-smoke: doctor FAIL" >&2; exit 1; fi
	echo "server-smoke: doctor reported FAIL (not required here)"
fi
echo "server-smoke: ok"
