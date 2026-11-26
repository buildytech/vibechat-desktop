#!/usr/bin/env bash
# The desktop app is a window to the owner's vibechat-server: no local agent, no templates, no local projects.
#   scripts/check-desktop.sh            the Go packages the app links: only desktop/, protocol/ and the shared
#                                       internal/{appid,applog,privfile,webapp} (server/internal is not even importable
#                                       from desktop/: Go's internal rule)
#   scripts/check-desktop.sh <binary>   also the built binary: none of the agent runtime's or the server's markers
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
mod="$(go list -m)"
deps="$(go list -deps ./desktop/cmd/vibechat | grep "^$mod" || true)"
bad="$(grep -vE "^$mod(|/protocol(/auth)?|/desktop/.*|/internal/(appid|applog|privfile|webapp))$" <<<"$deps" || true)"
[ -z "$bad" ] || { echo "check-desktop: the desktop app links code it must not:" >&2; echo "$bad" >&2; exit 1; }
echo "check-desktop: $(wc -l <<<"$deps") module packages, none of server/"
if [ $# -gt 0 ]; then
	bin="$1"
	# Markers of the local agent (bridge, SDK sandbox, permission hook), the templates and the local host.
	for m in cursor-sdk-bridge cursorsandbox VIBECHAT_BRIDGE_DIR VIBECHAT_HOOK_EXE buildy-starter-svelte VIBECHAT_PROJECTS_DIR server/internal/; do
		if grep -aqF -- "$m" "$bin"; then echo "check-desktop: $bin contains \"$m\"" >&2; exit 1; fi
	done
	echo "check-desktop: $bin has no local agent, templates or server code"
fi
