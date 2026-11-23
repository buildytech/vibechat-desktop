#!/usr/bin/env bash
# Local gate before a push to main (no --force, no --no-verify): formatting, vet, the unit and host-seam contract tests,
# the submodule pin, and the headless (server) build. The GUI build and installers run in CI on all three systems.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
[ "$(git branch --show-current)" = "main" ] || { echo "gate: branch must be main" >&2; exit 1; }
git submodule update --init third_party/vibecode
[ -z "$(git -C third_party/vibecode status --porcelain --untracked-files=no)" ] || { echo "gate: third_party/vibecode has local changes" >&2; exit 1; }
unformatted="$(gofmt -l *.go cmd internal)"
[ -z "$unformatted" ] || { echo "gate: gofmt needed: $unformatted" >&2; exit 1; }
PKGS="./ ./internal/... ./cmd/..."
go vet $PKGS
go test -count=1 $PKGS
passed="$(go test -count=1 -run TestHostSeamContract -v ./internal/host | grep -c -- '--- \(PASS\|SKIP\): TestHostSeamContract/')"
echo "gate: host seam contract cases: $passed"
go build -tags server -o bin/vibechat-server ./cmd/vibechat
echo "gate: ok (vibecode $(git -C third_party/vibecode rev-parse --short HEAD))"
