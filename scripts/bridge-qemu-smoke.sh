#!/usr/bin/env bash
# The x64 agent runtime on a CPU without AVX/AVX2: run the shipped bridge folder under QEMU user mode emulating a
# Nehalem CPU (SSE4.2, no AVX) until the bridge prints its ready line; also the sandbox helper and ripgrep.
# Control: the upstream (non-baseline) bridge must die with SIGILL there, or the emulation proves nothing.
# Usage: scripts/bridge-qemu-smoke.sh <bridge dir> [upstream cursor-sdk-bridge-standalone-linux-x64.tar.gz]
set -euo pipefail
dir=$(cd "$1" && pwd)
upstream=${2:-}; [ -z "$upstream" ] || upstream=$(cd "$(dirname "$upstream")" && pwd)/$(basename "$upstream")
cpu=${QEMU_CPU_MODEL:-Nehalem-v2}
qemu=$(command -v qemu-x86_64 || command -v qemu-x86_64-static)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "qemu: $($qemu --version | head -1), cpu $cpu"
grep -q '"runtime": "bun-.*-baseline"' "$dir/manifest.json" || { echo "FAIL: manifest runtime is not baseline"; exit 1; }

# 1) the bridge reaches ready (placeholder key: ready needs no network)
mkdir -p "$tmp/home" "$tmp/state"
cd "$tmp/home"
HOME="$tmp/home" CURSOR_API_KEY=qemu-smoke-placeholder \
  "$qemu" -cpu "$cpu" "$dir/bin/cursor-sdk-bridge" --host 127.0.0.1 --port 0 --state-root "$tmp/state" \
  > "$tmp/out.txt" 2>&1 &
pid=$!
cd - >/dev/null
for _ in $(seq 1 120); do
  grep -q '^cursor-sdk-bridge ready ' "$tmp/out.txt" && break
  kill -0 "$pid" 2>/dev/null || break
  sleep 1
done
kill "$pid" 2>/dev/null || true
for _ in $(seq 1 10); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
kill -9 "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true
tok=$(sed -n 's/.*"authTokenFile":"\([^"]*\)".*/\1/p' "$tmp/out.txt" | head -1)
[ -n "$tok" ] && rm -rf "$(dirname "$tok")"
sed 's/^/  bridge: /' "$tmp/out.txt" | head -20
grep -q '^cursor-sdk-bridge ready ' "$tmp/out.txt" || { echo "FAIL: bridge not ready under -cpu $cpu"; exit 1; }
echo "ok: bridge ready under -cpu $cpu"

# 2) the SDK helper binaries start
helper=$(ls -d "$dir"/bin/node_modules/@cursor/sdk-linux-x64)
"$qemu" -cpu "$cpu" "$helper/bin/rg" --version | head -1
set +e
"$qemu" -cpu "$cpu" "$helper/bin/cursorsandbox" --version > "$tmp/sb.txt" 2>&1
code=$?
set -e
cat "$tmp/sb.txt"
[ "$code" -lt 128 ] || { echo "FAIL: cursorsandbox crashed ($code)"; exit 1; }
echo "ok: rg and cursorsandbox run under -cpu $cpu"

# 3) control: the upstream AVX2 build must not run there
if [ -n "$upstream" ]; then
  tar -xzf "$upstream" -C "$tmp" bin/cursor-sdk-bridge
  set +e
  HOME="$tmp/home" timeout 60 "$qemu" -cpu "$cpu" "$tmp/bin/cursor-sdk-bridge" --version > "$tmp/up.txt" 2>&1
  code=$?
  set -e
  head -3 "$tmp/up.txt"
  if [ "$code" -ne 132 ] && ! grep -q 'Illegal instruction' "$tmp/up.txt"; then
    echo "FAIL: the upstream bridge did not hit SIGILL (exit $code): the emulated CPU still has AVX2?"; exit 1
  fi
  echo "ok: control — upstream bridge dies with SIGILL under -cpu $cpu"
fi
