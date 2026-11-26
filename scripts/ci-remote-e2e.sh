#!/usr/bin/env bash
# CI: the desktop app's server connection end to end against the Debian 13 VM of ci-server-debian.sh (vibechat-server
# installed and running there). In the VM: sshd and the tunnel user, made with the same lines the Studio shows in
# Settings (apps/studio/src/desktop-connection.ts tunnelSetupCommand); a fresh host token. On the runner: a fresh
# SSH key and `go test -tags e2e -run TestE2E ./desktop/internal/remote` through SSH to the VM. The token and the private key
# stay in 0600 temp files and are never printed.
# Usage: ci-remote-e2e.sh <incus vm>
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
vm="$1"
user=vibechat-tunnel
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
chmod 700 "$work"
in_vm() { sudo incus exec "$vm" -- "$@"; }
in_vm env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssh-server >/dev/null
in_vm systemctl enable --now ssh >/dev/null 2>&1 || in_vm systemctl start ssh
ssh-keygen -q -t ed25519 -N '' -C vibechat-desktop -f "$work/id"
pub="$(cat "$work/id.pub")"
home="/home/$user"
keys="$home/.ssh/authorized_keys"
line="restrict,port-forwarding,permitopen=\"127.0.0.1:18120\",permitopen=\"127.0.0.1:18121\" $pub"
in_vm bash -euo pipefail -c "
id $user >/dev/null 2>&1 || adduser --disabled-password --gecos \"\" --shell /usr/sbin/nologin $user
install -d -m 700 -o $user -g $user $home/.ssh
grep -qxF '$line' $keys 2>/dev/null || echo '$line' >> $keys
chown $user:$user $keys && chmod 600 $keys
"
# A fresh host token for this run (the service reads the hash at start).
umask 077
in_vm runuser -u vibechat -- env VIBECHAT_DATA_DIR=/var/lib/vibechat vibechat-server token new 2>/dev/null > "$work/token"
in_vm systemctl restart vibechat-server.service
for _ in $(seq 1 100); do in_vm curl -fsS -o /dev/null http://127.0.0.1:18120/runtime/v1/host/hello 2>/dev/null && break; sleep 0.2; done
ip="$(in_vm hostname -I | awk '{print $1}')"
fp="$(in_vm ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub | awk '{print $2}')"
echo "ci-remote-e2e: sshd at $ip:22, host key $fp"
# The tunnel user has no shell: a command over SSH must fail.
if ssh -i "$work/id" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes "$user@$ip" true 2>/dev/null; then
	echo "ci-remote-e2e: the tunnel user ran a command" >&2
	exit 1
fi
VIBECHAT_E2E_SSH="$ip:22" VIBECHAT_E2E_SSH_KEY="$work/id" VIBECHAT_E2E_TOKEN_FILE="$work/token" VIBECHAT_E2E_HOST_FP="$fp" \
	go test -tags e2e -count=1 -run TestE2E -v ./desktop/internal/remote
echo "ci-remote-e2e: ok"
