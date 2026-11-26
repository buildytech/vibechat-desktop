#!/bin/sh
# vibechat-server preremove: stop the units on removal (not on upgrade); /var/lib/vibechat stays (projects, key).
set -e
if [ "$1" = "remove" ] && [ -d /run/systemd/system ]; then
	systemctl disable --now vibechat-update.timer vibechat-server.service >/dev/null 2>&1 || true
fi
exit 0
