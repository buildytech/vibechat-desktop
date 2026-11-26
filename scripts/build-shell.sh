#!/usr/bin/env bash
# Builds the desktop app's start screen (apps/studio `build:shell`: connect a server / set up a new one) from the pinned
# submodule third_party/studio (Bun, its own lockfile and scripts) and copies it into desktop/shell/dist, which the
# desktop app embeds. Nothing is edited in the
# submodule.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
git submodule update --init third_party/studio
command -v bun >/dev/null || { echo "build-shell: bun is required (https://bun.sh)" >&2; exit 1; }
(
	cd third_party/studio
	bun install --frozen-lockfile
	bun run build:shell
)
[ -n "$(git -C third_party/studio status --porcelain --untracked-files=no)" ] && { echo "build-shell: the build changed tracked files in third_party/studio" >&2; git -C third_party/studio status --short >&2; exit 1; }
rm -rf desktop/shell/dist
mkdir -p desktop/shell/dist
cp -R third_party/studio/apps/studio/dist-shell/. desktop/shell/dist/
touch desktop/shell/dist/.gitkeep
echo "build-shell: desktop/shell/dist from studio $(git -C third_party/studio rev-parse --short HEAD)"
