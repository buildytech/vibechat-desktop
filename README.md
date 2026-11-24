# BuildY VibeChat desktop

The [BuildY Studio](https://github.com/buildytech/studio) as a desktop app on [Wails 3](https://wails.io)
(v3.1.0), for one user with their own Cursor API key.

This repository is **only the Go backend and the build**. Two public git submodules, pinned by commit:

- `third_party/studio` = [buildytech/studio](https://github.com/buildytech/studio): the Studio, Agent Contract v1 and
  the host seam (`packages/agent-contract/host-seam.json`, [ADR 0002](https://github.com/buildytech/studio/blob/main/docs/adr/0002-host-seam-contract.md));
- `third_party/templates` = [buildytech/templates](https://github.com/buildytech/templates): `vite-svelte-bun` and
  `vite-react-bun`.

Nothing in a submodule is edited here; a Studio or template change reaches the desktop app by moving the pin.

## Скачать / Download

Portable zips of the latest release (unzip and run; `bridge/` — the agent runtime — must stay next to the executable):

- **Linux x64**: [vibechat-desktop-linux-x64.zip](https://github.com/buildytech/vibechat-desktop/releases/latest/download/vibechat-desktop-linux-x64.zip)
  — GTK 4 + WebKitGTK 6 (`sudo apt install libgtk-4-1 libwebkitgtk-6.0-4`; Ubuntu 24.04 / Debian 13 or newer).
  Run `./vibechat-desktop-linux-x64/vibechat`.
- **Windows x64**: [vibechat-desktop-windows-x64.zip](https://github.com/buildytech/vibechat-desktop/releases/latest/download/vibechat-desktop-windows-x64.zip)
  — Windows 10/11 64-bit with Microsoft Edge WebView2 (preinstalled on Windows 11). Run `vibechat.exe`; the
  unsigned build may trigger SmartScreen: «More info» → «Run anyway».
- **macOS Apple Silicon (M1+)**: [vibechat-desktop-macos-arm64.zip](https://github.com/buildytech/vibechat-desktop/releases/latest/download/vibechat-desktop-macos-arm64.zip)
  — macOS 12+. The app is not notarized: after unzipping run
  `xattr -dr com.apple.quarantine "BuildY VibeChat.app"` (or right-click → Open).

Installers (`.deb`, NSIS `.exe`, `.dmg`) are on the [releases page](https://github.com/buildytech/vibechat-desktop/releases/latest).
Projects need [Bun](https://bun.sh) 1.3.3 or newer on PATH (`bun upgrade`); an older Bun gets a clear «update Bun»
message. A Cursor API key is needed for the agent: Settings in the app, or `vibechat --set-key`. The app log (bridge
output included, key redacted) is `%APPDATA%\tw.buildy.vibechat\logs\vibechat.log` on Windows,
`~/Library/Application Support/tw.buildy.vibechat/logs/vibechat.log` on macOS, `~/.config/tw.buildy.vibechat/logs/vibechat.log`
on Linux; `vibechat --bridge-smoke` checks that the agent runtime starts. `vibechat --version` prints the
version. Releases are built by CI from a `v*` tag.

## How it works

- The window loads the built Studio (`apps/studio` `build:desktop`), embedded in the binary (`frontend/dist`, made by
  `scripts/build-frontend.sh`).
- The Studio calls the host with `fetch("/runtime/v1/...")`. The Wails asset-server middleware answers those paths in
  Go, in process, on the app's own origin (`internal/host`). The route list is the pinned
  `packages/agent-contract/host-seam.json`; a route this build does not serve yet answers 501 with `{ error }`.
- The Cursor key is kept in the OS keychain (`github.com/zalando/go-keyring`). `CURSOR_API_KEY` in the environment is
  the development fallback. The key never reaches the webview.

### The agent: cursor-sdk-bridge in the SDK sandbox (fail closed)

- One [cursor-sdk-bridge](https://github.com/cursor/cursor-sdk-bridge) process per app (`internal/bridge`), found in
  `VIBECHAT_BRIDGE_DIR`, `<exe dir>/bridge`, `<exe dir>/../lib/vibechat/bridge` or `<app>/Contents/Resources/bridge`,
  with the platform helper `@cursor/sdk-<os>-<arch>` (`cursorsandbox`, `rg`) in `bin/node_modules/`. It listens on
  127.0.0.1 with a bearer token; the key reaches it only as `CURSOR_API_KEY` in its environment (never argv, a URL or a
  file). The catalog calls (`Me`, `ListModels`) need the key in their loopback request body; agent runs use the env.
- **The SDK sandbox is mandatory.** At start the host runs the helper's preflight; no bridge, no helper or a failed
  preflight turns `agent_sandbox` off and every prompt answers 503. Nothing runs unsandboxed.
- The bridge runs with a private `HOME` (`<data>/agent-home`) that holds the user-level `.cursor/sandbox.json`
  (`readBoundary: workspace`, `networkAccess: false`, plus the tool runtimes and `/proc` Bun needs) and
  `.cursor/hooks.json`: fail-closed `preToolUse` / `beforeReadFile` hooks that run this binary (`vibechat --hook`) and
  deny file tools outside the workspace, MCP, network commands, and writes to agent config. A project that carries
  its own `.cursor/{hooks,sandbox,cli,permissions,mcp}.json` or `.claude/settings*.json` is refused before a run.
- The agent's network is fully closed. Dependencies are installed by the host (`bun install`) outside the sandbox:
  when a project is created, when a chat's worktree is made, and after a run that changed `package.json`/`bun.lock`.
- Models: `auto`, Composer 2.5 (and fast), `grok-4.7` (context 256k/500k, effort, fast), Claude Opus 5. Runs stream
  Agent Contract v1 events over SSE (`/agent/sessions/:sid/events`); the transcript is `<data>/state/transcripts`.

### Projects, chats and worktrees

- Projects live in `~/VibeChat/<name>` (`VIBECHAT_PROJECTS_DIR`), state in `<UserConfigDir>/tw.buildy.vibechat`
  (`VIBECHAT_DATA_DIR`). Each project is a git repository (`main`) scaffolded from a template embedded from the
  templates submodule.
- Every chat works in its own git worktree, `<data>/worktrees/<project>/<chat>` on branch `vibe/<chat>`, outside the
  project folder. Before a run the worktree takes in `main`; after it the host commits the run and merges it into
  `main` (the project folder, where the preview runs). A conflict leaves the work on the branch and says so. Deleting
  a chat removes its worktree and branch.

### Live preview

- Play starts the project's dev server (`bun x vite --host 127.0.0.1 --port <port> --strictPort`) in its own process
  group (on Windows also a Job Object with kill-on-close, so the tree dies with the app), after the host-side
  `bun install`. Stop, deleting the project and quitting the app stop the whole group.
- The Studio iframe opens a separate loopback origin, `http://127.0.0.1:<proxy port>` (`internal/preview`), never
  the dev server directly. The host mints a one-time grant (`?__buildy_grant=…`, 60 s); the proxy also lets through
  requests with its session cookie or that the browser marks as coming from the preview itself (Sec-Fetch-Site
  same-origin / Origin / Referer), checks the Host header, and answers everything else 403.
- Vite HMR runs over the proxy port (`HMR_CLIENT_PORT`), WebSocket included.

## Build

Requirements: Go 1.26+, Bun 1.4 (the Studio build), and on Linux `pkg-config libgtk-4-dev libwebkitgtk-6.0-dev`.

```bash
git clone --recurse-submodules https://github.com/buildytech/vibechat-desktop
cd vibechat-desktop
bash scripts/build-frontend.sh               # bun install + build:desktop in third_party/studio → frontend/dist
go build -o bin/vibechat ./cmd/vibechat      # the desktop app
bin/vibechat --set-key                       # paste the Cursor key on stdin, kept in the OS keychain
bin/vibechat
```

Headless mode for tests and the exam (no window; loopback only, port `VIBECHAT_PORT`, default 18120):

```bash
go build -tags server -o bin/vibechat-server ./cmd/vibechat
bin/vibechat-server
curl -s http://127.0.0.1:18120/runtime/v1/host/capabilities
```

The server build has no auth: never expose it beyond loopback.

The agent needs the runtime next to the binary: `go run ./cmd/fetch-bridge -out bin/bridge` downloads the
cursor-sdk-bridge build and the SDK sandbox helper pinned in `bridge.lock.json` and verifies their SHA-256 / sha512.
On x64 (Linux, Windows, Intel macOS) the upstream bridge needs AVX2 (Bun's default runtime: `0xC000001D` /
SIGILL on older CPUs), so fetch-bridge takes the JavaScript bundle out of the pinned binary and compiles it again on
the pinned Bun **baseline** runtime of the same version (`baselineRuntime` in the lock; SSE4.2, no AVX). `go run
./cmd/fetch-bridge -verify bin/bridge` checks a bridge folder (manifest `runtime: bun-*-baseline`, machine code equal
to the pinned baseline Bun); `scripts/bridge-qemu-smoke.sh <bridge dir>` runs it under QEMU `-cpu Nehalem-v2` until
ready (CI does both). An early bridge exit is logged with its decoded exit code (e.g. `0xC000001D
STATUS_ILLEGAL_INSTRUCTION`) and the last output lines.
Without it (or without a working sandbox) the app runs but the agent stays off.

## Installers

`bash scripts/package.sh` (after `scripts/build-frontend.sh`) builds the unsigned installer of the current OS into
`dist/`, with the agent runtime inside, and leaves the bare executable with `bin/bridge/` next to it:

| OS | Installer | Runtime location |
|---|---|---|
| Linux | `vibechat_<version>_amd64.deb` | `/usr/lib/vibechat/bridge` |
| Windows | `vibechat-amd64-installer.exe` (NSIS) | `<install dir>\bridge` |
| macOS | `BuildY VibeChat.dmg` (ad-hoc signed `.app`) | `Contents/Resources/bridge` |

It also writes the portable zip of that OS: `vibechat-desktop-linux-x64.zip`, `vibechat-desktop-windows-x64.zip`,
`vibechat-desktop-macos-arm64.zip` (Apple Silicon only). CI uploads `vibechat-installer-<os>` (installer + zip) and
`vibechat-<os>` (the bare executable + `bridge/`); a pushed `v*` tag also publishes a GitHub Release with the three
zips and the installers under these stable names.

## Tests and the gate

`bash scripts/gate.sh` before every push to `main` (no `--force`, no `--no-verify`): gofmt, `go vet`, unit tests, the
host seam contract test against the pinned `host-seam.json`, and the headless build. CI (`.github/workflows/ci.yml`)
builds the Studio, runs the tests and builds the desktop app and unsigned installers on Linux, macOS and Windows.

## License

Apache-2.0, see `LICENSE`.
