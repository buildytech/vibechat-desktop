#!/usr/bin/env bash
# Builds the Studio (apps/web `build:desktop`) from the pinned submodule third_party/vibecode (Bun, its own lockfile and
# scripts) and copies the static site into frontend/dist, which the Go binary embeds. Nothing is edited in the
# submodule.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
git submodule update --init third_party/vibecode
command -v bun >/dev/null || { echo "build-frontend: bun is required (https://bun.sh)" >&2; exit 1; }
(
	cd third_party/vibecode
	bun install --frozen-lockfile
	bun run build:desktop
)
[ -n "$(git -C third_party/vibecode status --porcelain --untracked-files=no)" ] && { echo "build-frontend: the build changed tracked files in third_party/vibecode" >&2; git -C third_party/vibecode status --short >&2; exit 1; }
rm -rf frontend/dist
mkdir -p frontend/dist
cp -R third_party/vibecode/apps/web/dist/. frontend/dist/
touch frontend/dist/.gitkeep
echo "build-frontend: frontend/dist from vibecode $(git -C third_party/vibecode rev-parse --short HEAD)"
