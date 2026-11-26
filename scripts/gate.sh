#!/usr/bin/env bash
# Local gate before a push to main (no --force, no --no-verify): formatting, vet, the unit and host-seam contract tests,
# the submodule pins, the server (server/cmd/vibechat-server), the desktop app (desktop/cmd/vibechat, when this
# system has the GUI libraries) and that the app links no server or local-agent code (scripts/check-desktop.sh). The
# GUI builds and installers of all three systems run in CI.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
[ "$(git branch --show-current)" = "main" ] || { echo "gate: branch must be main" >&2; exit 1; }
git submodule update --init third_party/studio server/third_party/templates
for m in third_party/studio server/third_party/templates; do
	[ -z "$(git -C "$m" status --porcelain --untracked-files=no)" ] || { echo "gate: $m has local changes" >&2; exit 1; }
done
unformatted="$(gofmt -l *.go protocol server desktop internal)"
[ -z "$unformatted" ] || { echo "gate: gofmt needed: $unformatted" >&2; exit 1; }
go vet ./...
go test -count=1 ./...
passed="$(go test -count=1 -run TestHostSeamContract -v ./server/internal/host | grep -c -- '--- \(PASS\|SKIP\): TestHostSeamContract/')"
echo "gate: host seam contract cases: $passed"
go test -count=1 -run TestDesktopRoutesMatchTheContract ./desktop/internal/remote >/dev/null
go build -o bin/vibechat-server ./server/cmd/vibechat-server
scripts/check-desktop.sh
if pkg-config --exists gtk4 webkitgtk-6.0 2>/dev/null; then
	go build -o bin/vibechat ./desktop/cmd/vibechat
	scripts/check-desktop.sh bin/vibechat
fi
GOOS=windows go vet ./server/cmd/vibechat-server ./protocol/... ./internal/privfile ./server/internal/hostwire ./desktop/internal/...
echo "gate: ok (studio $(git -C third_party/studio rev-parse --short HEAD), templates $(git -C server/third_party/templates rev-parse --short HEAD))"
