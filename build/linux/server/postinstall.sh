#!/bin/sh
# vibechat-server postinstall: the system user and its private data folder, then the units. The server itself starts
# once a host token exists (ConditionPathExists in the unit).
set -e
if ! getent passwd vibechat >/dev/null; then
	adduser --system --group --home /var/lib/vibechat --shell /usr/sbin/nologin vibechat
fi
mkdir -p /var/lib/vibechat
chown vibechat:vibechat /var/lib/vibechat
chmod 0700 /var/lib/vibechat
# The pinned Go toolchain for Go templ projects (/opt/vibechat/go; a no-op when it is current). It needs
# dl.google.com in the egress allowlist; without it the server runs and `vibechat-server doctor` says what is missing.
if [ -x /usr/bin/vibechat-server ]; then
	timeout 900 /usr/bin/vibechat-server ensure-go || echo "vibechat-server: Go for templ projects is not installed (run: vibechat-server ensure-go)" >&2
fi
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
	systemctl enable vibechat-server.service vibechat-update.timer >/dev/null 2>&1 || true
	systemctl start vibechat-update.timer >/dev/null 2>&1 || true
	# A new version (vibechat-update) replaces the binary under the running server: restart it; a first install
	# without a token stays stopped.
	if systemctl is-active --quiet vibechat-server.service; then
		systemctl restart vibechat-server.service || true
	else
		systemctl start vibechat-server.service >/dev/null 2>&1 || true
	fi
fi
exit 0
