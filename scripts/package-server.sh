#!/usr/bin/env bash
# The VibeChat server package (Linux, CI ubuntu job), into dist/:
#   vibechat-server-linux-amd64.deb   stable name: vibechat-server update downloads releases/<tag>/<this name>
#   (SHA256SUMS of all release assets is written by the release job.)
# Layout: /usr/lib/vibechat-server/{vibechat-server, bridge/}, /usr/bin/vibechat-server → it, systemd units
# vibechat-server.service (user vibechat, loopback only) and vibechat-update.{service,timer} (daily self-update).
# Needs: Go, the wails3 CLI (its nfpm packs the .deb), server/frontend/dist (the server serves the Studio,
# scripts/build-frontend.sh); bin/bridge is fetched here when missing (server/cmd/fetch-bridge, server/bridge.lock.json).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
WAILS3="${WAILS3:-wails3}"
VERSION="${VIBECHAT_VERSION:-0.2.13}"
export VIBECHAT_VERSION="${VERSION#v}" GOARCH="${GOARCH:-amd64}"
[ "$(uname -s)" = Linux ] || { echo "package-server: Linux only" >&2; exit 1; }
[ -f server/frontend/dist/index.html ] || { echo "package-server: server/frontend/dist missing (scripts/build-frontend.sh)" >&2; exit 1; }
[ -f bin/bridge/manifest.json ] || go run ./server/cmd/fetch-bridge -lock server/bridge.lock.json -out bin/bridge -cache "${BRIDGE_CACHE:-.bridge-cache}"
mkdir -p bin/server dist
CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "-s -w -X github.com/buildytech/vibechat-desktop/internal/appid.Version=$VIBECHAT_VERSION" -o bin/server/vibechat-server ./server/cmd/vibechat-server
"$WAILS3" tool package -name vibechat-server -format deb -config build/linux/server/nfpm.yaml -out dist
mv dist/vibechat-server.deb "dist/vibechat-server-linux-$GOARCH.deb"
dpkg-deb --info "dist/vibechat-server-linux-$GOARCH.deb" | sed -n '1,20p'
dpkg-deb --contents "dist/vibechat-server-linux-$GOARCH.deb" | grep -E 'vibechat-server|systemd' 
